package serverstate

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/redact"
)

// The agent identity store (issue #74, ADR-0023).
//
// # An identity is cluster state, like everything else here
//
// One Secret per identity in the state namespace, so revocation is a write
// every replica sees immediately and a restart forgets nothing (ADR-0013 §1).
// A Secret rather than the ConfigMap the specs and history use: what is stored
// is not a credential — it is a salted HMAC of one, and no token can be
// recovered from it — but identity material belongs behind whatever RBAC an
// operator puts on Secrets, and the server already needs Secret permissions
// for ADR-0009's cluster backend, so the tighter home costs nothing.
//
// # The token is hashed, not encrypted, and not with a password KDF
//
// A token is 256 bits from crypto/rand, not a human-chosen password, so the
// offline-guessing resistance argon2 or bcrypt buys is resistance to an attack
// that cannot succeed: there is no dictionary for a uniformly random 256-bit
// value. What their cost would buy instead is a deliberate CPU burn on every
// single request — revocation must be immediate, so every request re-reads the
// identity and re-verifies the credential, with no cached allow. HMAC-SHA256
// under a per-identity random salt gives a constant-time compare and no shared
// precomputation across identities, at a price a per-request check can pay.
//
// # What the store does not decide
//
// Expiry and revocation are recorded here and are read back verbatim.
// [AgentStore.Authenticate] proves the credential and nothing else — whether an
// expired or revoked identity may act is an authorization question, and it is
// answered once, in internal/api, so the wire error for it is written in one
// place.

// Agent identity codes. Like every other Code in this package they pass through
// to the wire verbatim (ADR-0013 §2), so agents branch on these strings.
const (
	// ErrAgentExists is a create against a name already issued.
	ErrAgentExists Code = "agent/exists"

	// ErrAgentCredential is a bearer token that names no identity or does not
	// match the identity it names. The two cases are deliberately one code: a
	// caller learning which agent names exist from a failed authentication is
	// an enumeration oracle, and there is nothing a legitimate caller does
	// differently for the two.
	ErrAgentCredential Code = "agent/invalid-credential"

	// ErrAgentScope is an issuance the store refuses: no operation class, a
	// TTL past the maximum, a name that is not a DNS-1123 label.
	ErrAgentScope Code = "agent/invalid-scope"
)

// AsAgentExists reports whether err is an agent/exists.
func AsAgentExists(err error) bool { return hasCode(err, ErrAgentExists) }

// AsAgentCredential reports whether err is an agent/invalid-credential.
func AsAgentCredential(err error) bool { return hasCode(err, ErrAgentCredential) }

// AsAgentScope reports whether err is an agent/invalid-scope.
func AsAgentScope(err error) bool { return hasCode(err, ErrAgentScope) }

func agentError(code Code, name, msg, remediation string) Error {
	return newError(code, "agent/"+name, msg, remediation)
}

// Operation is a class of RPC an agent credential may perform. The classes are
// coarse on purpose (ADR-0023): an agent's blast radius is "can it change
// anything", and a per-method matrix is a policy language nobody could audit.
type Operation string

const (
	// OpRead is state a caller may look at.
	OpRead Operation = "read"
	// OpMutate is state a caller may change. It implies [OpRead].
	OpMutate Operation = "mutate"
	// OpAdmin is issuing, listing and revoking agent identities. It is never
	// granted to an agent credential — the constant exists so the RPC table in
	// internal/api can name the class those methods belong to.
	OpAdmin Operation = "admin"
)

// Scope bounds what a credential may reach. An empty Projects or Environments
// list means every one, which is why [AgentStore.Create] refuses an empty
// Operations list rather than reading it the same way: two of the three fields
// widen by omission and the third must not.
type Scope struct {
	Projects     []string
	Environments []string
	Operations   []Operation
}

// Restricted reports whether the scope names any project or environment. An
// unrestricted scope may reach the RPCs whose target cannot be derived from the
// request; a restricted one may not, because there is nothing to check it
// against (ADR-0023).
func (s Scope) Restricted() bool { return len(s.Projects) > 0 || len(s.Environments) > 0 }

// AllowsProject reports whether project is in scope. An empty project name is
// refused for a project-restricted scope: it is "every project", not "none".
func (s Scope) AllowsProject(project string) bool { return contains(s.Projects, project) }

// AllowsEnvironment reports whether environment is in scope.
func (s Scope) AllowsEnvironment(environment string) bool {
	return contains(s.Environments, environment)
}

// Allows reports whether the scope grants an operation class. OpMutate implies
// OpRead — a credential that may deploy may obviously read what it deployed —
// and nothing implies OpAdmin.
func (s Scope) Allows(op Operation) bool {
	for _, granted := range s.Operations {
		if granted == op {
			return true
		}
		if op == OpRead && granted == OpMutate {
			return true
		}
	}
	return false
}

// contains is the membership rule both scope dimensions share: an empty list is
// every value, and a named list needs an exact match. An empty candidate never
// matches a named list, so "the request did not say" is never mistaken for "the
// request said something allowed".
func contains(list []string, value string) bool {
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Limit is one identity's request budget, enforced in the server as a token
// bucket (ADR-0023 §6).
type Limit struct {
	RequestsPerMinute int
	Burst             int
}

// Agent is one identity as the store holds it. It carries no credential: the
// token exists only in the response of the [AgentStore.Create] that minted it.
type Agent struct {
	Name      string
	Created   time.Time
	Expires   time.Time
	Revoked   bool
	RevokedAt time.Time
	Scope     Scope
	Limit     Limit
	// Version is the Secret's resourceVersion, opaque to callers, used by
	// Revoke's optimistic-concurrency check.
	Version string
}

// Expired reports whether the identity's lifetime has run out at now.
func (a Agent) Expired(now time.Time) bool {
	return !a.Expires.IsZero() && !now.Before(a.Expires)
}

// AgentSpec is one issuance request.
type AgentSpec struct {
	Name  string
	TTL   time.Duration
	Scope Scope
	Limit Limit
}

// Issuance defaults and bounds (ADR-0023 §5).
const (
	// DefaultAgentTTL is a working day's worth of credential. Short by design:
	// rotation is create-then-revoke, and a default measured in months would
	// make expiry a formality rather than a bound.
	DefaultAgentTTL = 24 * time.Hour

	// MaxAgentTTL is the ceiling. A request above it is refused rather than
	// clamped: an operator who asked for a year must learn it did not get one.
	MaxAgentTTL = 30 * 24 * time.Hour

	// DefaultRequestsPerMinute and DefaultBurst are the request budget an
	// issuance that names none receives. They are generous enough for an agent
	// polling a deployment and small enough that a runaway loop is capped.
	DefaultRequestsPerMinute = 120
	DefaultBurst             = 30

	// MaxRequestsPerMinute bounds what an issuance may ask for, for the same
	// reason MaxAgentTTL does.
	MaxRequestsPerMinute = 6000
)

// AgentTokenPrefix marks a bearer credential as an agent token rather than the
// shared password. Without it the server would have to try every identity's
// HMAC against every bearer header, and a mistyped password would be
// indistinguishable from a mistyped token in the failure it produced.
const AgentTokenPrefix = "kagt."

// agentSecretBytes is the credential's entropy. 32 bytes is the size of the
// HMAC that verifies it; more would be stored no more securely and less would
// be the only guessable part of the design.
const agentSecretBytes = 32

// Secret layout for an identity.
const (
	agentNamePrefix = "kelson-agent-"
	stateAgent      = "agent"
	labelAgent      = "kelson.dev/agent"

	// agentSecretType marks the object as kelson's rather than an arbitrary
	// Secret that happens to sit in the namespace. It is the same guard
	// internal/secret's managed-secret label is: the store refuses to decode or
	// delete anything it did not write.
	agentSecretType corev1.SecretType = "kelson.dev/agent-identity"

	keyCreated      = "created"
	keyExpires      = "expires"
	keyRevokedAt    = "revokedAt"
	keyProjects     = "projects"
	keyEnvironments = "environments"
	keyOperations   = "operations"
	keyRPM          = "requestsPerMinute"
	keyBurst        = "burst"
	keySalt         = "salt"
	keyHash         = "hash"
)

// AgentStoreOptions configures an [AgentStore].
type AgentStoreOptions struct {
	// Client is the typed client for the namespace the server runs against.
	Client kubernetes.Interface
	// Namespace is where state objects live.
	Namespace string
	// Now is the clock. Nil selects time.Now.
	Now func() time.Time
}

// AgentStore is the cluster-backed store of agent identities.
type AgentStore struct {
	client    kubernetes.Interface
	namespace string
	now       func() time.Time
}

// NewAgentStore returns a store over Secrets in one namespace.
func NewAgentStore(opts AgentStoreOptions) (*AgentStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("serverstate: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("serverstate: a namespace is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &AgentStore{client: opts.Client, namespace: opts.Namespace, now: now}, nil
}

// Create mints an identity and returns it with its token. The token is the only
// time the credential exists outside the caller's hands: what the store keeps is
// a salted HMAC, so this value cannot be recovered from the cluster afterwards.
//
// It is registered with internal/redact before it is returned, so from this
// moment on it cannot appear in a log line or an error message anywhere in the
// process (issue #117).
func (s *AgentStore) Create(ctx context.Context, spec AgentSpec) (Agent, string, error) {
	agent, err := s.validate(spec)
	if err != nil {
		return Agent{}, "", err
	}

	secretBytes := make([]byte, agentSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return Agent{}, "", fmt.Errorf("serverstate: generating an agent credential: %w", err)
	}
	salt := make([]byte, agentSecretBytes)
	if _, err := rand.Read(salt); err != nil {
		return Agent{}, "", fmt.Errorf("serverstate: generating an agent credential salt: %w", err)
	}
	token := AgentTokenPrefix + agent.Name + "." + base64.RawURLEncoding.EncodeToString(secretBytes)
	redact.Register(token)

	object := s.secret(agent, salt, agentHash(salt, secretBytes))
	if _, err := s.client.CoreV1().Secrets(s.namespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return Agent{}, "", agentError(ErrAgentExists, agent.Name,
				fmt.Sprintf("an agent identity named %q already exists", agent.Name),
				"choose another name, or revoke the existing identity first; rotation is create-then-revoke, so the successor needs its own name")
		}
		return Agent{}, "", fmt.Errorf("serverstate: create agent %q: %w", agent.Name, err)
	}
	return agent, token, nil
}

// validate resolves an issuance against the defaults and the bounds. It is
// separate from Create so the refusals are readable as one list.
func (s *AgentStore) validate(spec AgentSpec) (Agent, error) {
	name := strings.TrimSpace(spec.Name)
	if err := validSegment("agent name", name); err != nil {
		return Agent{}, agentError(ErrAgentScope, name, err.Error(),
			"use a DNS-1123 label: lower-case letters, digits and '-', starting and ending alphanumeric")
	}
	if len(spec.Scope.Operations) == 0 {
		return Agent{}, agentError(ErrAgentScope, name,
			"an agent identity must be granted at least one operation class",
			"grant read, or read and mutate; an empty grant is refused rather than read as 'everything'")
	}
	for _, op := range spec.Scope.Operations {
		switch op {
		case OpRead, OpMutate:
		case OpAdmin:
			return Agent{}, agentError(ErrAgentScope, name,
				"the admin operation class cannot be granted to an agent identity",
				"issue and revoke identities as a human, with `kelson agent` or AgentService; an agent that could mint an agent could mint one wider than itself (ADR-0023)")
		default:
			return Agent{}, agentError(ErrAgentScope, name,
				fmt.Sprintf("unknown operation class %q", op),
				"grant read or mutate")
		}
	}
	for _, project := range spec.Scope.Projects {
		if err := validSegment("scoped project", project); err != nil {
			return Agent{}, agentError(ErrAgentScope, name, err.Error(), "name projects exactly as their specs do")
		}
	}
	for _, environment := range spec.Scope.Environments {
		if err := validSegment("scoped environment", environment); err != nil {
			return Agent{}, agentError(ErrAgentScope, name, err.Error(), "name environments exactly as their specs do")
		}
	}

	ttl := spec.TTL
	switch {
	case ttl == 0:
		ttl = DefaultAgentTTL
	case ttl < 0 || ttl > MaxAgentTTL:
		return Agent{}, agentError(ErrAgentScope, name,
			fmt.Sprintf("a lifetime of %s is outside the permitted range (0 < ttl <= %s)", ttl, MaxAgentTTL),
			"ask for a shorter lifetime and rotate: rotation is create the successor, then revoke this identity")
	}

	limit := spec.Limit
	if limit.RequestsPerMinute == 0 {
		limit.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if limit.Burst == 0 {
		limit.Burst = DefaultBurst
	}
	if limit.RequestsPerMinute < 0 || limit.RequestsPerMinute > MaxRequestsPerMinute ||
		limit.Burst < 0 || limit.Burst > MaxRequestsPerMinute {
		return Agent{}, agentError(ErrAgentScope, name,
			fmt.Sprintf("a budget of %d requests per minute with a burst of %d is outside the permitted range (0 < n <= %d)",
				limit.RequestsPerMinute, limit.Burst, MaxRequestsPerMinute),
			"ask for a smaller budget")
	}

	created := s.now().UTC().Truncate(time.Second)
	return Agent{
		Name:    name,
		Created: created,
		Expires: created.Add(ttl),
		Scope: Scope{
			Projects:     append([]string(nil), spec.Scope.Projects...),
			Environments: append([]string(nil), spec.Scope.Environments...),
			Operations:   append([]Operation(nil), spec.Scope.Operations...),
		},
		Limit: limit,
	}, nil
}

// Get returns one identity. It is the per-request read that makes revocation
// immediate: nothing about an identity is cached anywhere.
func (s *AgentStore) Get(ctx context.Context, name string) (Agent, error) {
	if err := validSegment("agent name", name); err != nil {
		return Agent{}, agentError(ErrAgentCredential, name, "no such agent identity", agentCredentialRemediation)
	}
	object, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, agentSecretName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Agent{}, notAnAgent(name)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("serverstate: read agent %q: %w", name, err)
	}
	agent, _, _, decodeErr := decodeAgent(object)
	if decodeErr != nil {
		return Agent{}, decodeErr
	}
	return agent, nil
}

// List returns every identity, revoked and expired ones included, ordered by
// name. A credential that disappeared on expiry would make "was this action
// taken by an identity that still exists?" unanswerable.
func (s *AgentStore) List(ctx context.Context) ([]Agent, error) {
	list, err := s.client.CoreV1().Secrets(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			labelManagedBy: managedByKelson,
			labelState:     stateAgent,
		}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("serverstate: list agents in %s: %w", s.namespace, err)
	}
	out := make([]Agent, 0, len(list.Items))
	for i := range list.Items {
		agent, _, _, err := decodeAgent(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Revoke marks an identity dead. It is idempotent — revoking twice reports the
// same identity rather than failing — and it touches exactly one object, which
// is what makes "revoking an agent affects no human account" a property of the
// storage rather than a promise.
//
// The identity is kept rather than deleted: an audit trail that cannot name the
// principal of a past action is not one (#78).
func (s *AgentStore) Revoke(ctx context.Context, name string) (Agent, error) {
	if err := validSegment("agent name", name); err != nil {
		return Agent{}, notAnAgent(name)
	}
	secrets := s.client.CoreV1().Secrets(s.namespace)
	for attempt := 0; attempt < writeAttempts; attempt++ {
		object, err := secrets.Get(ctx, agentSecretName(name), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return Agent{}, notAnAgent(name)
		}
		if err != nil {
			return Agent{}, fmt.Errorf("serverstate: read agent %q: %w", name, err)
		}
		agent, salt, hash, err := decodeAgent(object)
		if err != nil {
			return Agent{}, err
		}
		if agent.Revoked {
			return agent, nil
		}
		agent.Revoked = true
		agent.RevokedAt = s.now().UTC().Truncate(time.Second)

		updated := s.secret(agent, salt, hash)
		updated.ResourceVersion = object.ResourceVersion
		written, err := secrets.Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue // Something else wrote the identity; re-read and revoke again.
		}
		if err != nil {
			return Agent{}, fmt.Errorf("serverstate: revoke agent %q: %w", name, err)
		}
		agent.Version = written.ResourceVersion
		return agent, nil
	}
	return Agent{}, VersionConflict("agent/"+name,
		fmt.Sprintf("agent %q is being written concurrently and did not settle in %d attempts", name, writeAttempts),
		"retry the revocation")
}

// Authenticate proves a bearer token and returns the identity that owns it. It
// proves the credential and nothing else: expiry and revocation are recorded on
// the returned [Agent] and are the caller's to enforce, so the wire error for
// them is written in one place (internal/api).
//
// Every failure is the same code with the same message. Which of "no such
// identity", "malformed token" and "wrong secret" occurred is not something a
// legitimate caller does anything differently about, and telling them apart
// would make this an oracle for enumerating identity names.
func (s *AgentStore) Authenticate(ctx context.Context, token string) (Agent, error) {
	name, secretBytes, ok := splitAgentToken(token)
	if !ok {
		return Agent{}, invalidCredential()
	}
	object, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, agentSecretName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Agent{}, invalidCredential()
	}
	if err != nil {
		return Agent{}, fmt.Errorf("serverstate: read agent credential: %w", err)
	}
	agent, salt, hash, err := decodeAgent(object)
	if err != nil {
		return Agent{}, err
	}
	if !hmac.Equal(hash, agentHash(salt, secretBytes)) {
		return Agent{}, invalidCredential()
	}
	return agent, nil
}

// splitAgentToken takes `kagt.<name>.<secret>` apart. The name travels in the
// token so authentication is one Get rather than an HMAC against every identity
// the server holds; the secret is what proves the claim, so a forged name buys
// nothing.
func splitAgentToken(token string) (name string, secretBytes []byte, ok bool) {
	rest, found := strings.CutPrefix(token, AgentTokenPrefix)
	if !found {
		return "", nil, false
	}
	name, encoded, found := strings.Cut(rest, ".")
	if !found || validSegment("agent name", name) != nil {
		return "", nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != agentSecretBytes {
		return "", nil, false
	}
	return name, decoded, true
}

// agentHash is the stored proof of a token: HMAC-SHA256 of the secret under the
// identity's own random salt.
func agentHash(salt, secretBytes []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(secretBytes) //nolint:errcheck // hash.Hash never returns an error
	return mac.Sum(nil)
}

// secret builds the object one identity is stored as.
func (s *AgentStore) secret(agent Agent, salt, hash []byte) *corev1.Secret {
	data := map[string][]byte{
		keyCreated:      []byte(agent.Created.UTC().Format(time.RFC3339)),
		keyExpires:      []byte(agent.Expires.UTC().Format(time.RFC3339)),
		keyProjects:     []byte(strings.Join(agent.Scope.Projects, "\n")),
		keyEnvironments: []byte(strings.Join(agent.Scope.Environments, "\n")),
		keyOperations:   []byte(joinOperations(agent.Scope.Operations)),
		keyRPM:          []byte(strconv.Itoa(agent.Limit.RequestsPerMinute)),
		keyBurst:        []byte(strconv.Itoa(agent.Limit.Burst)),
		keySalt:         salt,
		keyHash:         hash,
	}
	if agent.Revoked {
		data[keyRevokedAt] = []byte(agent.RevokedAt.UTC().Format(time.RFC3339))
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentSecretName(agent.Name),
			Namespace: s.namespace,
			Labels: map[string]string{
				labelManagedBy: managedByKelson,
				labelState:     stateAgent,
				labelAgent:     agent.Name,
			},
		},
		Type: agentSecretType,
		Data: data,
	}
}

// decodeAgent is the inverse of secret. It returns the salt and hash beside the
// identity so Revoke can rewrite the object without re-minting the credential.
func decodeAgent(object *corev1.Secret) (Agent, []byte, []byte, error) {
	name := object.Labels[labelAgent]
	if object.Type != agentSecretType || name == "" {
		return Agent{}, nil, nil, fmt.Errorf("serverstate: Secret %s/%s is not a kelson agent identity",
			object.Namespace, object.Name)
	}
	agent := Agent{
		Name:    name,
		Version: object.ResourceVersion,
		Scope: Scope{
			Projects:     splitList(string(object.Data[keyProjects])),
			Environments: splitList(string(object.Data[keyEnvironments])),
			Operations:   splitOperations(string(object.Data[keyOperations])),
		},
		Limit: Limit{
			RequestsPerMinute: atoi(string(object.Data[keyRPM])),
			Burst:             atoi(string(object.Data[keyBurst])),
		},
	}
	var err error
	if agent.Created, err = parseStamp(object, keyCreated); err != nil {
		return Agent{}, nil, nil, err
	}
	if agent.Expires, err = parseStamp(object, keyExpires); err != nil {
		return Agent{}, nil, nil, err
	}
	if raw, present := object.Data[keyRevokedAt]; present && len(raw) > 0 {
		if agent.RevokedAt, err = parseStamp(object, keyRevokedAt); err != nil {
			return Agent{}, nil, nil, err
		}
		agent.Revoked = true
	}
	return agent, object.Data[keySalt], object.Data[keyHash], nil
}

func parseStamp(object *corev1.Secret, key string) (time.Time, error) {
	raw, present := object.Data[key]
	if !present || len(raw) == 0 {
		return time.Time{}, fmt.Errorf("serverstate: agent identity %s/%s has no %s", object.Namespace, object.Name, key)
	}
	stamp, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("serverstate: agent identity %s/%s has an unreadable %s: %w",
			object.Namespace, object.Name, key, err)
	}
	return stamp.UTC(), nil
}

func splitList(joined string) []string {
	if strings.TrimSpace(joined) == "" {
		return nil
	}
	return strings.Split(joined, "\n")
}

func joinOperations(ops []Operation) string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return strings.Join(out, "\n")
}

func splitOperations(joined string) []Operation {
	parts := splitList(joined)
	if len(parts) == 0 {
		return nil
	}
	out := make([]Operation, 0, len(parts))
	for _, part := range parts {
		out = append(out, Operation(part))
	}
	return out
}

// atoi reads a stored integer, treating an unreadable one as zero. The store
// wrote it, so an unparseable value means the object was edited by hand; zero
// then selects the server's default budget rather than an unbounded one.
func atoi(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

const agentCredentialRemediation = "issue a credential with `kelson agent create` (or AgentService.CreateAgent) and send it as " +
	"Authorization: Bearer <token>; a token cannot be recovered after issuance, so a lost one is replaced rather than found"

func invalidCredential() Error {
	return agentError(ErrAgentCredential, "credential",
		"the bearer token does not identify a live agent identity",
		agentCredentialRemediation)
}

func notAnAgent(name string) Error {
	return NotFound("agent/"+name,
		fmt.Sprintf("no agent identity named %q is stored", name),
		"list the identities with `kelson agent list` (or AgentService.ListAgents)")
}

func agentSecretName(name string) string { return agentNamePrefix + name }
