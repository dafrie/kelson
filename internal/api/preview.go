package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// PreviewService served: which of an environment's pull requests are running
// (ADR-0017 stage 3, reopening its decision 7).
//
// # Three answers, and the response says which one it gave
//
// An environment that declares no `previews:` block gets its identity and
// nothing else — no settings, no lifecycle, no error. That is not a failure and
// the caller's empty state is what explains how previews come to exist.
//
// An environment that declares previews in a delivery mode that cannot run them
// gets its settings and the renderer's own `render/previews-require-flux`. The
// gate is asked for by name (renderer.PreviewsRequireFlux) rather than by
// rendering the whole environment: a render can fail for a dozen unrelated
// reasons — an unresolved image, a missing overlay — and answering "why are
// there no previews" with the first of those would be a worse answer than the
// gate's own sentence.
//
// An environment that declares previews in flux mode gets the cluster's reply.
// The cluster is read through the delivery plane, which is where a Kubernetes
// client is allowed to live (.golangci.yml) and which this request already has
// to build to know the environment's target anyway.
//
// # Nothing here writes, and there is no field that could
//
// Previews are published by CI and torn down by flux-operator (ADR-0017
// decisions 8 and 6). A write RPC would have to duplicate the publisher without
// the checkout and the image reference CI already has, or delete an object the
// operator recreates on its next poll.

// ListPreviews reports the previews an environment has.
func (s *Server) ListPreviews(ctx context.Context, req *connect.Request[kelsonv1alpha1.ListPreviewsRequest]) (*connect.Response[kelsonv1alpha1.ListPreviewsResponse], error) {
	msg := req.Msg
	project, environment, resolved, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return nil, failRequest(err)
	}

	out := &kelsonv1alpha1.ListPreviewsResponse{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		Namespace:   resolved.Environment.Namespace,
		Mode:        string(resolved.Environment.Mode),
	}
	previews := resolved.Environment.Previews
	if previews == nil {
		return connect.NewResponse(out), nil
	}
	out.Settings = wirePreviewSettings(previews)

	// The gate, stated rather than hidden. An environment whose mode forbids
	// previews has nothing in the cluster to read and asking would only turn a
	// spec answer into a cluster error.
	if errs := renderer.PreviewsRequireFlux(resolved); len(errs) > 0 {
		out.Errors = wireErrors(errs)
		return connect.NewResponse(out), nil
	}

	t := Target{
		Project:     out.Project,
		Environment: out.Environment,
		Namespace:   out.Namespace,
	}
	plane, err := s.plane(ctx, t)
	if err != nil {
		return nil, err
	}
	if plane.Previews == nil {
		return nil, unimplemented("reading previews from the cluster")
	}

	set, err := plane.Previews.Previews(ctx, flux.PreviewScope{
		Project:     t.Project,
		Environment: t.Environment,
		Namespace:   t.Namespace,
	})
	if err != nil {
		return nil, failRequest(err)
	}

	out.Lifecycle = wirePreviewLifecycle(set.Lifecycle)
	now := time.Now()
	out.Previews = make([]*kelsonv1alpha1.Preview, 0, len(set.Previews))
	for _, p := range set.Previews {
		out.Previews = append(out.Previews, wirePreview(p, now))
	}
	return connect.NewResponse(out), nil
}

// wirePreviewSettings projects the resolved previews block. It is the resolved
// one and not the authored one deliberately: the limit and the interval a
// reader is shown are the values the rendered manifest carries, defaults
// included, because a ceiling nobody can read is a ceiling nobody checks
// (ADR-0017).
func wirePreviewSettings(p *model.ResolvedPreviews) *kelsonv1alpha1.PreviewSettings {
	out := &kelsonv1alpha1.PreviewSettings{
		Provider:            string(p.Provider),
		Repo:                p.Repo,
		SecretRef:           p.SecretRef,
		Interval:            p.Interval,
		FilterLabels:        p.Filter.Labels,
		IncludeBranch:       p.Filter.IncludeBranch,
		ExcludeBranch:       p.Filter.ExcludeBranch,
		Limit:               int32(p.Filter.Limit), //nolint:gosec // a preview ceiling validated to at most 10000
		SkipLabels:          p.Skip,
		ArtifactsRepository: p.Artifacts.Repository,
		ArtifactsSecretRef:  p.Artifacts.SecretRef,
	}
	return out
}

func wirePreviewLifecycle(l flux.PreviewLifecycle) *kelsonv1alpha1.PreviewLifecycle {
	return &kelsonv1alpha1.PreviewLifecycle{
		Name:            l.Name,
		Served:          l.Served,
		Present:         l.Present,
		ProviderReady:   string(l.Provider),
		ProviderReason:  l.ProviderReason,
		ProviderMessage: l.ProviderMessage,
		SetReady:        string(l.Set),
		SetReason:       l.SetReason,
		SetMessage:      l.SetMessage,
	}
}

// wirePreview projects one preview. The age is computed here for the reason
// ListSecrets computes its own: a client should render the same number the CLI
// would without needing a clock synchronised with the cluster's.
func wirePreview(p flux.Preview, now time.Time) *kelsonv1alpha1.Preview {
	out := &kelsonv1alpha1.Preview{
		Id:            p.ID,
		Namespace:     p.Namespace,
		Sha:           p.SHA,
		Phase:         string(p.Phase),
		Reason:        p.Reason,
		Message:       p.Message,
		ArtifactReady: string(p.Artifact),
		AppliedReady:  string(p.Applied),
		Revision:      p.Revision,
		Suspended:     p.Suspended,
		Hosts:         p.Hosts,
	}
	if !p.CreatedAt.IsZero() {
		out.CreatedAt = p.CreatedAt.UTC().Format(time.RFC3339)
		age := now.Sub(p.CreatedAt)
		if age < 0 {
			age = 0
		}
		out.AgeSeconds = int64(age.Seconds())
	}
	return out
}
