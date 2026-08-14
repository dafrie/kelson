package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
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
	// The seam check comes after the guard on purpose. Both answers are true on
	// a server with no secret backend, and "you may not" is the one that does
	// not depend on how this server happens to be wired — an agent must not
	// learn that a rule does not apply to it by asking a server that could not
	// have obeyed it anyway.
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
