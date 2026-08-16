package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/secret"
)

// SecretService served: write the Secrets a spec's references point at, list
// them masked, delete the ones kelson manages (issue #116, ADR-0009).
//
// # The first statement of SetSecret is the one that matters
//
// Every value the request carries is handed to internal/redact before anything
// else happens to it — before the seam check, before validation, before the
// store is called. From that point the value cannot appear in a log line, in an
// error message, in a structured error detail or in a Settled event, whichever
// plane writes it, because errors.go runs every free-text field of every wire
// error through the same registry (issue #117).
//
// internal/secret registers them again when it receives them. That is
// deliberate duplication, not an oversight: this handler can fail before it
// ever reaches the store, and a value that reached the process but not the
// registry is exactly the window #117 exists to close.
//
// # Nothing in this file can return a value
//
// The responses carry secret.Secret, which has no field a value could be in.
// So "List masks the values" needs no masking step here and cannot be
// regressed by a future edit to this file: there is nothing to forget to call.
//
// # The environment's backend decides whether a mutation may happen at all
//
// A request here carries no spec, so until issue #269 these handlers wrote a
// cluster Secret whichever backend the environment had selected — under `sops`
// into a place the delivery spine would fight over, which docs/secrets.md
// recorded as a limitation rather than a bug. [Server.writableBackend] closes
// that: the effective `secrets.backend` is read off the stored spec, and a
// backend kelson must not write from here is refused by name before the store
// is reached.

// SetSecret writes keys into a Secret in the environment's namespace.
//
// The dry-run ladder means what #69 says it means. RENDER validates the request
// and opens no connection — a render dry run that talked to a cluster to answer
// would not be one — and SERVER is a real Kubernetes server-side dry-run apply:
// admission runs, the merge against the live object is computed, and nothing is
// persisted.
func (s *Server) SetSecret(ctx context.Context, req *connect.Request[kelsonv1alpha1.SetSecretRequest]) (*connect.Response[kelsonv1alpha1.SetSecretResponse], error) {
	msg := req.Msg
	for _, value := range msg.GetValues() {
		redact.Register(value)
	}
	auditDryRun(ctx, msg.GetDryRun())

	request := secret.SetRequest{
		Target: secretTarget(msg.GetTarget()),
		Name:   msg.GetName(),
		Values: msg.GetValues(),
		DryRun: msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}
	namespace, err := request.Validate()
	if err != nil {
		return nil, failSecret(err)
	}
	// Agent policy (ADR-0025). A secret write is a change to the environment's
	// live state — the pods read the value — so propose-only refuses it, and a
	// request that addresses a bare namespace instead of a (project,
	// environment) is refused too: policy lives on the environment, and there
	// is no environment in "namespace: shop-production" that kelson may assume.
	if persists(msg.GetDryRun()) {
		if _, err := s.guard(ctx, model.AgentOpSecretSet, request.Project, request.Environment); err != nil {
			return nil, err
		}
	}
	// The backend check runs on every rung, RENDER included: a validation that
	// answered "this would write" for an environment whose values belong
	// somewhere else would be a preview of something that is never going to
	// happen, and RENDER is exactly what an agent sends to find out.
	if err := s.writableBackend(ctx, request.Target); err != nil {
		return nil, failSecret(err)
	}
	// The seam check comes after the guard on purpose. Both answers are true on
	// a server with no secret backend, and "you may not" is the one that does
	// not depend on how this server happens to be wired — an agent must not
	// learn that a rule does not apply to it by asking a server that could not
	// have obeyed it anyway. The backend refusal above is ordered ahead of it
	// for the same reason: which backend an environment uses is a fact about
	// the spec, true on every server that reads it.
	if s.secrets == nil {
		return nil, unimplemented("the secret backend")
	}

	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		// Keys only, and deliberately no `secret.keys`: kelson did not look, so
		// reporting what the Secret holds would be a claim about a cluster this
		// call never opened a connection to.
		return connect.NewResponse(&kelsonv1alpha1.SetSecretResponse{
			Secret:      &kelsonv1alpha1.SecretSummary{Name: request.Name, Namespace: namespace},
			WrittenKeys: request.Keys(),
			DryRun:      true,
		}), nil
	}

	written, err := s.secrets.Set(ctx, request)
	if err != nil {
		return nil, failSecret(err)
	}
	// The record counts keys and names the kind. It cannot carry a value —
	// there is no field for one here, the values went to internal/redact on
	// entry, and the store scrubs every free-text field it accepts (#117).
	auditChange(ctx, controlstore.AuditChange{
		Source:    controlstore.ChangeFromRendered,
		Resources: len(request.Keys()),
		Kinds:     []string{"Secret"},
	})
	return connect.NewResponse(&kelsonv1alpha1.SetSecretResponse{
		Secret:      wireSecret(written),
		WrittenKeys: request.Keys(),
		DryRun:      request.DryRun,
	}), nil
}

// UnsetSecret removes named keys from a Secret kelson manages (issue #269).
//
// It is filed under the `secret-delete` agent operation rather than a word of
// its own: `policy.forbid` is the model plane's vocabulary (model.AgentOperations)
// and coarse by design, and an operator who wrote `forbid: [secret-delete]` has
// said agents may not take credentials away here. A key removal that slipped
// past that because it is spelled differently would make the rule advisory.
//
// No value travels in either direction: the request names keys, and the
// response is the same masked read-back every other RPC here returns.
func (s *Server) UnsetSecret(ctx context.Context, req *connect.Request[kelsonv1alpha1.UnsetSecretRequest]) (*connect.Response[kelsonv1alpha1.UnsetSecretResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())

	request := secret.UnsetRequest{
		Target: secretTarget(msg.GetTarget()),
		Name:   msg.GetName(),
		Keys:   msg.GetKeys(),
		DryRun: msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}
	namespace, err := request.Validate()
	if err != nil {
		return nil, failSecret(err)
	}
	if persists(msg.GetDryRun()) {
		if _, err := s.guard(ctx, model.AgentOpSecretDelete, request.Project, request.Environment); err != nil {
			return nil, err
		}
	}
	if err := s.writableBackend(ctx, request.Target); err != nil {
		return nil, failSecret(err)
	}
	if s.secrets == nil {
		return nil, unimplemented("the secret backend")
	}

	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		// No `secret.keys`, and here the omission is doing more work than it
		// does for SetSecret: whether the named keys are even in the Secret is
		// the removal's whole precondition, and it cannot be answered without
		// reading the cluster. A RENDER rung reports what was asked for and
		// nothing about what is there.
		return connect.NewResponse(&kelsonv1alpha1.UnsetSecretResponse{
			Secret:      &kelsonv1alpha1.SecretSummary{Name: request.Name, Namespace: namespace},
			RemovedKeys: request.RemovedKeys(),
			DryRun:      true,
		}), nil
	}

	left, err := s.secrets.Unset(ctx, request)
	if err != nil {
		return nil, failSecret(err)
	}
	auditChange(ctx, controlstore.AuditChange{
		Source:    controlstore.ChangeFromRendered,
		Resources: len(request.RemovedKeys()),
		Kinds:     []string{"Secret"},
	})
	return connect.NewResponse(&kelsonv1alpha1.UnsetSecretResponse{
		Secret:      wireSecret(left),
		RemovedKeys: request.RemovedKeys(),
		DryRun:      request.DryRun,
	}), nil
}

// ListSecrets reports the Secrets kelson manages in the environment's
// namespace: names, keys, ages. Never a value — there is nowhere to put one.
func (s *Server) ListSecrets(ctx context.Context, req *connect.Request[kelsonv1alpha1.ListSecretsRequest]) (*connect.Response[kelsonv1alpha1.ListSecretsResponse], error) {
	if s.secrets == nil {
		return nil, unimplemented("the secret backend")
	}
	target := secretTarget(req.Msg.GetTarget())
	namespace, err := target.Resolve()
	if err != nil {
		return nil, failSecret(err)
	}
	secrets, err := s.secrets.List(ctx, target)
	if err != nil {
		return nil, failSecret(err)
	}
	out := make([]*kelsonv1alpha1.SecretSummary, 0, len(secrets))
	for _, item := range secrets {
		out = append(out, wireSecret(item))
	}
	return connect.NewResponse(&kelsonv1alpha1.ListSecretsResponse{Secrets: out, Namespace: namespace}), nil
}

// DeleteSecret removes a Secret kelson manages. One that kelson did not write
// is `secret/not-managed` — a failed precondition rather than an invalid
// argument, because the request is well-formed and the world is what refuses
// it.
func (s *Server) DeleteSecret(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeleteSecretRequest]) (*connect.Response[kelsonv1alpha1.DeleteSecretResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	request := secret.DeleteRequest{
		Target: secretTarget(msg.GetTarget()),
		Name:   msg.GetName(),
		DryRun: msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}
	namespace, err := request.Validate()
	if err != nil {
		return nil, failSecret(err)
	}
	if persists(msg.GetDryRun()) {
		if _, err := s.guard(ctx, model.AgentOpSecretDelete, request.Project, request.Environment); err != nil {
			return nil, err
		}
	}
	// A delete is gated on the backend for the same reason a write is, and the
	// failure it prevents is the more confusing one: under `sops` the Secret in
	// the namespace is something the delivery spine applied, so deleting it
	// here removes an object that comes straight back on the next reconcile and
	// tells the caller the credential is gone when it is not.
	if err := s.writableBackend(ctx, request.Target); err != nil {
		return nil, failSecret(err)
	}
	if s.secrets == nil {
		return nil, unimplemented("the secret backend")
	}
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return connect.NewResponse(&kelsonv1alpha1.DeleteSecretResponse{Namespace: namespace}), nil
	}
	if err := s.secrets.Delete(ctx, request); err != nil {
		return nil, failSecret(err)
	}
	auditChange(ctx, controlstore.AuditChange{
		Source:    controlstore.ChangeFromRendered,
		Resources: 1,
		Kinds:     []string{"Secret"},
	})
	return connect.NewResponse(&kelsonv1alpha1.DeleteSecretResponse{
		Deleted:   !request.DryRun,
		Namespace: namespace,
	}), nil
}

// writableBackend refuses a mutation this service must not perform for the
// environment's effective secret backend (issue #269).
//
// Which backend an environment uses is not the request's to state — a
// SecretTarget carries a project, an environment and at most a namespace — so
// it is read off the stored spec. Two of the three backends are refusals here,
// and they are different kinds of refusal:
//
//   - `sops` is a gap with a tracking issue. The value belongs encrypted in the
//     published artifact (ADR-0028 §7) and the writer that used to commit it is
//     deleted (#224), so nothing on kelson's side can put it where the delivery
//     spine reads it. Writing a plain cluster Secret instead is what this
//     refusal exists to stop: kustomize-controller applies the artifact's own
//     Secret over it on the next reconcile, so the value would be silently
//     replaced by whatever was encrypted — or orphaned, if there is no
//     encrypted one. The CLI refuses it in the same voice
//     (cmd/kelson/secret.go's sopsUnavailable).
//   - `externalSecrets` is permanent (ADR-0020). No value passes through
//     kelson at all under it, and secret.ExternalBackend says so and names
//     where the value does belong.
func (s *Server) writableBackend(ctx context.Context, t secret.Target) error {
	backend, err := s.storedSecretBackend(ctx, t)
	if err != nil {
		return err
	}
	switch backend.Backend {
	case model.SecretsSOPS:
		return delivery.NotImplemented("secret",
			"environment "+t.Environment+" selects secret backend sops, and kelson cannot write it: the "+
				"encrypted Secret belongs inside the published artifact (ADR-0028 decision 7) and the writer "+
				"that produced it was deleted with the old delivery machinery. SOPS itself is unaffected — "+
				"encryption, the age recipients and in-cluster decryption are unchanged — what is missing is "+
				"the destination, and a plain Secret written here would be overwritten by the artifact's own "+
				"on the next reconcile",
			"#224")
	case model.SecretsExternalSecrets:
		return secret.ExternalBackend(t, backend.Store)
	default:
		return nil
	}
}

// storedSecretBackend resolves one environment's effective `secrets.backend`
// out of the spec store.
//
// The three answers are [Server.storedPolicy]'s three, for the same reasons: a
// stored environment's backend applies; a project or environment the store does
// not hold has selected nothing, which is `cluster` — the model's own default
// and what this service has always written; and a spec that cannot be read or
// resolved is a refusal, because a backend kelson cannot determine is not the
// same thing as the default one. Guessing `cluster` there is precisely the bug
// #269 is about, one level up.
//
// It resolves rather than reading `spec.secrets` directly: the backend is a P4
// default chain (project `defaults.secrets`, then the Environment's own block)
// and model.Resolve is where that chain lives. `kelson secret` reads the same
// answer the same way (cmd/kelson/secret.go), so the CLI and the server cannot
// disagree about which backend an environment has.
func (s *Server) storedSecretBackend(ctx context.Context, t secret.Target) (model.SecretBackend, error) {
	cluster := model.SecretBackend{Backend: model.SecretsCluster}
	if s.specs == nil {
		return cluster, nil
	}
	stored, err := s.specs.Get(ctx, t.Project)
	if err != nil {
		if controlstore.AsNotFound(err) {
			return cluster, nil
		}
		return cluster, secret.BackendUnreadable(t, "the stored spec could not be read", err)
	}
	spec, err := decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	if err != nil {
		return cluster, secret.BackendUnreadable(t, "the stored spec does not decode", err)
	}
	environment, err := selectEnvironment(spec.environments, t.Environment)
	if err != nil {
		// An environment the stored spec does not declare has selected no
		// backend, exactly like a project the store does not hold. The write
		// still has to be addressed to somewhere real, and the namespace it
		// resolves to is what decides that.
		return cluster, nil
	}
	resolved, errs := model.Resolve(spec.project, environment)
	if len(errs) > 0 {
		return cluster, secret.BackendUnreadable(t, "the stored spec does not resolve", errs)
	}
	return resolved.Environment.Secrets, nil
}

func secretTarget(t *kelsonv1alpha1.SecretTarget) secret.Target {
	return secret.Target{
		Project:     t.GetProject(),
		Environment: t.GetEnvironment(),
		Namespace:   t.GetNamespace(),
	}
}

// wireSecret projects the masked read-back onto the wire. The age is computed
// here rather than left to the client so the CLI, the UI and an agent all read
// the same number without needing a clock synchronised with the cluster's.
func wireSecret(s secret.Secret) *kelsonv1alpha1.SecretSummary {
	out := &kelsonv1alpha1.SecretSummary{
		Name:      s.Name,
		Namespace: s.Namespace,
		Keys:      s.Keys,
	}
	if !s.CreatedAt.IsZero() {
		out.CreatedAt = s.CreatedAt.UTC().Format(time.RFC3339)
		out.AgeSeconds = int64(s.Age(time.Now()).Seconds())
	}
	return out
}
