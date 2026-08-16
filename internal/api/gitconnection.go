package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// GitConnectionService served: the forge credentials kelson holds, over the
// wire (ADR-0033, issue #248).
//
// # No response built here can carry a credential value
//
// gitconnection.proto states that as a schema property — there is no field a
// token, a private key or a webhook secret could arrive in or leave through —
// and this file keeps it as a structural one. The connection store reaches the
// handlers as *two* interfaces: [GitConnectionStore], which lists, reads,
// creates, deletes and records observations, and [ConnectionSecretReader],
// which is the single method that opens a Secret. List and Get hold only the
// first, so the read-secret method is not reachable from them — not "not
// called", not reachable. Only TestConnection holds both, it uses the material
// to make an outbound call, and what it puts on the wire is the same
// reachable/account/count triple the status subresource carries.
//
// # An app connection is reported and never accepted
//
// CreateConnection builds a token-auth connection and nothing else. The GitHub
// App path is the manifest flow's HTTP callback (ADR-0033 decision 2): the
// credential is minted by GitHub and handed to the *server*, so an RPC that
// took an app id and a private key would be exactly the credential-carrying
// request the paragraph above says does not exist.

// connectionProbeTimeout bounds one TestConnection.
//
// It is the whole probe rather than one request: minting an installation token
// is a call, listing repositories is a paginated read, and a caller pressing
// "test" wants an answer or a reason in a bounded time — not a handler parked
// on a forge that accepted the connection and stopped talking. A probe that
// runs out reports unreachable with the timeout named, which is a true and
// useful answer, so the budget is deliberately shorter than the forge client's
// own 30s-per-request ceiling would allow a multi-page listing to take.
const connectionProbeTimeout = 25 * time.Second

// ListConnections reports every connection the instance holds.
func (s *Server) ListConnections(ctx context.Context, _ *connect.Request[kelsonv1alpha1.ListConnectionsRequest]) (*connect.Response[kelsonv1alpha1.ListConnectionsResponse], error) {
	if s.connections == nil {
		return nil, unimplemented("the git connection store")
	}
	stored, err := s.connections.List(ctx)
	if err != nil {
		return nil, failRequest(err)
	}
	out := make([]*kelsonv1alpha1.GitConnection, 0, len(stored))
	for _, conn := range stored {
		out = append(out, wireConnection(conn))
	}
	return connect.NewResponse(&kelsonv1alpha1.ListConnectionsResponse{Connections: out}), nil
}

// GetConnection reports one connection by name.
func (s *Server) GetConnection(ctx context.Context, req *connect.Request[kelsonv1alpha1.GetConnectionRequest]) (*connect.Response[kelsonv1alpha1.GetConnectionResponse], error) {
	if s.connections == nil {
		return nil, unimplemented("the git connection store")
	}
	conn, err := s.connections.Get(ctx, req.Msg.GetName())
	if err != nil {
		return nil, failRequest(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.GetConnectionResponse{Connection: wireConnection(conn)}), nil
}

// CreateConnection creates a token-auth connection.
//
// Validation runs before anything is written, through model.ValidateGitConnection
// — the same function the CLI and the controller run, so a document this refuses
// is refused identically wherever it is presented (ADR-0027 decision 5). Unlike
// PutSpec the findings cannot travel inline: CreateConnectionResponse has no
// `errors` field, so an invalid request is an InvalidArgument carrying the same
// structured errors as details.
//
// A connection naming a Secret that does not exist is *created*, deliberately:
// the proto says so, the two objects have separate lifecycles, and either order
// must work. It reports Ready false with the reason on the first probe.
func (s *Server) CreateConnection(ctx context.Context, req *connect.Request[kelsonv1alpha1.CreateConnectionRequest]) (*connect.Response[kelsonv1alpha1.CreateConnectionResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	doc, err := connectionDocument(msg)
	if err != nil {
		return nil, failRequest(err)
	}
	if errs := model.ValidateGitConnection(doc); len(errs) > 0 {
		return nil, failRequest(errs)
	}

	// RENDER validates and touches nothing, which for a create is the whole of
	// what it can do: there is no render step, and the connection it would
	// produce is a projection of the request. SERVER goes to the API server as
	// a real dry-run apply, so admission has its say.
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return connect.NewResponse(&kelsonv1alpha1.CreateConnectionResponse{
			Connection: wireConnection(controlstore.StoredConnection{Name: doc.Metadata.Name, Spec: doc.Spec}),
			DryRun:     true,
		}), nil
	}
	if s.connections == nil {
		return nil, unimplemented("the git connection store")
	}

	created, err := s.connections.Create(ctx, doc.Metadata.Name, doc.Spec, controlstore.CreateConnectionOptions{
		IdempotencyKey: msg.GetIdempotencyKey(),
		DryRun:         msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	})
	if err != nil {
		return nil, failRequest(err)
	}
	if persists(msg.GetDryRun()) {
		auditChange(ctx, controlstore.AuditChange{Resources: 1, Kinds: []string{model.KindGitConnection}})
	}
	return connect.NewResponse(&kelsonv1alpha1.CreateConnectionResponse{
		Connection: wireConnection(created),
		DryRun:     !persists(msg.GetDryRun()),
	}), nil
}

// DeleteConnection removes a connection and names the projects it was serving.
//
// The delete is not blocked by them. A connection is deleted because it is
// wrong — a leaked token, a revoked app — and refusing until every project has
// been edited would make revoking a credential harder than keeping it. So the
// builds that will start failing are named instead of discovered later.
//
// `affected_projects` is computed on *both* dry-run rungs. Resolving it is a
// read of the stored specs, and the dry-run ladder gates what a call writes,
// not what it reads: a dry run that withheld the one field it exists to preview
// would be a rehearsal of nothing.
func (s *Server) DeleteConnection(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeleteConnectionRequest]) (*connect.Response[kelsonv1alpha1.DeleteConnectionResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	if s.connections == nil {
		return nil, unimplemented("the git connection store")
	}
	conn, err := s.connections.Get(ctx, msg.GetName())
	if err != nil {
		return nil, failRequest(err)
	}
	affected, err := s.projectsThrough(ctx, conn.Name)
	if err != nil {
		return nil, failRequest(err)
	}

	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return connect.NewResponse(&kelsonv1alpha1.DeleteConnectionResponse{AffectedProjects: affected}), nil
	}
	if err := s.connections.Delete(ctx, conn.Name, controlstore.DeleteConnectionOptions{
		IdempotencyKey: msg.GetIdempotencyKey(),
		DryRun:         msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}); err != nil {
		return nil, failRequest(err)
	}
	if persists(msg.GetDryRun()) {
		auditChange(ctx, controlstore.AuditChange{Resources: 1, Kinds: []string{model.KindGitConnection}})
	}
	return connect.NewResponse(&kelsonv1alpha1.DeleteConnectionResponse{
		Deleted:          persists(msg.GetDryRun()),
		AffectedProjects: affected,
	}), nil
}

// TestConnection probes the forge, live, and records what came back.
//
// The probe is two questions and they fail differently, which is why the
// message is assembled per cause rather than out of one string: minting the
// credential answers "does this authenticate", and the repository listing
// answers "what can it see". A connection whose credential works and whose
// listing failed is reachable — the deploy path only needs the first — and
// saying otherwise would send an operator to rotate a credential that is fine.
//
// The outcome is written to the CR's status on the way out, so
// `kubectl get gitconnections` and this RPC agree about a connection somebody
// just tested. A status write that fails does not fail the probe: the answer
// was obtained, and losing it because the record could not be kept would be the
// wrong half to discard.
func (s *Server) TestConnection(ctx context.Context, req *connect.Request[kelsonv1alpha1.TestConnectionRequest]) (*connect.Response[kelsonv1alpha1.TestConnectionResponse], error) {
	if s.connections == nil {
		return nil, unimplemented("the git connection store")
	}
	conn, err := s.connections.Get(ctx, req.Msg.GetName())
	if err != nil {
		return nil, failRequest(err)
	}
	if s.connectionSecrets == nil {
		return nil, unimplemented("reading the Secret a connection references")
	}

	probeCtx, cancel := context.WithTimeout(ctx, connectionProbeTimeout)
	defer cancel()
	obs := s.probe(probeCtx, conn)

	// The status write runs on the request's context, not the probe's: a probe
	// that spent its whole budget would otherwise have nothing left to record
	// its own result with, and the result is the part worth keeping.
	message := obs.Message
	if _, err := s.connections.UpdateStatus(ctx, conn.Name, obs); err != nil {
		// Said out loud rather than swallowed. The caller asked a question and
		// got an answer, so the RPC succeeds — but `kubectl get gitconnections`
		// is about to disagree with what they were just told, and a client that
		// was not warned would file that as a second bug.
		message += " (this result could not be recorded on the connection: " + redact.Scrub(err.Error()) + ")"
	}
	return connect.NewResponse(&kelsonv1alpha1.TestConnectionResponse{
		Reachable:    obs.Reachable,
		Account:      obs.Account,
		Repositories: obs.Repositories,
		Message:      message,
	}), nil
}

// probe is the live half of TestConnection: resolve the adapter, read the
// Secret, mint, browse. It returns an observation rather than an error because
// every failure here is an *answer* — "this connection does not work, and here
// is which part of it" — and not a failure of the server to respond.
func (s *Server) probe(ctx context.Context, conn controlstore.StoredConnection) controlstore.ConnectionObservation {
	provider, ok := s.forge(string(conn.Spec.Provider))
	if !ok {
		return controlstore.ConnectionObservation{
			Message: fmt.Sprintf("no adapter serves provider %q, so this connection cannot be probed. "+
				"kelson speaks %s; every other forge connects as `generic` with a token",
				conn.Spec.Provider, providerNames()),
			ReadyReason:     controlstore.ReasonProviderUnknown,
			ReachableReason: controlstore.ReasonProviderUnknown,
		}
	}

	material, err := s.connectionSecrets.ReadAuthSecret(ctx, conn)
	if err != nil {
		return controlstore.ConnectionObservation{
			Message:         redact.Scrub(secretCause(err)),
			ReadyReason:     controlstore.ReasonSecretMissing,
			ReachableReason: controlstore.ReasonSecretMissing,
		}
	}
	// Ready is settled from here on: the document validated to be stored and
	// the Secret holds the key this auth kind reads. Whatever the forge says
	// next is Reachable's business.
	c := forgeConn(conn, material)

	// The connection's own host stands in for a repository URL. Nothing in the
	// seam scopes a credential to one — an installation token already carries
	// the repositories the user chose — and a probe has no repository in hand,
	// so the host is what makes the adapter's error name something the operator
	// recognises.
	cred, err := provider.MintCloneCredential(ctx, c, c.Host)
	if err != nil {
		return controlstore.ConnectionObservation{
			Ready:           true,
			Message:         redact.Scrub(mintCause(conn, err)),
			ReachableReason: controlstore.ReasonUnreachable,
		}
	}
	// Belt and braces over the adapter's own registration: a credential this
	// process has learned is unprintable from the moment it exists (#117), and
	// the token-auth path returns a stored value the adapter never minted.
	redact.Register(cred.SecretValues()...)

	browser, ok := provider.(forge.RepoBrowser)
	if !ok {
		return controlstore.ConnectionObservation{
			Ready:     true,
			Reachable: true,
			Message: fmt.Sprintf("the credential is present and well-formed, and provider %q has no repository "+
				"browser — so there is no account or repository count to report. That is a capability this "+
				"forge does not offer, not a fault", conn.Spec.Provider),
			ReachableReason: controlstore.ReasonReachable,
		}
	}

	repos, err := browser.ListRepositories(ctx, c)
	if err != nil {
		return controlstore.ConnectionObservation{
			Ready:           true,
			Reachable:       reachableDespite(err),
			Message:         redact.Scrub(listCause(conn, err)),
			ReachableReason: reachableReason(err),
		}
	}
	account, spread := accountOf(repos)
	return controlstore.ConnectionObservation{
		Ready:           true,
		Reachable:       true,
		Account:         account,
		Repositories:    int32(len(repos)), //nolint:gosec // a repository count does not overflow int32
		Message:         probeSummary(conn, account, spread, len(repos)),
		ReachableReason: controlstore.ReasonReachable,
	}
}

// Repository browsing, capability-gated (ADR-0033 decision 3, issue #248).
//
// # Why these two RPCs can exist now and could not before
//
// gitconnection.proto's first cut left repository listing off the wire on the
// grounds that "an RPC every connection answered would have to lie for the ones
// that cannot". The objection was about the answer: an empty list is what a
// GitHub App installation with nothing selected looks like *and* what a
// `generic` token connection with no browser at all would have to look like,
// and no client can tell those apart. These two calls do not answer that way.
// A connection whose provider implements [forge.RepoBrowser] is served, and one
// whose provider does not is *refused*, with a code in the api plane's own
// vocabulary — so "nothing here" and "this forge cannot be asked" are different
// answers rather than the same empty slice.
//
// # The refusal comes before the Secret is opened
//
// [Server.browse] resolves the connection, resolves the adapter and checks the
// capability, and only then reaches for [ConnectionSecretReader]. A connection
// that cannot be browsed therefore never has its Secret read to find that out,
// which keeps this file's structural rule intact for the path that most looks
// like it needs bending: the one place a credential would be read for a request
// that was always going to be refused.

// Connection error codes. They are the api plane's own vocabulary, like
// `auth/…` and `report/…`, and they ride the wire shape every other plane's
// errors do (errors.go) — so an agent branches on
// `connection/capability-unsupported` exactly where it branches on
// `store/not-found`.
const (
	// ErrConnectionCapabilityUnsupported is a connection whose provider does
	// not implement the optional capability the RPC needs. It is a statement
	// about the *forge*, never about the connection's health: the connection
	// works, and what it works for is in the message.
	ErrConnectionCapabilityUnsupported = "connection/capability-unsupported"
	// ErrConnectionProviderUnknown is a connection declaring a forge no adapter
	// in this build speaks. Distinct from the one above because the remedies
	// have nothing in common: a missing capability is permanent for that forge
	// until somebody writes it, and an unknown provider is a value that should
	// not have been stored (or a build older than the connection).
	ErrConnectionProviderUnknown = "connection/provider-unknown"
)

const connectionDocsBase = "https://kelson.dev/server/errors"

// connectionError is one refusal about a connection, in the shape errors.go
// projects onto the wire without a second taxonomy.
type connectionError struct {
	Code        string
	Resource    string
	Message     string
	Remediation string
}

func (e connectionError) Error() string {
	return fmt.Sprintf("%s [%s] %s: %s", e.Resource, e.Code, e.Message, e.Remediation)
}

func (e connectionError) wire() *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Code,
		Resource:    e.Resource,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     connectionDocsBase + "/" + e.Code,
	}
}

// unbrowsable is what both refusals become: Unimplemented, and never
// InvalidArgument.
//
// It is the distinction failRequest already draws for a gated delivery
// capability, for the same reason (ADR-0028, issue #224). An agent that reads
// InvalidArgument rewrites its request and tries again forever; there is no
// request that makes a `generic` connection grow a repository browser, and
// Unimplemented is the code that says so. A browser client reads it the same
// way: fall back to the field that always works.
func unbrowsable(e connectionError) error { return fail(connect.CodeUnimplemented, e) }

// connectionBrowseTimeout bounds one repository or branch listing.
//
// It is the probe's budget for the probe's reason: both listings paginate, an
// installation on a large organisation is several pages, and a picker waiting
// on a forge that accepted the connection and stopped talking wants a refusal
// in bounded time rather than a spinner. The listing is the more forgivable of
// the two to lose — a user who cannot browse can still paste a URL — so it does
// not get a longer one.
const connectionBrowseTimeout = connectionProbeTimeout

// ListConnectionRepositories reports what one connection's credential can see.
func (s *Server) ListConnectionRepositories(ctx context.Context, req *connect.Request[kelsonv1alpha1.ListConnectionRepositoriesRequest]) (*connect.Response[kelsonv1alpha1.ListConnectionRepositoriesResponse], error) {
	browse, err := s.browse(ctx, req.Msg.GetConnection())
	if err != nil {
		return nil, err
	}

	listCtx, cancel := context.WithTimeout(ctx, connectionBrowseTimeout)
	defer cancel()
	repos, err := browse.browser.ListRepositories(listCtx, browse.conn)
	if err != nil {
		return nil, failRequest(browseFailed(browse.stored, "list its repositories", err))
	}

	out := make([]*kelsonv1alpha1.GitRepository, 0, len(repos))
	for _, r := range repos {
		out = append(out, &kelsonv1alpha1.GitRepository{
			FullName:      r.FullName,
			HtmlUrl:       r.HTMLURL,
			DefaultBranch: r.DefaultBranch,
			Private:       r.Private,
		})
	}
	return connect.NewResponse(&kelsonv1alpha1.ListConnectionRepositoriesResponse{Repositories: out}), nil
}

// ListConnectionBranches reports one repository's branches, for the picker's
// second step.
func (s *Server) ListConnectionBranches(ctx context.Context, req *connect.Request[kelsonv1alpha1.ListConnectionBranchesRequest]) (*connect.Response[kelsonv1alpha1.ListConnectionBranchesResponse], error) {
	repository := strings.TrimSpace(req.Msg.GetRepository())
	if repository == "" {
		return nil, failRequest(fmt.Errorf("api: ListConnectionBranches needs a repository: the \"owner/name\" " +
			"ListConnectionRepositories reported. Branches are a property of one repository, and a connection " +
			"routinely sees many"))
	}
	browse, err := s.browse(ctx, req.Msg.GetConnection())
	if err != nil {
		return nil, err
	}

	listCtx, cancel := context.WithTimeout(ctx, connectionBrowseTimeout)
	defer cancel()
	branches, err := browse.browser.ListBranches(listCtx, browse.conn, repository)
	if err != nil {
		return nil, failRequest(browseFailed(browse.stored, "list the branches of "+repository, err))
	}
	return connect.NewResponse(&kelsonv1alpha1.ListConnectionBranchesResponse{Branches: branches}), nil
}

// connectionBrowse is a connection resolved far enough to browse it: the stored
// object for diagnostics, the adapter's browsing half, and the material joined
// onto it.
type connectionBrowse struct {
	stored  controlstore.StoredConnection
	browser forge.RepoBrowser
	conn    forge.Conn
}

// browse is the join both listings need, and the gate both are subject to.
//
// The order of its steps is the interesting part and is not an accident. The
// capability is checked *before* the Secret is read, so a refusal costs no
// credential read at all — see this section's header. Everything it returns is
// already a ConnectRPC error, because each step fails in its own vocabulary
// (the store's not-found, the connection family's two codes, an unwired seam)
// and flattening them into one would lose the code an agent branches on.
func (s *Server) browse(ctx context.Context, name string) (connectionBrowse, error) {
	if s.connections == nil {
		return connectionBrowse{}, unimplemented("the git connection store")
	}
	conn, err := s.connections.Get(ctx, name)
	if err != nil {
		return connectionBrowse{}, failRequest(err)
	}

	provider, ok := s.forge(string(conn.Spec.Provider))
	if !ok {
		return connectionBrowse{}, unbrowsable(connectionError{
			Code:     ErrConnectionProviderUnknown,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q declares provider %q, which no adapter in this build of kelson "+
				"speaks, so nothing here can browse it", conn.Name, conn.Spec.Provider),
			Remediation: fmt.Sprintf("kelson speaks %s; every other forge connects as `generic` with a token. "+
				"Recreate the connection with one of them, or paste the repository's URL — that path needs no "+
				"adapter at all", providerNames()),
		})
	}
	browser, ok := provider.(forge.RepoBrowser)
	if !ok {
		return connectionBrowse{}, unbrowsable(connectionError{
			Code:     ErrConnectionCapabilityUnsupported,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q speaks %q, and that adapter implements no repository browser: "+
				"there is no list of repositories or branches for it to return. What it can do is %s",
				conn.Name, conn.Spec.Provider, capabilitiesOf(provider)),
			Remediation: fmt.Sprintf("paste the repository's URL instead — that path works for every connection, "+
				"every forge and every auth kind, and %q still authenticates the clone that follows. Browsing is "+
				"an optional capability — its absence degrades the UI, never the deploy — so this "+
				"connection is not broken and there is nothing on it to fix; a forge gains a picker when somebody "+
				"writes that half of its adapter", conn.Name),
		})
	}

	if s.connectionSecrets == nil {
		return connectionBrowse{}, unimplemented("reading the Secret a connection references")
	}
	material, err := s.connectionSecrets.ReadAuthSecret(ctx, conn)
	if err != nil {
		return connectionBrowse{}, failRequest(err)
	}
	return connectionBrowse{stored: conn, browser: browser, conn: forgeConn(conn, material)}, nil
}

// capabilitiesOf names what an adapter actually implements, so a refusal can
// say what the connection is good for rather than only what it is not.
//
// It asks the adapter rather than consulting a table of providers, because a
// table here would be a second answer to a question internal/forge already
// answers by declaration — and it would be wrong the day an adapter grows a
// capability, in the one message a user reads when something is already not
// working.
func capabilitiesOf(p forge.Provider) string {
	can := []string{"mint short-lived credentials and clone private repositories, which is what a deploy needs"}
	if _, ok := p.(forge.RepoBrowser); ok {
		can = append(can, "browse repositories and branches")
	}
	if _, ok := p.(forge.WebhookSource); ok {
		can = append(can, "verify webhook deliveries")
	}
	if _, ok := p.(forge.StatusReporter); ok {
		can = append(can, "report commit statuses and upsert one pull-request comment")
	}
	if _, ok := p.(forge.PRProposer); ok {
		can = append(can, "open a pull request proposing a change")
	}
	return strings.Join(can, "; ")
}

// browseFailed wraps a listing the forge would not complete.
//
// It is [unavailable] and not an invalid argument: the request was well-formed
// and the credential was accepted far enough to be spent, so what failed is a
// dependency of this server rather than anything the caller could rewrite. The
// remediation points at TestConnection because that is the call that says which
// half is wrong — and at the URL field, because a picker that cannot list is a
// picker the user can step around.
func browseFailed(conn controlstore.StoredConnection, what string, err error) error {
	return unavailable("connection %q could not %s: %w. Test the connection to see whether the credential is "+
		"still accepted at %s, or paste the repository's URL — the pasted path needs no listing",
		conn.Name, what, err, conn.Spec.EffectiveHost())
}

func connectionResource(name string) string { return model.KindGitConnection + "/" + name }

// forgeConn joins a stored connection with the material its Secret held, which
// is the value every call into internal/forge takes.
//
// It is one function because three handlers need the same join and a second
// spelling of it is how a credential ends up in the wrong field: the app
// identifiers come from the spec and the key material from the Secret, and only
// the pair is a usable connection. [forgeconn.Resolver.join] does the same join
// for the planes below the API, against the same two halves — this is the
// api-plane copy for the one seam that reads its store directly rather than
// through the resolver.
func forgeConn(conn controlstore.StoredConnection, material controlstore.AuthMaterial) forge.Conn {
	c := forge.Conn{
		Provider:      string(conn.Spec.Provider),
		Host:          conn.Spec.EffectiveHost(),
		Token:         material.Token,
		Username:      material.Username,
		PrivateKeyPEM: material.PrivateKeyPEM,
		WebhookSecret: material.WebhookSecret,
	}
	if app := conn.Spec.Auth.GitHubApp; app != nil {
		c.AppID, c.InstallationID = app.AppID, app.InstallationID
	}
	return c
}

// forge resolves the adapter for a provider, through the injected lookup when
// this server has one. The default is internal/forge's own registry; the seam
// exists so a handler test can drive every branch of [Server.probe] without an
// httptest forge, which is the same shape every other cluster-facing capability
// in this package enters through.
func (s *Server) forge(provider string) (forge.Provider, bool) {
	if s.forges != nil {
		return s.forges(provider)
	}
	return forge.For(provider)
}

// projectsThrough names the stored projects that reach a repository through one
// connection, using the shared matcher (controlstore.MatchConnection) so this
// answer and the build plane's choice of credential cannot disagree.
//
// # Every source a project reaches, not the first one it declares
//
// A project's repositories are `spec.source` *and* `spec.sources` (ADR-0035
// decision 1) — one list written at two ceremonies — plus the GitSources its
// components bind to by name (decision 3). Reading the singular spelling alone
// made a plural-spelling project invisible here, so revoking a credential
// reported "no projects affected" while every build in that project was about
// to start failing. That is the one sentence this field exists to prevent.
//
// The global tier is included on a narrower rule than the project's own, and
// deliberately: a project's declared sources count as *declared*, because
// declaring one is the project saying it uses that repository, while an
// instance's GitSources are offered to everyone — counting them for every
// project would name the whole instance on every delete. So a GitSource counts
// for a project when one of its components is *bound* to it, and a project-local
// name shadows the global one it hides, exactly as resolution reads them.
//
// A server with no spec store holds no projects, so the answer is empty rather
// than an error: the delete is still the right thing to do and there is nothing
// to warn about. A listing that *failed* is not that case and is returned —
// answering "nothing is affected" out of a failed read is the same lie the
// build path refuses to tell (see [Server.globalSources]).
func (s *Server) projectsThrough(ctx context.Context, connection string) ([]string, error) {
	if s.specs == nil {
		return nil, nil
	}
	stored, err := s.specs.List(ctx)
	if err != nil {
		return nil, err
	}
	conns, err := s.connections.List(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]controlstore.ConnectionRef, 0, len(conns))
	for _, c := range conns {
		refs = append(refs, c.Ref())
	}
	globals, err := s.globalSources(ctx)
	if err != nil {
		return nil, err
	}

	sources := make([]controlstore.ProjectSource, 0, len(stored))
	for _, st := range stored {
		project, ok := decodeProjectDocument(st.Documents.Project)
		if !ok {
			// A project whose document no longer decodes is not evidence about
			// this connection either way, and refusing the delete over it would
			// make one broken spec un-revoke every credential.
			continue
		}
		for _, src := range reachedSources(project, globals) {
			sources = append(sources, controlstore.ProjectSource{
				Project:    st.Project,
				Git:        src.Git,
				Connection: src.Connection,
			})
		}
	}
	return controlstore.AffectedProjects(connection, sources, refs), nil
}

// reachedSources is every repository one project reaches: the sources it
// declares in either spelling, and the GitSources its components bind to.
//
// It answers in [model.Source]s where [projectRepositories] answers in URLs,
// because this caller needs the `connection:` beside the URL — an explicitly
// named connection is how a project reaches one whose host match would have
// picked another.
//
// [model.ProjectSpec.EffectiveSources] is what expands the singular spelling,
// rather than a second reading of the same two fields here — the shorthand is
// shorthand in exactly one place, so this scan and the resolver cannot come to
// different conclusions about what a project declared.
//
// The shadowing rule is the resolver's too (ADR-0035 decision 3): a component
// bound to `tools` uses the project's `tools` when it has one, so the global of
// that name is not a repository this project reaches at all.
func reachedSources(p *model.Project, globals []model.Source) []model.Source {
	local := p.Spec.EffectiveSources()
	out := make([]model.Source, 0, len(local))
	out = append(out, local...)
	if len(globals) == 0 {
		return out
	}

	declared := make(map[string]bool, len(local))
	for _, src := range local {
		declared[src.Name] = true
	}
	bound := make(map[string]bool, len(p.Spec.Components))
	for _, c := range p.Spec.Components {
		// A helm component's `source:` is a chart source and names no
		// repository kelson clones, which is what SourceName reports as "".
		if name := c.SourceName(); name != "" && !declared[name] {
			bound[name] = true
		}
	}
	for _, g := range globals {
		if bound[g.Name] {
			out = append(out, g)
		}
	}
	return out
}

// decodeProjectDocument reads the Project out of a stored document.
//
// It is not decodeSpec: that one demands at least one Environment, and both
// callers here — the connection scan and the report's component check — want
// `spec.project` and nothing else.
//
// Validation findings are deliberately ignored while the *parse* is not. A
// stored document that no longer validates is a real state (validation moves;
// a spec stored under an older rule set stays where it was), and refusing to
// read its source would mean one such project made every connection
// un-deletable without warning. Whether a stored spec is valid is PutSpec's
// question and the Ready condition's, not this scan's.
func decodeProjectDocument(doc []byte) (*model.Project, bool) {
	if len(doc) == 0 {
		return nil, false
	}
	parsed, _ := model.DecodeDocuments(doc)
	for _, d := range parsed {
		if p, ok := d.(*model.Project); ok {
			return p, true
		}
	}
	return nil, false
}

// connectionDocument turns the request into the document the model validates.
//
// Every connection this RPC makes is token auth, and the request has no field
// that could say otherwise: `secret_ref` names a Secret holding `token` (and
// optionally `username`). GIT_AUTH_KIND_GITHUB_APP is a value this service
// reports and never accepts (the file header, ADR-0033 decision 2).
func connectionDocument(msg *kelsonv1alpha1.CreateConnectionRequest) (*model.GitConnection, error) {
	name := strings.TrimSpace(msg.GetName())
	if name == "" {
		return nil, fmt.Errorf("api: CreateConnection needs a name: it is the connection's metadata.name and " +
			"what a Project's spec.source.connection refers to")
	}
	if strings.TrimSpace(msg.GetSecretRef()) == "" {
		return nil, fmt.Errorf("api: CreateConnection needs secret_ref: the name of an existing Secret holding "+
			"the token under key %q. This request carries the name and never the token", model.TokenKey)
	}
	owner, err := modelOwner(msg.GetOwner())
	if err != nil {
		return nil, err
	}
	return &model.GitConnection{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindGitConnection},
		Metadata: model.ObjectMeta{Name: name},
		Spec: model.GitConnectionSpec{
			Provider: model.GitProvider(strings.TrimSpace(msg.GetProvider())),
			Host:     strings.TrimSpace(msg.GetHost()),
			Auth:     model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: strings.TrimSpace(msg.GetSecretRef())}},
			Owner:    owner,
		},
	}, nil
}

// modelOwner reads the ownership reference. An unset owner is the instance,
// which is the day-one shape and the only kind enforced today (ADR-0033
// decision 6) — and it is stored as nil rather than as an explicit `instance`,
// because model.GitConnectionSpec already reads nil that way and writing it out
// would put a field in every document that says what the default says.
func modelOwner(owner *kelsonv1alpha1.GitOwner) (*model.ConnectionOwner, error) {
	switch owner.GetKind() {
	case kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_UNSPECIFIED, kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_INSTANCE:
		if name := owner.GetName(); name != "" {
			return nil, fmt.Errorf("api: an instance-owned connection names no principal, and this one names %q: "+
				"set owner.kind to user or team, or leave the name empty", name)
		}
		return nil, nil
	case kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_USER:
		return &model.ConnectionOwner{Kind: model.OwnerUser, Name: owner.GetName()}, nil
	case kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_TEAM:
		return &model.ConnectionOwner{Kind: model.OwnerTeam, Name: owner.GetName()}, nil
	default:
		return nil, fmt.Errorf("api: unknown owner kind %v", owner.GetKind())
	}
}

// wireConnection projects a stored connection onto the wire.
//
// Read the fields it does *not* set as carefully as the ones it does: there is
// no branch here that could reach a Secret's contents, because
// kelsonv1alpha1.GitConnection has no field one would fit in and this function
// is handed a [controlstore.StoredConnection], which holds none.
func wireConnection(conn controlstore.StoredConnection) *kelsonv1alpha1.GitConnection {
	out := &kelsonv1alpha1.GitConnection{
		Name:         conn.Name,
		Provider:     string(conn.Spec.Provider),
		Host:         conn.Spec.EffectiveHost(),
		Owner:        wireOwner(conn.Spec.Owner),
		AuthKind:     kelsonv1alpha1.GitAuthKind_GIT_AUTH_KIND_UNSPECIFIED,
		Account:      conn.Status.Account,
		Repositories: conn.Status.Repositories,
		Ready:        conn.Status.Ready,
		Reachable:    conn.Status.Reachable,
		Message:      conn.Status.Message,
	}
	switch {
	case conn.Spec.Auth.GitHubApp != nil:
		app := conn.Spec.Auth.GitHubApp
		out.AuthKind = kelsonv1alpha1.GitAuthKind_GIT_AUTH_KIND_GITHUB_APP
		out.AppId = app.AppID
		out.InstallationId = app.InstallationID
		out.SecretRef = app.SecretRef
	case conn.Spec.Auth.Token != nil:
		out.AuthKind = kelsonv1alpha1.GitAuthKind_GIT_AUTH_KIND_TOKEN
		out.SecretRef = conn.Spec.Auth.Token.SecretRef
	}
	if out.Message == "" && !conn.Status.Observed {
		// Both booleans are false before the first probe as well as after a
		// failed one. Without this a client cannot tell "broken" from "not
		// looked at yet", which the proto names as the thing this field exists
		// to prevent.
		out.Message = "not yet observed: no probe has asked this forge anything. Run TestConnection."
	}
	return out
}

func wireOwner(owner *model.ConnectionOwner) *kelsonv1alpha1.GitOwner {
	if owner == nil || owner.Kind == model.OwnerInstance {
		return &kelsonv1alpha1.GitOwner{Kind: kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_INSTANCE}
	}
	kind := kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_UNSPECIFIED
	switch owner.Kind {
	case model.OwnerUser:
		kind = kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_USER
	case model.OwnerTeam:
		kind = kelsonv1alpha1.GitOwnerKind_GIT_OWNER_KIND_TEAM
	}
	return &kelsonv1alpha1.GitOwner{Kind: kind, Name: owner.Name}
}

// accountOf reduces a repository list to the account the credential acts as.
//
// The seam reports repositories, not identities: there is no ListAccount in
// forge.Provider, and adding one for a field that is a display convenience
// would put a method on every future adapter. So the account is the owner every
// visible repository shares — which is exactly right for an app installation,
// the case ADR-0033 wants it for — and the second return says the credential
// sees more than one owner, which a token belonging to a person routinely does.
func accountOf(repos []forge.Repo) (account string, spread bool) {
	owners := map[string]bool{}
	for _, r := range repos {
		if owner, _, ok := strings.Cut(r.FullName, "/"); ok && owner != "" {
			owners[owner] = true
		}
	}
	if len(owners) != 1 {
		return "", len(owners) > 1
	}
	for owner := range owners {
		return owner, false
	}
	return "", false
}

func probeSummary(conn controlstore.StoredConnection, account string, spread bool, count int) string {
	switch {
	case account != "":
		return fmt.Sprintf("%s answered: the credential acts as %s and can see %s",
			conn.Spec.EffectiveHost(), account, plural(count, "repository", "repositories"))
	case spread:
		return fmt.Sprintf("%s answered: the credential can see %s across several accounts, so there is no "+
			"single account to report", conn.Spec.EffectiveHost(), plural(count, "repository", "repositories"))
	default:
		return fmt.Sprintf("%s answered, and the credential can see no repositories. For a GitHub App that is "+
			"an installation with nothing selected — add repositories to it",
			conn.Spec.EffectiveHost())
	}
}

// secretCause explains a Secret that could not be read. The store's own error
// already names the object and the key and says how to fix it, so this only
// puts it in the sentence a probe answer is.
func secretCause(err error) string {
	var se controlstore.Error
	if errors.As(err, &se) {
		return se.Message + ". " + se.Remediation
	}
	return "the connection's Secret could not be read: " + err.Error()
}

// mintCause names which of the credential failures happened. They are separated
// because their remedies have nothing in common: a rejected key is a
// reconnection, an uninstalled app is three clicks on GitHub, and a forge that
// did not answer is a network path.
func mintCause(conn controlstore.StoredConnection, err error) string {
	host := conn.Spec.EffectiveHost()
	switch {
	case errors.Is(err, forge.ErrNotInstalled):
		return fmt.Sprintf("%s authenticated the app but reports no installation covering it: the app exists and "+
			"is not installed, was uninstalled, or was transferred away. Install it and pick the repositories "+
			"kelson may see", host)
	case errors.Is(err, forge.ErrAuthFailed):
		return fmt.Sprintf("%s rejected the credential in Secret %s: it is expired, revoked, or not valid for this "+
			"host. Write a new value into that Secret — the connection does not change when the credential does",
			host, secretRefOf(conn))
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s did not answer within %s. The credential was never judged; this is a network path "+
			"or a forge that is down, not a credential fault", host, connectionProbeTimeout)
	case errors.Is(err, context.Canceled):
		return fmt.Sprintf("the probe of %s was cancelled before %s answered", host, host)
	default:
		return fmt.Sprintf("%s could not be reached with this connection's credential: %s", host, err.Error())
	}
}

// listCause explains a repository listing that failed after the credential was
// accepted.
func listCause(conn controlstore.StoredConnection, err error) string {
	host := conn.Spec.EffectiveHost()
	if errors.Is(err, forge.ErrAuthFailed) {
		return fmt.Sprintf("%s accepted the credential and then refused to list repositories with it: the token is "+
			"valid and lacks the scope this read needs", host)
	}
	return fmt.Sprintf("the credential authenticated against %s, and listing its repositories did not finish: %s. "+
		"Clones and builds through this connection are unaffected — the account and the repository count are "+
		"what could not be read", host, err.Error())
}

// reachableDespite decides whether a failed listing still counts as reachable.
//
// It does, unless the forge refused the credential on the second call: minting
// worked, so the connection authenticates, and the delivery path needs nothing
// more than that. A 401 or 403 on the listing is the one answer that contradicts
// the mint, and it is reported as unreachable because it means the credential is
// not what the connection claims it is.
func reachableDespite(err error) bool { return !errors.Is(err, forge.ErrAuthFailed) }

func reachableReason(err error) string {
	if reachableDespite(err) {
		return controlstore.ReasonReachable
	}
	return controlstore.ReasonUnreachable
}

func secretRefOf(conn controlstore.StoredConnection) string {
	switch {
	case conn.Spec.Auth.GitHubApp != nil:
		return conn.Spec.Auth.GitHubApp.SecretRef
	case conn.Spec.Auth.Token != nil:
		return conn.Spec.Auth.Token.SecretRef
	default:
		return "(none)"
	}
}

// providerNames lists the adapters that exist, for the remediation on a
// connection declaring one that does not.
func providerNames() string {
	names := make([]string, 0, len(model.GitProviders))
	for _, p := range model.GitProviders {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
