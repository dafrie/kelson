package controlstore

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// The connection store, backed by GitConnection custom resources (ADR-0033
// decision 1).
//
// # It is the spec store's shape, minus the version token
//
// A connection is one CR in the server's namespace, written with the same
// server-side apply under [FieldManager] and read back the same way. What it
// does *not* have is an optimistic-concurrency token on the wire, and that is
// the schema's decision rather than an omission: gitconnection.proto has no
// UpdateConnection at all — rotating a credential writes the Secret the
// connection already names, and changing the host or the Secret changes what
// the connection *is*, so the vocabulary is create and delete. With no update
// there is no read-modify-write for a version to protect, so a create against
// an existing name is a `store/version-conflict` (this is not what you thought
// was there) and nothing else asserts a version.
//
// # The Secret read lives here, and only here
//
// [GitConnectionStore.ReadAuthSecret] is the one method in this package that
// opens a Secret the *user* wrote rather than one kelson keeps state in. It is
// here because the api plane may not hold a Kubernetes client (.golangci.yml),
// and it is separate from every other method because the handlers that list and
// show connections must not be able to reach it — internal/api narrows this
// store to two interfaces for exactly that reason, and gitconnection.proto's
// header promises there is no field a credential could travel back in.
//
// Every value it reads is registered with internal/redact before it is
// returned. From that moment the token, the private key and the webhook secret
// cannot appear in a log line, an error message or a structured error detail,
// whichever plane writes it (issue #117) — including the errors this store
// itself returns while failing to use them.

const (
	// stateConnection marks the custom resources this store owns, beside
	// [stateSpec]. Same purpose: it is what tells a reader of `kubectl get
	// gitconnections --show-labels` which objects kelson-server wrote.
	stateConnection = "connection"

	// labelConnection carries the connection's own name, so the provenance
	// labels answer "which connection is this" without parsing the object name.
	labelConnection = "kelson.dev/connection"
)

// The reasons this store stamps on a connection's conditions.
//
// They live here rather than beside [v1alpha1.ConditionReachable] because that
// package's reason vocabulary is a closed set written for the Project and
// Environment conditions, and this slice does not own it. They are strings a
// `kubectl get -o jsonpath` and an agent branch on, so a follow-up that gives
// GitConnection a reconciler should move them next to the condition they
// belong to rather than declare a second set there.
const (
	// ReasonConnectionReady — the document validates and its Secret holds the
	// keys its auth kind needs.
	ReasonConnectionReady = "Ready"

	// ReasonSecretMissing — the Secret the connection names does not exist, or
	// exists without the key this auth kind reads. Ready is false and Reachable
	// cannot even be asked.
	ReasonSecretMissing = "SecretMissing"

	// ReasonReachable — the forge authenticated the credential and answered.
	ReasonReachable = "Reachable"

	// ReasonUnreachable — the forge did not answer, or refused. The message is
	// what separates a revoked installation from a network path that does not
	// exist.
	ReasonUnreachable = "Unreachable"

	// ReasonProviderUnknown — the connection declares a provider no adapter in
	// internal/forge serves. Nothing can be probed; the enum grows per adapter
	// (ADR-0033 decision 3).
	ReasonProviderUnknown = "ProviderUnknown"
)

// StoredConnection is one GitConnection as the store holds it: the spec as
// authored, and what the last probe observed.
type StoredConnection struct {
	// Name is the CR's metadata.name and the string `source.connection` names.
	Name string
	// Spec is model.GitConnectionSpec verbatim — identifiers and Secret names,
	// never a value (ADR-0009).
	Spec model.GitConnectionSpec
	// Version is the CR's resourceVersion. It is carried because a caller may
	// want to log it; no RPC in gitconnection.proto asserts one.
	Version string
	// Generation is `.metadata.generation`, so a status write can stamp the
	// generation it describes.
	Generation int64
	// Status is the flattened status subresource.
	Status ConnectionStatus
}

// Ref is the connection as [MatchConnection] wants it.
func (c StoredConnection) Ref() ConnectionRef {
	return ConnectionRef{Name: c.Name, Host: c.Spec.EffectiveHost(), Account: c.Status.Account}
}

// ConnectionStatus is `GitConnectionStatus` flattened for a caller that may not
// name the API machinery's types: the two conditions as booleans, the
// provider-reported pair, and the prose that says which case an empty answer
// is.
type ConnectionStatus struct {
	// Ready is the Ready condition: the document is well-formed and its Secret
	// exists with the keys its auth kind needs.
	Ready bool
	// Reachable is the Reachable condition: the last probe authenticated
	// against the forge and got an answer.
	Reachable bool
	// Account is who the credential acts as, as the provider reported it.
	Account string
	// Repositories is how many repositories it can see, as the provider
	// reported it.
	Repositories int32
	// Message is why the conditions say what they say.
	Message string
	// Observed reports whether any probe has ever written this status. Both
	// booleans are false before the first probe as well as after a failed one,
	// and a client that could not tell those apart would render "broken" for
	// "not looked at yet" (gitconnection.proto says so about the wire field
	// this feeds).
	Observed bool
	// ObservedGeneration is the generation the status describes.
	ObservedGeneration int64
}

// ConnectionObservation is one probe's outcome, as [GitConnectionStore.UpdateStatus]
// writes it. It is the same triple TestConnectionResponse carries plus the
// condition reasons, because a condition with no reason is one nothing can be
// branched on.
type ConnectionObservation struct {
	Ready        bool
	Reachable    bool
	Account      string
	Repositories int32
	Message      string
	// ReadyReason and ReachableReason default to the Ready/Unreachable pair
	// above when empty, so a caller that only has an outcome still writes a
	// branchable condition.
	ReadyReason     string
	ReachableReason string
}

// AuthMaterial is what a connection's Secret holds, read at use time.
//
// It is returned to exactly one caller — the code that builds a forge.Conn —
// and never travels further: nothing in this struct reaches a wire message,
// because no wire message has a field for it (gitconnection.proto's header).
// Every non-empty value in it has been registered with internal/redact before
// this value existed.
type AuthMaterial struct {
	// SecretRef is the Secret these values came out of, for a diagnostic that
	// must name where to look without quoting what it found.
	SecretRef string
	// Token and Username are the token auth shape's keys.
	Token, Username string
	// PrivateKeyPEM and WebhookSecret are the GitHub App shape's.
	PrivateKeyPEM, WebhookSecret []byte
}

// CreateConnectionOptions carries the write-time controls CreateConnection has.
type CreateConnectionOptions struct {
	// IdempotencyKey, when it matches the key recorded on the stored
	// connection, makes this call a replay: the stored state is returned as
	// success and nothing is written. Without it, a create against a name that
	// exists is a conflict — which is the outcome difference
	// CreateConnectionRequest's comment says the key exists to erase.
	IdempotencyKey string
	// DryRun asks the API server to validate the write and discard it:
	// admission runs against the live object and nothing is persisted.
	DryRun bool
}

// DeleteConnectionOptions carries the same for DeleteConnection.
type DeleteConnectionOptions struct {
	// IdempotencyKey makes a delete replayable. A deleted object takes its
	// annotations with it, so a replayed delete is indistinguishable from a
	// first delete of a connection that never existed; with a key present the
	// absence is reported as success, and without one it is a store/not-found —
	// the same contract [DeleteOptions] states for a spec.
	IdempotencyKey string
	DryRun         bool
}

// GitConnectionStoreOptions configures a [GitConnectionStore].
type GitConnectionStoreOptions struct {
	// Client reads and writes GitConnection custom resources and reads the
	// Secrets they reference. It must be built against a scheme carrying both
	// v1alpha1 and core/v1 — [NewClient] builds exactly that.
	Client client.Client
	// Namespace is where the connections and their Secrets live: kelson's own
	// namespace, which is the only place ADR-0033 decision 1 puts them.
	Namespace string
}

// GitConnectionStore is the custom-resource-backed store of forge connections.
type GitConnectionStore struct {
	client    client.Client
	namespace string
}

// NewGitConnectionStore returns a store over one namespace.
func NewGitConnectionStore(opts GitConnectionStoreOptions) (*GitConnectionStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("controlstore: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("controlstore: a namespace is required")
	}
	return &GitConnectionStore{client: opts.Client, namespace: opts.Namespace}, nil
}

// List returns every connection the instance holds, ordered by name.
//
// It is not filtered by ownership: ADR-0033 decision 6 grants *use* by
// visibility, so a project can resolve through any of them, and a list that hid
// some would describe a different instance than the one that builds.
func (s *GitConnectionStore) List(ctx context.Context) ([]StoredConnection, error) {
	var list v1alpha1.GitConnectionList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("controlstore: list gitconnections in %s: %w", s.namespace, err)
	}
	out := make([]StoredConnection, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, connectionFrom(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one connection by name.
func (s *GitConnectionStore) Get(ctx context.Context, name string) (StoredConnection, error) {
	if err := validSegment("connection", name); err != nil {
		return StoredConnection{}, err
	}
	conn, err := s.read(ctx, name)
	if err != nil {
		return StoredConnection{}, err
	}
	if conn == nil {
		return StoredConnection{}, notStoredConnection(name)
	}
	return connectionFrom(conn), nil
}

// Create writes a new connection.
//
// The spec is stored as given: validating it is the handler's job
// (ADR-0027 decision 5, and gitconnection.proto's CreateConnection accepts only
// the token shape). What this refuses is a name that is already taken, because
// there is no UpdateConnection and a create that silently replaced a connection
// would repoint every project resolving through it at a different forge.
func (s *GitConnectionStore) Create(ctx context.Context, name string, spec model.GitConnectionSpec, opts CreateConnectionOptions) (StoredConnection, error) {
	if err := validSegment("connection", name); err != nil {
		return StoredConnection{}, err
	}
	ref := connectionRef(name)

	current, err := s.read(ctx, name)
	if err != nil {
		return StoredConnection{}, err
	}
	if current != nil {
		// The replay check comes first, for the reason CreateConnectionRequest
		// gives: a retry after a timeout must answer what the first attempt
		// answered, and "already exists" is a different outcome for the same
		// operation.
		if opts.IdempotencyKey != "" && current.Annotations[annIdempotencyKey] == opts.IdempotencyKey {
			return connectionFrom(current), nil
		}
		return StoredConnection{}, VersionConflict(ref,
			fmt.Sprintf("a connection named %q already exists in %s", name, s.namespace),
			"there is no UpdateConnection: a connection's host and Secret are what it *is*, so pointing it "+
				"somewhere else is DeleteConnection and then CreateConnection. To rotate the credential, "+
				"write the Secret this connection already names — the connection does not change")
	}

	desired := &v1alpha1.GitConnection{Spec: spec}
	desired.APIVersion = model.APIVersion
	desired.Kind = v1alpha1.KindGitConnection
	desired.Name = name
	desired.Namespace = s.namespace
	desired.Labels = connectionLabels(name)
	desired.Annotations = idempotencyAnnotation(opts.IdempotencyKey)

	if err := s.create(ctx, desired, opts.DryRun); err != nil {
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			return StoredConnection{}, withCause(VersionConflict(ref,
				fmt.Sprintf("connection %q was created while this write was in flight", name),
				"read it with GetConnection; if it is not the connection you meant, delete it and create yours"), err)
		}
		return StoredConnection{}, fmt.Errorf("controlstore: write %s: %w", ref, err)
	}
	if opts.DryRun {
		// Nothing was persisted, so there is nothing to read back. The
		// projection of the desired object is the honest answer: it is what
		// admission accepted, with no resourceVersion because no version exists.
		return connectionFrom(desired), nil
	}

	written, err := s.read(ctx, name)
	if err != nil {
		return StoredConnection{}, err
	}
	if written == nil {
		return StoredConnection{}, fmt.Errorf("controlstore: connection %q disappeared immediately after it was written", name)
	}
	return connectionFrom(written), nil
}

// Delete removes a connection.
//
// It does not touch the Secret the connection references. kelson did not create
// that Secret for a token connection, and deleting somebody else's object on the
// way out is not this call's to do (gitconnection.proto says so on the RPC).
func (s *GitConnectionStore) Delete(ctx context.Context, name string, opts DeleteConnectionOptions) error {
	if err := validSegment("connection", name); err != nil {
		return err
	}
	current, err := s.read(ctx, name)
	if err != nil {
		return err
	}
	if current == nil {
		if opts.IdempotencyKey != "" {
			return nil
		}
		return notStoredConnection(name)
	}

	delOpts := []client.DeleteOption{}
	if opts.DryRun {
		delOpts = append(delOpts, client.DryRunAll)
	}
	switch err := s.client.Delete(ctx, current, delOpts...); {
	case apierrors.IsNotFound(err):
		return nil // Someone else deleted it; the requested end state holds.
	case err != nil:
		return fmt.Errorf("controlstore: delete %s: %w", connectionRef(name), err)
	}
	return nil
}

// UpdateStatus records what a probe observed, on the status subresource.
//
// It is a read-modify-write against the live object rather than an apply, for
// [EnvironmentStore]'s reason one kind over: an apply states a complete intent
// for the fields its manager owns, so applying a status-only object under
// [FieldManager] would delete the spec that manager wrote. The retry loop is
// what makes two probes racing each other end with one of them recorded rather
// than with a 409 surfaced to whoever pressed the button second.
func (s *GitConnectionStore) UpdateStatus(ctx context.Context, name string, obs ConnectionObservation) (StoredConnection, error) {
	if err := validSegment("connection", name); err != nil {
		return StoredConnection{}, err
	}
	var last error
	for range writeAttempts {
		current, err := s.read(ctx, name)
		if err != nil {
			return StoredConnection{}, err
		}
		if current == nil {
			return StoredConnection{}, notStoredConnection(name)
		}

		current.Status.ObservedGeneration = current.Generation
		current.Status.Account = obs.Account
		current.Status.Repositories = obs.Repositories
		setConnectionCondition(&current.Status.Conditions, current.Generation,
			v1alpha1.ConditionReady, obs.Ready,
			reasonOr(obs.ReadyReason, obs.Ready, ReasonConnectionReady, ReasonSecretMissing), obs.Message)
		setConnectionCondition(&current.Status.Conditions, current.Generation,
			v1alpha1.ConditionReachable, obs.Reachable,
			reasonOr(obs.ReachableReason, obs.Reachable, ReasonReachable, ReasonUnreachable), obs.Message)

		err = s.client.Status().Update(ctx, current)
		if err == nil {
			return connectionFrom(current), nil
		}
		if !apierrors.IsConflict(err) {
			return StoredConnection{}, fmt.Errorf("controlstore: write the status of %s: %w", connectionRef(name), err)
		}
		last = err
	}
	return StoredConnection{}, withCause(VersionConflict(connectionRef(name),
		fmt.Sprintf("connection %q kept changing while the probe result was being recorded", name),
		"retry the probe: something else is writing this connection's status"), last)
}

// RecordInstallation writes the installation ID a GitHub `installation`
// webhook reported onto an app connection's spec (ADR-0033 decision 2 step 3).
//
// # Why this exists when there is no UpdateConnection
//
// The header above says the vocabulary is create and delete, because a
// connection's host and Secret are what it *is*. This does not contradict that:
// the installation ID is not something the author writes. The app-manifest flow
// creates the app and the connection before anybody installs it, so
// `installationID: 0` is a real intermediate state that only the forge can end,
// and the alternative to recording it here is a connection that can never mint
// a token and a user told to type a number GitHub showed them once.
//
// It writes exactly that one field, through the same read-modify-write retry
// [GitConnectionStore.UpdateStatus] uses and for the same reason — an apply
// under [FieldManager] would state a complete intent and prune whatever else
// the author wrote. A connection that is not app-authenticated is refused
// rather than silently ignored: a token connection receiving an installation
// event means the webhook matched the wrong connection, which is worth a
// sentence.
func (s *GitConnectionStore) RecordInstallation(ctx context.Context, name string, installationID int64) (StoredConnection, error) {
	if err := validSegment("connection", name); err != nil {
		return StoredConnection{}, err
	}
	var last error
	for range writeAttempts {
		current, err := s.read(ctx, name)
		if err != nil {
			return StoredConnection{}, err
		}
		if current == nil {
			return StoredConnection{}, notStoredConnection(name)
		}
		app := current.Spec.Auth.GitHubApp
		if app == nil {
			return StoredConnection{}, NotFound(connectionRef(name),
				fmt.Sprintf("connection %q does not authenticate as a GitHub App, so it has no installation to record", name),
				"an installation belongs to an app connection. A token connection receiving one means a delivery "+
					"verified against the wrong secret, which is worth looking at rather than recording")
		}
		if app.InstallationID == installationID {
			// The same event twice writes the same number; not writing it at
			// all is what keeps a redelivery from bumping the generation and
			// re-triggering every watcher of this object.
			return connectionFrom(current), nil
		}
		app.InstallationID = installationID

		err = s.client.Update(ctx, current, client.FieldOwner(FieldManager))
		if err == nil {
			return connectionFrom(current), nil
		}
		if !apierrors.IsConflict(err) {
			return StoredConnection{}, fmt.Errorf("controlstore: record the installation of %s: %w", connectionRef(name), err)
		}
		last = err
	}
	return StoredConnection{}, withCause(VersionConflict(connectionRef(name),
		fmt.Sprintf("connection %q kept changing while its installation was being recorded", name),
		"retry: something else is writing this connection"), last)
}

// ReadAuthSecret reads the material the connection references and registers
// every value with internal/redact before returning it.
//
// It takes a [StoredConnection] rather than a name so the caller has already
// had to read the connection — there is no path from a bare string to a Secret
// value here — and it returns a [AuthMaterial] rather than the Secret, so a
// caller cannot stumble into a key it had no business reading.
//
// A missing Secret and a Secret missing the key are both `store/not-found`,
// naming the object and the key. That is the connection's Ready condition going
// false with a reason an operator can act on, which is the whole reason
// ADR-0033 keeps the reference and the material in separate objects with
// separate lifecycles.
func (s *GitConnectionStore) ReadAuthSecret(ctx context.Context, conn StoredConnection) (AuthMaterial, error) {
	ref := connectionRef(conn.Name)
	app, token := conn.Spec.Auth.GitHubApp, conn.Spec.Auth.Token
	switch {
	case app == nil && token == nil:
		return AuthMaterial{}, NotFound(ref,
			fmt.Sprintf("connection %q references no credential: neither auth.githubApp nor auth.token is set", conn.Name),
			"delete it and create one that names a Secret — a connection that authenticates with neither "+
				"reaches nothing an anonymous clone could not")
	case app != nil && token != nil:
		return AuthMaterial{}, NotFound(ref,
			fmt.Sprintf("connection %q sets both auth.githubApp and auth.token", conn.Name),
			"keep one: kelson would otherwise have to pick which credential it acts as without the author knowing which")
	}

	name := ""
	if app != nil {
		name = app.SecretRef
	} else {
		name = token.SecretRef
	}
	if name == "" {
		return AuthMaterial{}, NotFound(ref,
			fmt.Sprintf("connection %q names no Secret", conn.Name),
			"set auth.token.secretRef (or auth.githubApp.secretRef) to the name of a Secret in "+s.namespace)
	}

	var secret corev1.Secret
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &secret)
	if apierrors.IsNotFound(err) {
		return AuthMaterial{}, NotFound(ref,
			fmt.Sprintf("connection %q references Secret %s/%s, which does not exist", conn.Name, s.namespace, name),
			fmt.Sprintf("create it — `kubectl -n %s create secret generic %s --from-literal=%s=…` — or point the "+
				"connection at the Secret that already holds the credential. The two objects have separate "+
				"lifecycles, so either order works", s.namespace, name, model.TokenKey))
	}
	if err != nil {
		return AuthMaterial{}, fmt.Errorf("controlstore: read Secret %s/%s for %s: %w", s.namespace, name, ref, err)
	}

	// Registration happens before any value is examined, let alone returned: a
	// key that turns out to be missing must not make the ones that were present
	// printable on the way out through the error below (issue #117).
	material := AuthMaterial{SecretRef: name}
	for _, key := range []string{model.TokenKey, model.GitHubAppPrivateKeyKey, model.GitHubAppWebhookSecretKey} {
		if v := secret.Data[key]; len(v) > 0 {
			redact.Register(string(v))
		}
	}
	material.Token = string(secret.Data[model.TokenKey])
	material.Username = string(secret.Data[model.TokenUsernameKey])
	material.PrivateKeyPEM = secret.Data[model.GitHubAppPrivateKeyKey]
	material.WebhookSecret = secret.Data[model.GitHubAppWebhookSecretKey]

	// The username is not registered: it is an identity, it is displayed
	// deliberately, and scrubbing an account name out of unrelated text helps
	// nobody (forge.Credential.SecretValues draws the same line).
	if want := requiredKey(app != nil); !material.holds(want) {
		return AuthMaterial{}, NotFound(ref,
			fmt.Sprintf("Secret %s/%s holds no %q, which is the key a %s connection authenticates with",
				s.namespace, name, want, authShape(app != nil)),
			fmt.Sprintf("add the key: `kubectl -n %s patch secret %s -p '{\"stringData\":{\"%s\":\"…\"}}'`",
				s.namespace, name, want))
	}
	return material, nil
}

// requiredKey is the one key an auth shape cannot work without. The webhook
// secret is not one: an app connection with no webhook secret can still mint
// tokens and clone, it just cannot verify deliveries, which degrades to polling
// (ADR-0033's consequence about an instance behind NAT).
func requiredKey(app bool) string {
	if app {
		return model.GitHubAppPrivateKeyKey
	}
	return model.TokenKey
}

func authShape(app bool) string {
	if app {
		return "GitHub App"
	}
	return "token"
}

// holds reports whether the material carries one key. It is a method so the
// check reads as a question about the material rather than about the map it
// came out of, which no longer exists by this point.
func (m AuthMaterial) holds(key string) bool {
	switch key {
	case model.GitHubAppPrivateKeyKey:
		return len(m.PrivateKeyPEM) > 0
	default:
		return m.Token != ""
	}
}

// read loads one connection, reporting a missing one as (nil, nil) so every
// caller decides for itself what absence means.
func (s *GitConnectionStore) read(ctx context.Context, name string) (*v1alpha1.GitConnection, error) {
	var conn v1alpha1.GitConnection
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &conn)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("controlstore: read %s: %w", connectionRef(name), err)
	}
	return &conn, nil
}

// create is the write primitive, and it is a plain Create rather than
// [SpecStore.apply]'s server-side apply.
//
// The spec store applies because a Put is convergent: the same project written
// twice is one object stated twice. A connection is not — there is no
// UpdateConnection, so the only write is the first one — and an apply would
// turn the race this store exists to refuse into a silent overwrite. Create
// gets `AlreadyExists` from the API server instead, which is the same answer
// the read above gives and the one this handler can act on.
//
// It also narrows the grant the chart asks for: `create` on gitconnections,
// where an apply would need `patch` and therefore the ability to rewrite one.
func (s *GitConnectionStore) create(ctx context.Context, obj *v1alpha1.GitConnection, dryRun bool) error {
	opts := []client.CreateOption{client.FieldOwner(FieldManager)}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	return s.client.Create(ctx, obj, opts...)
}

// connectionFrom projects a custom resource onto the store's view.
func connectionFrom(conn *v1alpha1.GitConnection) StoredConnection {
	out := StoredConnection{
		Name:       conn.Name,
		Spec:       conn.Spec,
		Version:    conn.ResourceVersion,
		Generation: conn.Generation,
		Status: ConnectionStatus{
			Account:            conn.Status.Account,
			Repositories:       conn.Status.Repositories,
			ObservedGeneration: conn.Status.ObservedGeneration,
		},
	}
	ready := meta.FindStatusCondition(conn.Status.Conditions, v1alpha1.ConditionReady)
	reachable := meta.FindStatusCondition(conn.Status.Conditions, v1alpha1.ConditionReachable)
	out.Status.Observed = ready != nil || reachable != nil
	if ready != nil {
		out.Status.Ready = ready.Status == metav1.ConditionTrue
		out.Status.Message = ready.Message
	}
	if reachable != nil {
		out.Status.Reachable = reachable.Status == metav1.ConditionTrue
		// The Reachable message wins when both carry one: Ready is about the
		// document and the Secret, which a reader can check themselves, and
		// Reachable is the half only the forge could answer.
		if reachable.Message != "" {
			out.Status.Message = reachable.Message
		}
	}
	return out
}

// setConnectionCondition stamps one condition, leaving LastTransitionTime alone
// when only the message changed (meta.SetStatusCondition's behaviour, and the
// reason internal/controller's setReady uses it).
func setConnectionCondition(conditions *[]metav1.Condition, generation int64, kind string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               kind,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// reasonOr picks the reason a caller supplied, or the default for the outcome.
// A condition with no reason is a condition nothing can branch on, so there is
// no path here that leaves one empty.
func reasonOr(given string, ok bool, whenTrue, whenFalse string) string {
	switch {
	case given != "":
		return given
	case ok:
		return whenTrue
	default:
		return whenFalse
	}
}

func connectionLabels(name string) map[string]string {
	return map[string]string{
		labelManagedBy:  managedByKelson,
		labelConnection: name,
		labelState:      stateConnection,
	}
}

func notStoredConnection(name string) Error {
	return NotFound(connectionRef(name),
		fmt.Sprintf("no connection named %q is stored", name),
		"list the connections with ListConnections, or create one with CreateConnection")
}

// connectionRef is the resource identity carried on errors. It names the
// connection rather than the custom resource, matching [specRef]: which object
// holds a connection is an implementation detail, and the name is what a
// project's `source.connection` says.
func connectionRef(name string) string { return "connection/" + name }
