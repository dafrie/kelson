package model

// GitConnection is the forge kelson can talk to, and the Secret it talks with
// (ADR-0033). It is the third document kind, and it is the first one that is
// not an authoring document: nothing about it is rendered, and the renderer
// never sees one (ADR-0033 decision 1). It is read by the planes that already
// have cluster access — the server, the controller, the build plane — which is
// the same split ClusterProfile already draws.
//
// The document carries identifiers and references and never a credential
// value. That half of ADR-0009 is untouched by ADR-0033: the material lives in
// a Kubernetes Secret somebody else manages, and `secretRef` is its name. What
// ADR-0033 amends is the *use* — kelson now reads those Secrets to mint
// short-lived tokens and to call forge APIs — which changes nothing about what
// a spec may contain.
type GitConnection struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta        `yaml:"metadata" json:"metadata" jsonschema:"required"`
	Spec     GitConnectionSpec `yaml:"spec" json:"spec" jsonschema:"required"`
}

type GitConnectionSpec struct {
	// Provider selects the adapter that speaks to this forge. The enum grows
	// per adapter and not per aspiration: `generic` is the universal token
	// fallback that every git host answers, and a named provider is a promise
	// that an adapter for it exists (ADR-0033 decision 3).
	Provider GitProvider `yaml:"provider" json:"provider" jsonschema:"required,enum=github,enum=generic,description=the forge this connection speaks to; the enum grows one value per adapter"`

	// Host is the forge's base URL — what a self-hosted GHE or GitLab sets, and
	// what host-match resolution compares a Project's source URL against
	// (ADR-0033 decision 4).
	//
	// Empty means DefaultGitHubHost for provider github, which is the common
	// case and the reason the field is not required outright. Every other
	// provider must say where it lives, because there is nothing to default to.
	Host string `yaml:"host,omitempty" json:"host,omitempty" jsonschema:"format=uri,description=forge base URL such as https://github.com or https://git.acme.internal; defaults to https://github.com for provider github"`

	// Auth is how kelson authenticates to that forge: exactly one of the two.
	Auth GitConnectionAuth `yaml:"auth" json:"auth" jsonschema:"required"`

	// Owner is who may edit this connection. Nil means the instance owns it,
	// which is the only answer that is enforced today (ADR-0033 decision 6):
	// use is granted by visibility and mutation by ownership, and until
	// principals exist (#231) there is no subject to enforce mutation against.
	// The field is stored shown and validated now so tenancy attaches to it
	// rather than migrating it.
	Owner *ConnectionOwner `yaml:"owner,omitempty" json:"owner,omitempty"`
}

// GitProvider is the forge an adapter is written for. It is a closed enum for
// the reason ADR-0017's provider enum is: a value here is a promise that the
// shape has been run end to end, not that somebody could imagine running it.
type GitProvider string

const (
	// GitProviderGitHub is github.com and GitHub Enterprise Server: the only
	// provider with the app-manifest flow (ADR-0033 decision 2).
	GitProviderGitHub GitProvider = "github"

	// GitProviderGeneric is any git host reachable over HTTPS with a token.
	// It offers private clones and nothing else — no repo picker, no webhook,
	// no commit status — which is what the capability seam makes legible
	// (ADR-0033 decision 3).
	GitProviderGeneric GitProvider = "generic"
)

// GitProviders is the enum, in the order error remediations list it.
var GitProviders = []GitProvider{GitProviderGitHub, GitProviderGeneric}

// Valid reports whether p is one of the enum's members.
func (p GitProvider) Valid() bool {
	for _, known := range GitProviders {
		if p == known {
			return true
		}
	}
	return false
}

// DefaultGitHubHost is the forge base URL a `provider: github` connection means
// when it names none. It is written out as a constant rather than left implicit
// so host-match resolution and the UI answer "which host is this" the same way
// for a connection that never said.
const DefaultGitHubHost = "https://github.com"

// GitConnectionAuth is how kelson authenticates to a forge. Exactly one member
// is set: the two are different credential *kinds*, not two spellings of one.
//
// An installation token minted from a GitHub App is scoped to the repositories
// somebody chose, expires within the hour and is minted on demand; a token is a
// standing credential pasted into a Secret. Choosing between them is the whole
// of ADR-0033 decision 2, so a document that sets both has not chosen and a
// document that sets neither cannot authenticate at all.
//
// Neither member holds a value. Both name a Secret, and kelson reads that
// Secret at use time in the planes that have cluster access — never in the
// renderer and never into a spec (ADR-0009).
type GitConnectionAuth struct {
	// GitHubApp is the per-instance GitHub App of ADR-0033 decision 2, created
	// by the app-manifest flow. Provider github only.
	GitHubApp *GitHubAppAuth `yaml:"githubApp,omitempty" json:"githubApp,omitempty" jsonschema:"description=provider github only; the per-instance GitHub App this connection acts as"`

	// Token is the universal fallback: a PAT / project token / deploy token,
	// held in a Secret. It is the only path for every forge whose adapter has
	// not landed yet.
	Token *TokenAuth `yaml:"token,omitempty" json:"token,omitempty" jsonschema:"description=a personal or project access token held in a Secret; the fallback every forge answers"`
}

// GitHubAppAuth identifies the GitHub App installation this connection acts as.
//
// The two identifiers are public — an app ID and an installation ID appear in
// URLs — and the two things that are not (the private key and the webhook
// secret) live in the Secret named by SecretRef under the keys
// [GitHubAppPrivateKeyKey] and [GitHubAppWebhookSecretKey].
type GitHubAppAuth struct {
	// AppID is the app's numeric ID, as GitHub reports it when the app is
	// created from the manifest.
	AppID int64 `yaml:"appID" json:"appID" jsonschema:"required,minimum=1,description=the GitHub App's numeric ID as the manifest flow reports it"`

	// InstallationID is the installation the app was installed as, recorded
	// from the `installation` webhook event.
	//
	// Zero is the valid pre-install state and not a missing value: the app
	// exists from step 2 of the manifest flow and is installed in step 3, so a
	// connection between the two is a connection with an app and no
	// installation. It cannot mint a token yet, which is a status
	// (`Ready=False`) rather than an invalid document.
	InstallationID int64 `yaml:"installationID,omitempty" json:"installationID,omitempty" jsonschema:"minimum=0,description=the installation this app acts as; 0 is the valid pre-install state before the installation webhook arrives"`

	// SecretRef names the Secret holding the app's private key and webhook
	// secret, in kelson's own namespace. It is a name and never a value.
	SecretRef string `yaml:"secretRef" json:"secretRef" jsonschema:"required,minLength=1,description=name of the Secret holding the app private key and webhook secret; never a value"`
}

// TokenAuth names the Secret holding a forge token.
type TokenAuth struct {
	// SecretRef names the Secret holding the token, and optionally the username
	// it belongs to, under the keys [TokenKey] and [TokenUsernameKey].
	SecretRef string `yaml:"secretRef" json:"secretRef" jsonschema:"required,minLength=1,description=name of the Secret holding the token; never a value"`
}

// The keys kelson reads inside the Secrets a GitConnection names. They are
// named here rather than at each reader for the reason [ValidSecretKey] is
// exported: the writer of the Secret and the reader of it must agree on the
// alphabet and on the words, and a second copy of either would drift.
//
// Nothing in this package reads a Secret. These are the vocabulary the planes
// that can (the server, the controller, the build plane) share with the
// documentation that tells an operator what to put in one.
const (
	// GitHubAppPrivateKeyKey holds the app's RS256 private key, PEM-encoded.
	GitHubAppPrivateKeyKey = "privateKey"

	// GitHubAppWebhookSecretKey holds the HMAC secret every delivery to the
	// webhook endpoint is verified against (ADR-0034 decision 1).
	GitHubAppWebhookSecretKey = "webhookSecret"

	// TokenKey holds the token itself.
	TokenKey = "token"

	// TokenUsernameKey holds the username the token belongs to, where the forge
	// wants one in the basic-auth pair. Optional: most forges accept any
	// username beside a token.
	TokenUsernameKey = "username"
)

// ConnectionOwner is who owns a connection — a discriminated reference whose
// semantics ADR-0033 decision 6 fixes now so that tenancy (#231) does not have
// to re-litigate them.
//
// Use is granted by visibility and mutation by ownership: any project that can
// see a connection may build and preview through it, and editing, rotating or
// deleting it belongs to its owner. Until principals exist there is no subject
// to enforce that against, so `user` and `team` are stored, shown and validated
// and nothing else — which is why the UI copy for them has to say "visible to
// everyone on this instance" until it is not.
type ConnectionOwner struct {
	// Kind is instance, user or team. Only instance is meaningful today.
	Kind string `yaml:"kind" json:"kind" jsonschema:"required,enum=instance,enum=user,enum=team,description=who may edit this connection; only instance is enforced today"`

	// Name is the principal, and it is required for every kind but instance —
	// a user-owned connection that names no user owns nothing. It is refused on
	// `kind: instance`, which has no principal to name.
	Name string `yaml:"name,omitempty" json:"name,omitempty" jsonschema:"description=the owning principal; required when kind is user or team and refused when it is instance"`
}

// Owner kinds. The set is closed and two thirds of it is reserved: the
// alternative was to ship `instance` alone and add the other two with tenancy,
// which would make every stored connection a document tenancy has to migrate.
const (
	OwnerInstance = "instance"
	OwnerUser     = "user"
	OwnerTeam     = "team"
)

// OwnerKinds is the enum, in the order error remediations list it.
var OwnerKinds = []string{OwnerInstance, OwnerUser, OwnerTeam}

// IsInstanceOwned reports whether this connection belongs to the instance
// rather than to a principal. A nil owner is instance-owned: the field is the
// exception and its absence is the day-one experience ADR-0033 decision 6
// describes — one person connects the forge and every project sees it.
func (s GitConnectionSpec) IsInstanceOwned() bool {
	return s.Owner == nil || s.Owner.Kind == OwnerInstance
}

// EffectiveHost is the forge base URL this connection means: what it says, or
// the provider's default when it says nothing. It exists so host-match
// resolution and every display of a connection answer the question the same
// way rather than each deciding what an empty host means.
func (s GitConnectionSpec) EffectiveHost() string {
	if s.Host != "" {
		return s.Host
	}
	if s.Provider == GitProviderGitHub {
		return DefaultGitHubHost
	}
	return ""
}
