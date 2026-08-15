// Package forgeconn turns a repository URL into the credential kelson reads it
// with ([ADR-0033](docs/adr/0033-git-connections.md) decisions 4 and 5).
//
// # Why it is a package and not three call sites
//
// Four planes ask the same question and none of them may answer it alone. The
// ref resolver needs a transport credential for `git ls-remote`; the build
// plane needs one for the clone init container; the preview path needs the
// material behind it so it can write flux-operator's Secret; and the webhook
// listener needs the connections a delivery might have come from. Each of
// those is "which connection covers this repository, and what does it mint",
// asked once and answered four ways if nobody owns it.
//
// [controlstore.MatchConnection] already owns the *rule* — it is a pure
// function over values, deliberately, so it can live in the store package
// without dragging the store into a caller. What it does not do is read the
// Secret or call the forge, because it holds neither. This package is the
// other half: it joins the matched connection with its material and hands back
// something the forge adapter can act on. It reads Secrets only through the
// [Store] seam, so it needs no Kubernetes client of its own and stays on the
// standard-library allow-list.
//
// # The bootstrap token is a connection, not a second code path
//
// ADR-0033 decision 5 keeps `KELSON_GIT_TOKEN` alive as bootstrap and says
// exactly how: "at startup the server surfaces it as an implicit,
// instance-owned `generic` connection". [Bootstrap] is that presentation. It is
// synthesised in memory and never written as a custom resource — kelson does
// not author documents the user did not — and it resolves through the same
// [Resolver.Resolve] every stored connection does, so a caller has one
// resolution path and one credential vocabulary rather than an `if token != ""`
// beside every use.
//
// It ranks *below* every stored connection and covers every host, which is the
// only reading that keeps today's behaviour intact: the variable is
// process-wide today and authenticates whatever repository it is pointed at. So
// a stored connection that matches by host wins outright, and the bootstrap
// token answers only where nothing else does. That makes creating a
// GitConnection an upgrade rather than a migration, which is what "bootstrap"
// has to mean.
//
// # Nothing here logs, and everything it learns is registered
//
// Values arrive from [Store.ReadAuthSecret], which registers them with
// internal/redact before returning; credentials minted from them are registered
// here at the moment they are minted (ADR-0033 decision 2). A caller that
// interpolates a [forge.Credential] into a log line gets the redacted envelope
// from its own String method, and a caller that interpolates the raw password
// gets [redact.Sentinel] from the scrubber. Neither is relied on alone.
package forgeconn

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// Store is the half of [controlstore.GitConnectionStore] this package needs.
//
// It is an interface rather than the concrete store for the reason every seam
// in this repository is one: a resolution test must not need an API server. It
// is also narrower than the store on purpose — nothing here creates, deletes or
// writes a status, so nothing here can.
type Store interface {
	// List returns every connection the instance holds.
	List(ctx context.Context) ([]controlstore.StoredConnection, error)
	// ReadAuthSecret reads the material one connection references, registering
	// every value with internal/redact before returning it.
	ReadAuthSecret(ctx context.Context, conn controlstore.StoredConnection) (controlstore.AuthMaterial, error)
}

// BootstrapName is what the implicit `KELSON_GIT_TOKEN` connection is called.
//
// It is a name and not an empty string because it appears in exactly the places
// a stored connection's name does — a resolution diagnostic, the deprecation
// line on the startup banner — and "the connection named nothing" is not a
// sentence an operator can act on. It is also what `source.connection` may name
// to pin a project to the bootstrap credential explicitly, which is the one way
// to say "yes, that one" while the variable is still set.
const BootstrapName = "kelson-git-token"

// BootstrapEnv is the variable [Bootstrap] is read from. It is named here so
// the resolver, the deprecation notice and the error remediations all spell it
// the same way.
const BootstrapEnv = "KELSON_GIT_TOKEN"

// Bootstrap is `KELSON_GIT_TOKEN` presented as a connection (ADR-0033
// decision 5).
//
// It carries the value rather than a Secret reference, and that is the one
// place this package differs from the CR-backed path: there is no Secret to
// reference, because the operator put the credential in the process's
// environment. The value is registered with internal/redact by [NewBootstrap]
// so it is unprintable from the moment the process learns it, exactly as a
// value read out of a Secret is.
type Bootstrap struct {
	token    string
	username string
}

// NewBootstrap returns the implicit connection for a token, or nil for an empty
// one. A nil *Bootstrap is a valid [Resolver.Bootstrap]: it means the variable
// was not set, which is the ordinary state of an instance that has connected a
// forge properly.
func NewBootstrap(token, username string) *Bootstrap {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	redact.Register(token)
	return &Bootstrap{token: token, username: strings.TrimSpace(username)}
}

// Deprecation is the sentence a server prints beside its startup configuration
// when the bootstrap token is in play (ADR-0033 decision 5: "marked deprecated
// in `kelson doctor`-style output").
//
// It is here rather than in the banner that prints it because the banner is one
// caller of several — the controller and the CLI reach the same resolver — and
// a deprecation notice each of them worded differently would be three
// deprecations of three different things.
func (b *Bootstrap) Deprecation() string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("source credential: %s (deprecated — it authenticates every repository this instance reads; "+
		"create a GitConnection instead, which is scoped to one forge and mints short-lived tokens: ADR-0033)", BootstrapEnv)
}

// stored presents the bootstrap token as the store would present a connection,
// so the matcher and every diagnostic see one shape.
func (b *Bootstrap) stored() controlstore.StoredConnection {
	return controlstore.StoredConnection{
		Name: BootstrapName,
		Spec: model.GitConnectionSpec{
			Provider: model.GitProviderGeneric,
			// No host: the variable is process-wide today and covers whatever
			// repository it is pointed at. Giving it one would be inventing a
			// scope the operator never stated, and narrowing what works today.
			Auth: model.GitConnectionAuth{Token: &model.TokenAuth{SecretRef: "(environment: " + BootstrapEnv + ")"}},
		},
		Status: controlstore.ConnectionStatus{Ready: true, Observed: true, Message: "read from " + BootstrapEnv},
	}
}

func (b *Bootstrap) material() controlstore.AuthMaterial {
	return controlstore.AuthMaterial{
		SecretRef: "(environment: " + BootstrapEnv + ")",
		Token:     b.token,
		Username:  b.username,
	}
}

// Resolver answers "which connection covers this repository, and what does it
// mint". It is safe for concurrent use: it holds no mutable state, and the
// forge adapters' own token caches carry their own locks.
type Resolver struct {
	// Store lists connections and reads their Secrets. Nil is a resolver with
	// no stored connections at all, which is the correct configuration for a
	// process that has no cluster access and only the bootstrap token.
	Store Store

	// Bootstrap is the implicit `KELSON_GIT_TOKEN` connection, or nil.
	Bootstrap *Bootstrap
}

// Resolution is one answer: which connection won, and everything a caller needs
// to act through it.
type Resolution struct {
	// Match is what [controlstore.MatchConnection] concluded over the stored
	// connections. A bootstrap answer carries [controlstore.MatchNone] here and
	// sets Bootstrap — the stored connections genuinely matched nothing, and
	// saying otherwise would make the field a lie the moment anyone logged it.
	Match controlstore.ConnectionMatch

	// Stored is the winning connection as the store holds it. It is the
	// synthesised presentation for a bootstrap answer.
	Stored controlstore.StoredConnection

	// Bootstrap reports that the answer is the implicit `KELSON_GIT_TOKEN`
	// connection rather than a stored one.
	Bootstrap bool

	// Provider is the adapter for [Stored]'s provider.
	Provider forge.Provider

	// Conn is the connection joined with its Secret material: what every
	// forge call takes.
	Conn forge.Conn
}

// Name is the winning connection's name.
func (r Resolution) Name() string { return r.Stored.Name }

// Resolve picks the connection for one project source.
//
// sourceGit is `Project.spec.source.git` and named is
// `Project.spec.source.connection`, which may be empty. The second return is
// false for "no connection covers this source", which is the ordinary answer
// for a public repository and never an error: ADR-0033 decision 4 keeps
// anonymous the default, and [controlstore.MatchNone]'s own doc says a caller
// turns it into "clone anonymously".
//
// The two refusals are the ones the override field exists to prevent:
// [controlstore.MatchAmbiguous] names every tied connection, and
// [controlstore.MatchMissing] names the connection that was asked for and does
// not exist. Both point at `source.connection`, because that is the field that
// fixes them.
func (r *Resolver) Resolve(ctx context.Context, sourceGit, named string) (Resolution, bool, error) {
	stored, err := r.list(ctx)
	if err != nil {
		return Resolution{}, false, err
	}

	refs := make([]controlstore.ConnectionRef, 0, len(stored))
	for _, c := range stored {
		refs = append(refs, c.Ref())
	}
	match := controlstore.MatchConnection(sourceGit, named, refs)

	switch match.Kind {
	case controlstore.MatchAmbiguous:
		return Resolution{}, false, ambiguous(sourceGit, match.Candidates)
	case controlstore.MatchMissing:
		// The bootstrap connection is nameable: an operator migrating off the
		// variable may want to pin a project to it while both exist. It is
		// checked here rather than before the match so a *stored* connection of
		// the same name wins, which is what makes the eventual real connection
		// replace the implicit one without the spec changing.
		if r.Bootstrap != nil && match.Name == BootstrapName {
			return r.bootstrapResolution()
		}
		return Resolution{}, false, missing(match.Name, names(stored))
	case controlstore.MatchNone:
		if r.Bootstrap != nil {
			return r.bootstrapResolution()
		}
		return Resolution{Match: match}, false, nil
	}

	for _, c := range stored {
		if c.Name != match.Name {
			continue
		}
		res, err := r.join(ctx, c)
		if err != nil {
			return Resolution{}, false, err
		}
		res.Match = match
		return res, true, nil
	}
	// MatchConnection only returns a name it was given one for, so this is
	// unreachable rather than defensive — but a silent zero Resolution would be
	// an anonymous clone of a private repository, which is worth a sentence.
	return Resolution{}, false, fmt.Errorf("forgeconn: the matcher chose connection %q, which is not in the list it was given", match.Name)
}

// Credential resolves and mints in one call: what the ref resolver and the
// build plane both want.
//
// The second return is false for an anonymous read, so a caller writes
// `if !ok { anonymous }` rather than inspecting the credential for emptiness.
// Every minted password is registered with internal/redact before it is
// returned, whichever auth shape produced it — an installation token is learned
// here, and a stored token was learned by the store, and a caller must not have
// to know which.
func (r *Resolver) Credential(ctx context.Context, sourceGit, named string) (forge.Credential, Resolution, bool, error) {
	res, ok, err := r.Resolve(ctx, sourceGit, named)
	if err != nil || !ok {
		return forge.Credential{}, res, false, err
	}
	cred, err := res.Provider.MintCloneCredential(ctx, res.Conn, sourceGit)
	if err != nil {
		return forge.Credential{}, res, false, mintFailed(res, sourceGit, err)
	}
	redact.Register(cred.SecretValues()...)
	return cred, res, true, nil
}

// ByProvider returns every connection of one provider, joined with its
// material — the webhook listener's question, which is not "which connection
// covers this repository" but "which of the connections I hold could have sent
// this delivery".
//
// A connection whose Secret cannot be read is skipped rather than failing the
// call, and that is the property the webhook endpoint depends on: one broken
// connection must not make an instance stop verifying deliveries from the
// others. The skipped ones are returned so a caller can say how many candidates
// it could not even consider — silence about them would turn a misconfigured
// Secret into "signature does not match".
func (r *Resolver) ByProvider(ctx context.Context, provider string) (found []Resolution, skipped []string, err error) {
	stored, err := r.list(ctx)
	if err != nil {
		return nil, nil, err
	}
	want := strings.ToLower(strings.TrimSpace(provider))
	for _, c := range stored {
		if !strings.EqualFold(string(c.Spec.Provider), want) {
			continue
		}
		res, err := r.join(ctx, c)
		if err != nil {
			skipped = append(skipped, c.Name)
			continue
		}
		found = append(found, res)
	}
	return found, skipped, nil
}

// Get resolves one connection by name, joined with its material. It is the
// webhook callback's path: an `installation` event names the app it is about,
// and the connection it belongs to has already been chosen by signature.
func (r *Resolver) Get(ctx context.Context, name string) (Resolution, error) {
	stored, err := r.list(ctx)
	if err != nil {
		return Resolution{}, err
	}
	for _, c := range stored {
		if c.Name == name {
			return r.join(ctx, c)
		}
	}
	if r.Bootstrap != nil && name == BootstrapName {
		res, _, err := r.bootstrapResolution()
		return res, err
	}
	return Resolution{}, missing(name, names(stored))
}

func (r *Resolver) list(ctx context.Context) ([]controlstore.StoredConnection, error) {
	if r.Store == nil {
		return nil, nil
	}
	return r.Store.List(ctx)
}

func (r *Resolver) bootstrapResolution() (Resolution, bool, error) {
	stored := r.Bootstrap.stored()
	provider, ok := forge.For(string(stored.Spec.Provider))
	if !ok {
		return Resolution{}, false, fmt.Errorf("forgeconn: no adapter for provider %q", stored.Spec.Provider)
	}
	// The same join a stored connection goes through, from material this
	// process holds rather than material a Secret held. Building the Conn some
	// other way here is how the two paths would drift.
	material := r.Bootstrap.material()
	return Resolution{
		Stored:    stored,
		Bootstrap: true,
		Provider:  provider,
		Conn: forge.Conn{
			Provider: string(stored.Spec.Provider),
			Token:    material.Token,
			Username: material.Username,
		},
	}, true, nil
}

// join reads the connection's Secret and builds the [forge.Conn] a call takes.
func (r *Resolver) join(ctx context.Context, c controlstore.StoredConnection) (Resolution, error) {
	provider, ok := forge.For(string(c.Spec.Provider))
	if !ok {
		return Resolution{}, unknownProvider(c)
	}
	if r.Store == nil {
		return Resolution{}, fmt.Errorf("forgeconn: connection %q needs a store to read %s from", c.Name, authSecretName(c))
	}
	material, err := r.Store.ReadAuthSecret(ctx, c)
	if err != nil {
		return Resolution{}, err
	}

	conn := forge.Conn{
		Provider: string(c.Spec.Provider),
		Host:     c.Spec.EffectiveHost(),
		Token:    material.Token,
		Username: material.Username,
	}
	if app := c.Spec.Auth.GitHubApp; app != nil {
		conn.AppID = app.AppID
		conn.InstallationID = app.InstallationID
		conn.PrivateKeyPEM = material.PrivateKeyPEM
		conn.WebhookSecret = material.WebhookSecret
	}
	return Resolution{Stored: c, Provider: provider, Conn: conn}, nil
}

func authSecretName(c controlstore.StoredConnection) string {
	switch {
	case c.Spec.Auth.GitHubApp != nil:
		return c.Spec.Auth.GitHubApp.SecretRef
	case c.Spec.Auth.Token != nil:
		return c.Spec.Auth.Token.SecretRef
	default:
		return "(no secret)"
	}
}

func names(conns []controlstore.StoredConnection) []string {
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// The refusals. They are [delivery.Error] values because that is the shape
// every plane below the API already speaks (internal/gitref's own auth failure
// is one), so a resolution refusal reaches a CLI, the UI and an agent through
// the machinery that is already there rather than as a bare string.

// ErrResource is what a resolution refusal is *about*. It is not the Project
// and not the connection: it is the act of choosing between them, which is what
// the remediation has to talk about.
const ErrResource = "git/connection"

// ambiguous is ADR-0033 decision 4's "a resolution error naming both and the
// field that disambiguates, never a silent pick".
func ambiguous(sourceGit string, candidates []string) error {
	return delivery.ApplyFailed(ErrResource, "$.spec.source.connection",
		fmt.Sprintf("%d connections cover %s and none of them is more specific: %s",
			len(candidates), display(sourceGit), strings.Join(candidates, ", ")),
		fmt.Sprintf("set spec.source.connection on the Project to one of them (%s). Host matching is what "+
			"makes one connection zero configuration; with two covering the same repository kelson will not "+
			"guess which credential a build acts as (ADR-0033 decision 4)", strings.Join(candidates, ", ")))
}

// missing is the other half of the same decision: an explicit name that matches
// nothing is refused rather than quietly falling back to host matching, because
// the author said which one.
func missing(name string, held []string) error {
	have := "this instance holds no connections at all"
	if len(held) > 0 {
		have = "the connections this instance holds are: " + strings.Join(held, ", ")
	}
	return delivery.ApplyFailed(ErrResource, "$.spec.source.connection",
		fmt.Sprintf("spec.source.connection names %q, and no such connection exists", name),
		fmt.Sprintf("%s. Create it on the Git connections page, correct the name, or remove the field to "+
			"resolve by host match. kelson does not fall back to host matching here on purpose: the author "+
			"named a credential, and silently using another one is what the field exists to prevent", have))
}

// unknownProvider is a connection declaring a forge no adapter serves. It is a
// refusal rather than a fall back to the generic adapter for [forge.For]'s
// reason: answering a question about GitLab merge requests with a token that
// can only clone is worse than saying so.
func unknownProvider(c controlstore.StoredConnection) error {
	known := make([]string, 0, len(model.GitProviders))
	for _, p := range model.GitProviders {
		known = append(known, string(p))
	}
	return delivery.ApplyFailed(ErrResource, "$.spec.provider",
		fmt.Sprintf("connection %q declares provider %q, which no adapter in this build of kelson speaks",
			c.Name, c.Spec.Provider),
		fmt.Sprintf("use one of: %s. The enum grows one value per adapter (ADR-0033 decision 3), so a provider "+
			"kelson does not name is one whose integration has not been written rather than one you can enable",
			strings.Join(known, ", ")))
}

// mintFailed wraps whatever the forge said with which connection said it. The
// forge's own error is the cause and not the message: "401 Unauthorized" tells
// an operator nothing about which of their connections is expired.
func mintFailed(res Resolution, sourceGit string, cause error) error {
	e := delivery.ApplyFailed(ErrResource, "auth",
		fmt.Sprintf("connection %q could not mint a credential for %s (%s)", res.Name(), display(sourceGit), res.Conn),
		fmt.Sprintf("check connection %q on the Git connections page: a revoked installation, a deleted Secret and an "+
			"expired token all land here, and its Reachable condition says which", res.Name()))
	e.Cause = cause.Error()
	return e
}

func display(sourceGit string) string {
	if s := strings.TrimSpace(sourceGit); s != "" {
		return s
	}
	return "the project's source"
}
