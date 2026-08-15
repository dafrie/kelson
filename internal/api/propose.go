package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
)

// ProposeSpec: the edit kelson cannot store, offered to the repository that can
// (#248, ADR-0033 decision 3's [forge.PRProposer]).
//
// # What this is for, in one paragraph
//
// ADR-0027 decision 6 tells users to keep their Project and Environment
// documents in a repository and let Flux apply them, and the moment they do,
// PutSpec becomes a write git undoes on the next reconcile — silently, minutes
// later, with a successful RPC in between. GetSpec now reports which documents
// are in that state ([controlstore.GitOpsOwner]); this is what a client offers
// instead of the Save it has to disable. The change goes to the repository as a
// pull request, a human merges it, and Flux applies it — which is the same path
// the document took to get there.
//
// # Nothing here writes kelson state, and that is the whole design
//
// The store is not touched, no cluster is touched, and no environment changes.
// What changes is a branch in the user's repository. That is why the policy
// classification below is what it is, and why a failure here leaves nothing
// half-done: the objects a failed proposal leaves behind are unreferenced git
// blobs the forge collects itself.
//
// # kelson does not propose what it would have refused to store
//
// The documents are decoded and validated before the connection is even
// resolved, and findings come back inline exactly as PutSpec's do — an invalid
// document is an answer, not a transport failure. Proposing an unparseable
// document would be asking a human to review kelson's bug, and the pull request
// would then break their cluster on merge.

// ProposeSpec opens a pull request carrying the edited documents.
func (s *Server) ProposeSpec(ctx context.Context, req *connect.Request[kelsonv1alpha1.ProposeSpecRequest]) (*connect.Response[kelsonv1alpha1.ProposeSpecResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	project := strings.TrimSpace(msg.GetProject())
	if project == "" {
		return nil, failRequest(fmt.Errorf("api: ProposeSpec needs the project the documents belong to. Unlike " +
			"PutSpec it is a field rather than a value inside the YAML, because a restricted credential's scope " +
			"is checked against it before the request is read"))
	}
	auditTarget(ctx, project, "")

	if strings.TrimSpace(msg.GetRepository()) == "" {
		return nil, failRequest(fmt.Errorf("api: ProposeSpec needs the repository to propose against, as " +
			"\"owner/name\". kelson knows which Kustomization reconciles a document and not which repository that " +
			"Kustomization reads, so the repository is the caller's to name (see GitOpsOwnership in spec.proto)"))
	}
	files, err := proposedFiles(msg.GetFiles())
	if err != nil {
		return nil, failRequest(err)
	}

	// The same split PutSpec draws (errors.go's specFindings): a document the
	// *model* rejected travels inline, because "would you propose this?" was
	// answered; anything else is a failure of this server and stays a
	// ConnectRPC error.
	if err := s.validateProposed(ctx, project, files); err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.ProposeSpecResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}

	// Agent policy (ADR-0025). See [Server.guardProposal] for why a proposal is
	// filed under `spec-write` and why `propose-only` does not refuse it.
	if err := s.guardProposal(ctx, model.AgentOpSpecWrite, project); err != nil {
		return nil, err
	}

	branch := strings.TrimSpace(msg.GetBranch())
	if branch == "" {
		branch, err = proposalBranch(project, msg.GetIdempotencyKey())
		if err != nil {
			return nil, failRequest(err)
		}
	}

	// The rehearsal rung. RENDER resolves the connection and the capability —
	// so "this connection cannot open pull requests" is learned without
	// creating a branch — and stops before the forge is asked to write.
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		if _, err := s.proposer(ctx, msg.GetConnection()); err != nil {
			return nil, err
		}
		return connect.NewResponse(&kelsonv1alpha1.ProposeSpecResponse{
			Branch:     branch,
			BaseBranch: msg.GetBaseBranch(),
			DryRun:     true,
		}), nil
	}

	p, err := s.proposer(ctx, msg.GetConnection())
	if err != nil {
		return nil, err
	}

	openCtx, cancel := context.WithTimeout(ctx, proposalTimeout)
	defer cancel()
	url, err := p.proposer.OpenPullRequest(openCtx, p.conn, strings.TrimSpace(msg.GetRepository()), forge.Proposal{
		BaseBranch:    strings.TrimSpace(msg.GetBaseBranch()),
		Branch:        branch,
		CommitMessage: proposalCommitMessage(msg, project),
		Files:         files,
		Title:         proposalTitle(msg, project),
		Body:          proposalBody(msg, project, files),
	})
	if err != nil {
		return nil, proposalFailed(p.stored, strings.TrimSpace(msg.GetRepository()), err)
	}

	// The pull request's URL is what this call produced, which is what Revision
	// records (controlstore.AuditChange). A proposal produces no revision of
	// anything kelson stores — that is the point of it — and recording nothing
	// would make the one call that changes somebody's repository the one call
	// with no trace of what it changed.
	auditChange(ctx, controlstore.AuditChange{Revision: url})
	return connect.NewResponse(&kelsonv1alpha1.ProposeSpecResponse{
		Url:        url,
		Branch:     branch,
		BaseBranch: strings.TrimSpace(msg.GetBaseBranch()),
	}), nil
}

// filePaths is the proposal's paths in sorted order, so every list a caller
// reads — the pull-request body, an error, a test's assertion — is in the same
// order as the requests the adapter makes.
func filePaths(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for path := range files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// proposalTimeout bounds the forge half of one proposal.
//
// It is longer than the browse and probe budgets because the work is: three
// reads, a blob per file, a tree, a commit, a ref and the pull request, each a
// round trip. A caller waiting on it has already reviewed a diff and pressed a
// button, so the cost of waiting is lower than the cost of a proposal abandoned
// halfway through writing git objects.
const proposalTimeout = 60 * time.Second

// proposedFiles converts the request's file list into the map [forge.Proposal]
// takes, refusing the two shapes that would silently lose a document: no files
// at all, and two entries for one path.
func proposedFiles(in []*kelsonv1alpha1.ProposedFile) (map[string][]byte, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("api: ProposeSpec needs at least one file: a pull request with no changes is not a proposal")
	}
	out := make(map[string][]byte, len(in))
	for _, f := range in {
		path := strings.TrimSpace(f.GetPath())
		if path == "" {
			return nil, fmt.Errorf("api: a proposed file needs the path it takes in the repository")
		}
		if len(f.GetContent()) == 0 {
			return nil, fmt.Errorf("api: the proposed file %q is empty. Proposing an empty document would delete "+
				"the resource it describes on the next reconcile, which is a deletion dressed up as an edit", path)
		}
		if _, dup := out[path]; dup {
			return nil, fmt.Errorf("api: the proposal names %q twice, and one of the two would be silently "+
				"discarded — a pull request writes each path once", path)
		}
		out[path] = f.GetContent()
	}
	return out, nil
}

// validateProposed decodes every proposed file and validates what they add up
// to, returning inline findings in PutSpec's vocabulary.
//
// # An Environment is validated against the Project it resolves against
//
// A proposal routinely carries one environment's document and nothing else,
// and half of model's environment rules are about the project (a component the
// project does not declare, an image pin for a component built from source).
// So when the files carry no Project, the *stored* one is the context — which
// is also the honest one, because the stored Project is what the cluster will
// resolve this environment against after the merge.
//
// A project the store does not hold is refused rather than validated loosely:
// kelson would be proposing a document it cannot judge, into a repository it
// cannot read, and "probably fine" is not a thing to put in front of a
// reviewer.
func (s *Server) validateProposed(ctx context.Context, project string, files map[string][]byte) error {
	var proposedProject *model.Project
	var environments []*model.Environment
	var errs model.Errors

	for _, path := range filePaths(files) {
		docs, derrs := model.DecodeDocuments(files[path])
		errs = append(errs, derrs...)
		for _, d := range docs {
			switch v := d.(type) {
			case *model.Project:
				if proposedProject != nil {
					return fmt.Errorf("api: the proposal carries two Project documents (%s and %s); "+
						"a project has exactly one", proposedProject.Metadata.Name, v.Metadata.Name)
				}
				proposedProject = v
			case *model.Environment:
				environments = append(environments, v)
			}
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if proposedProject == nil && len(environments) == 0 {
		return fmt.Errorf("api: the proposed files hold no Project or Environment document. " +
			"ProposeSpec proposes kelson documents; a repository's other files are not this RPC's to write")
	}

	if proposedProject != nil {
		if proposedProject.Metadata.Name != project {
			return fmt.Errorf("api: the proposed Project document names %q and the request names %q",
				proposedProject.Metadata.Name, project)
		}
		if findings := model.ValidateSet(proposedProject, environments...); len(findings) > 0 {
			return findings
		}
		return nil
	}

	stored, err := s.storedProject(ctx, project)
	if err != nil {
		return err
	}
	var findings model.Errors
	for _, env := range environments {
		findings = append(findings, model.ValidateEnvironment(env, stored)...)
	}
	if len(findings) > 0 {
		return findings
	}
	return nil
}

// storedProject reads the Project document the store holds, for the validation
// context above.
func (s *Server) storedProject(ctx context.Context, project string) (*model.Project, error) {
	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}
	stored, err := s.specs.Get(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("api: this proposal carries no Project document, so the stored one is what its "+
			"environments are validated against, and it could not be read: %w. Include the Project document in "+
			"the proposal to validate the set on its own", err)
	}
	p, ok := decodeProjectDocument(stored.Documents.Project)
	if !ok {
		return nil, fmt.Errorf("api: the stored Project document for %q does not decode, so an Environment "+
			"proposed on its own cannot be validated against it. Include the Project document in the proposal", project)
	}
	return p, nil
}

// connectionProposal is a connection resolved far enough to propose through it.
type connectionProposal struct {
	stored   controlstore.StoredConnection
	proposer forge.PRProposer
	conn     forge.Conn
}

// proposer is [Server.browse]'s twin for the proposing capability, in the same
// order and for the same reason: the capability is checked before the Secret is
// opened, so a connection that could never have opened a pull request does not
// have its credential read to find that out.
func (s *Server) proposer(ctx context.Context, name string) (connectionProposal, error) {
	if strings.TrimSpace(name) == "" {
		return connectionProposal{}, failRequest(fmt.Errorf("api: ProposeSpec needs the connection to propose " +
			"through. It is not resolved by host match: ADR-0033 decision 4's match answers \"which credential " +
			"clones this project's source\", and the repository holding a project's documents is routinely a " +
			"different one"))
	}
	if s.connections == nil {
		return connectionProposal{}, unimplemented("the git connection store")
	}
	conn, err := s.connections.Get(ctx, name)
	if err != nil {
		return connectionProposal{}, failRequest(err)
	}

	provider, ok := s.forge(string(conn.Spec.Provider))
	if !ok {
		return connectionProposal{}, unproposable(connectionError{
			Code:     ErrConnectionProviderUnknown,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q declares provider %q, which no adapter in this build of kelson "+
				"speaks, so nothing here can open a pull request through it", conn.Name, conn.Spec.Provider),
			Remediation: fmt.Sprintf("kelson speaks %s. Copy the document and commit it yourself — the export is "+
				"the same bytes this would have proposed", providerNames()),
		})
	}
	proposer, ok := provider.(forge.PRProposer)
	if !ok {
		return connectionProposal{}, unproposable(connectionError{
			Code:     ErrConnectionCapabilityUnsupported,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q speaks %q, and that adapter opens no pull requests: a bare git "+
				"host has none to open. What it can do is %s", conn.Name, conn.Spec.Provider, capabilitiesOf(provider)),
			Remediation: "copy the document from the export and commit it yourself. Proposing is an optional " +
				"capability (ADR-0033 decision 3: absence degrades the UI, never the deploy), so this connection " +
				"is not broken and there is nothing on it to fix",
		})
	}

	if s.connectionSecrets == nil {
		return connectionProposal{}, unimplemented("reading the Secret a connection references")
	}
	material, err := s.connectionSecrets.ReadAuthSecret(ctx, conn)
	if err != nil {
		return connectionProposal{}, failRequest(err)
	}
	return connectionProposal{stored: conn, proposer: proposer, conn: forgeConn(conn, material)}, nil
}

// unproposable is [unbrowsable] for the proposing capability: Unimplemented and
// never InvalidArgument, for exactly the reason recorded there — there is no
// request that makes a `generic` connection grow a pull-request API, and an
// agent that read InvalidArgument would rewrite and retry forever.
func unproposable(e connectionError) error { return fail(connect.CodeUnimplemented, e) }

// ErrConnectionWriteNotPermitted is a connection whose credential authenticated
// and whose forge refused the *write*.
//
// It is its own code because it is the expected answer, not an exotic one: the
// app manifest of ADR-0033 decision 2 asks for `contents: read` deliberately
// (internal/forge/githubmanifest.go: "an installation granted more would be
// kelson asking for write access to source it only ever reads"), and opening a
// pull request writes git objects. So the first user to press Propose on an
// app connection meets this, and what they need is the sentence below rather
// than a 403 relayed from a forge.
const ErrConnectionWriteNotPermitted = "connection/write-not-permitted"

// proposalFailed turns a forge refusal into the answer the user can act on.
//
// The four cases are separated because their remedies have nothing in common,
// which is the same discipline [mintCause] applies to the probe: a permission
// is a checkbox on an app, an uninstalled app is three clicks, a rejected
// credential is a rotation, and everything else is a dependency that did not
// answer.
func proposalFailed(conn controlstore.StoredConnection, repository string, err error) error {
	host := conn.Spec.EffectiveHost()
	switch {
	case errors.Is(err, forge.ErrWriteNotPermitted):
		return fail(connect.CodePermissionDenied, connectionError{
			Code:     ErrConnectionWriteNotPermitted,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q authenticated against %s and was refused permission to write to "+
				"%s. Opening a pull request creates a blob, a tree, a commit and a branch, which needs "+
				"`contents: write` — and kelson's app asks for `contents: read`, on purpose, because everything "+
				"else it does only reads your source (ADR-0033 decision 2)", conn.Name, host, repository),
			Remediation: fmt.Sprintf("grant it, or commit the change yourself. To grant it: open the kelson app "+
				"at %s/settings/apps, set Permissions & events → Repository permissions → Contents to \"Read and "+
				"write\", and accept the new permission on the installation at %s/settings/installations — GitHub "+
				"does not apply a widened permission until the installation's owner approves it. For a token "+
				"connection, the token needs write access to %s. To commit it yourself: the export on this screen "+
				"is the same bytes this would have proposed",
				host, host, repository),
		})
	case errors.Is(err, forge.ErrNotInstalled):
		return fail(connect.CodePermissionDenied, connectionError{
			Code:     ErrConnectionWriteNotPermitted,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("connection %q authenticated against %s and reports no installation covering %s: "+
				"the repository holding a project's documents is often not the repository holding its source, and "+
				"an installation scoped to the second cannot write to the first", conn.Name, host, repository),
			Remediation: fmt.Sprintf("add %s to the installation at %s/settings/installations (ADR-0033 decision 2 "+
				"step 3), or commit the exported document yourself", repository, host),
		})
	case errors.Is(err, forge.ErrAuthFailed):
		return fail(connect.CodePermissionDenied, connectionError{
			Code:     ErrConnectionWriteNotPermitted,
			Resource: connectionResource(conn.Name),
			Message: fmt.Sprintf("%s rejected connection %q's credential: it is expired, revoked, or not valid "+
				"for this host", host, conn.Name),
			Remediation: "write a new value into the Secret this connection references — the connection does not " +
				"change when the credential does — and run TestConnection to confirm it before proposing again",
		})
	default:
		return unavailable("connection %q could not open a pull request against %s: %w. Test the connection to "+
			"see whether the credential is still accepted at %s; the exported document is unaffected and can be "+
			"committed by hand", conn.Name, repository, err, host)
	}
}

// proposalBranch names the branch a proposal creates.
//
// # An idempotency key makes the name stable, and that is as far as replay goes
//
// A replayed request derives the same branch and the forge refuses it by name
// ("branch already exists"), which is a visible, recoverable answer. It is not
// the recorded-outcome replay PutSpec offers, and pretending otherwise would be
// worse than saying so: answering a replay with the original pull request's URL
// means storing "the pull request I opened for this key" somewhere, and the
// only somewhere kelson has is the document the caller has just told it git
// owns. A duplicate pull request is the failure this avoids; recovering from
// the collision is the caller's, by proposing under a name of its own.
//
// Without a key the suffix is random, because two people proposing different
// edits to one project in the same minute must not collide.
func proposalBranch(project, idempotencyKey string) (string, error) {
	var suffix string
	if key := strings.TrimSpace(idempotencyKey); key != "" {
		sum := sha256.Sum256([]byte(project + "\x00" + key))
		suffix = hex.EncodeToString(sum[:4])
	} else {
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("api: generating a branch name for the proposal: %w", err)
		}
		suffix = hex.EncodeToString(buf)
	}
	return "kelson/" + project + "-" + suffix, nil
}

// proposalTitle, proposalCommitMessage and proposalBody fill in what the caller
// left blank. They are defaults and not decoration: a pull request whose title
// is empty is refused by the forge, and one whose body does not say where it
// came from is a change a reviewer has to guess the provenance of.
func proposalTitle(msg *kelsonv1alpha1.ProposeSpecRequest, project string) string {
	if title := strings.TrimSpace(msg.GetTitle()); title != "" {
		return title
	}
	return "Update the kelson documents for " + project
}

func proposalCommitMessage(msg *kelsonv1alpha1.ProposeSpecRequest, project string) string {
	if message := strings.TrimSpace(msg.GetBody()); message != "" && strings.TrimSpace(msg.GetTitle()) == "" {
		return message
	}
	return proposalTitle(msg, project)
}

// proposalBody appends the disclosure that ProposedFile's schema makes: each
// file is written whole, so anything else that shared the file is gone. A
// reviewer reading the pull request is the last person who can catch that, and
// they can only catch it if somebody tells them to look.
func proposalBody(msg *kelsonv1alpha1.ProposeSpecRequest, project string, files map[string][]byte) string {
	var b strings.Builder
	if body := strings.TrimSpace(msg.GetBody()); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	b.WriteString("Proposed by kelson for project `")
	b.WriteString(project)
	b.WriteString("`.\n\nEach file below is written whole, from the document kelson holds — if one of them also " +
		"contained something else, this replaces it. Review the diff rather than the summary.\n\n")
	for _, path := range filePaths(files) {
		b.WriteString("- `")
		b.WriteString(path)
		b.WriteString("`\n")
	}
	return b.String()
}
