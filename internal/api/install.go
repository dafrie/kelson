package api

import (
	"context"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery/install"
)

// InstallService served: `kelson install` at the API plane (issue #60,
// ADR-0021), which is what the UI's onboarding screen drives.
//
// # The handlers re-run the CLI's pipeline, not a copy of it
//
// Everything of substance — the pins table, detection-first eligibility, the
// digest check, per-object provenance — lives in internal/delivery/install and
// is exactly the code `kelson install` runs. This file only translates wire
// messages onto it, which is what keeps the CLI and the UI from disagreeing
// about what an install refuses (the same discipline api.go states for
// deploy).
//
// # Where the CLI's confirmation step went
//
// The CLI's preview-confirm-apply becomes PlanInstall (the preview), the
// client's own confirmation UI, and Install. Install re-plans server-side
// before applying, so the eligibility answer and the digest check are made at
// apply time — a plan a browser sat on for an hour is a stale claim, never an
// authorization. The scope table marks PlanInstall and Install administrative
// (scope.go): installing writes cluster-scoped RBAC, CRDs and webhook
// configurations, and ADR-0024 §3 refuses that class to agent credentials
// outright.

// Installer is what the handlers need from the delivery plane — the same
// interface cmd/kelson/install.go declares, re-declared rather than imported
// because the CLI's is unexported and a shared home would couple the two
// callers for one method pair.
type Installer interface {
	Plan(ctx context.Context, req install.Request) (*install.Plan, error)
	Execute(ctx context.Context, plan *install.Plan) (*install.Report, error)
}

// InstallConnector builds the installer for one request. Per request rather
// than at startup for the reason deliveryConnector is: the REST mapper behind
// it is discovery-backed and never refreshed, and an install is exactly the
// operation that registers new CRDs.
type InstallConnector func(ctx context.Context) (Installer, error)

// ListComponents reports the pins table crossed with live detection: what the
// cluster has, what kelson offers to add, and what it declines to.
func (s *Server) ListComponents(ctx context.Context, _ *connect.Request[kelsonv1alpha1.ListComponentsRequest]) (*connect.Response[kelsonv1alpha1.ListComponentsResponse], error) {
	if s.profile == nil {
		return nil, unimplemented("live profile capture")
	}
	profile, err := s.profile.Capture(ctx)
	if err != nil {
		return nil, fail(connect.CodeUnavailable, err)
	}
	res := &kelsonv1alpha1.ListComponentsResponse{}
	for _, c := range install.Components {
		outcome, detail := c.Presence(profile)
		res.Components = append(res.Components, &kelsonv1alpha1.ComponentStatus{
			Name:           c.Name,
			Title:          c.Title,
			Version:        c.Version,
			Namespace:      c.Namespace,
			Provides:       c.Provides,
			Installable:    c.Status == install.StatusSupported,
			FollowUp:       c.FollowUp,
			Presence:       outcome.String(),
			PresenceDetail: detail,
			ManifestUrl:    c.ManifestURL,
			Sha256:         c.SHA256,
		})
	}
	for _, g := range profile.Incomplete {
		res.ProfileGaps = append(res.ProfileGaps, &kelsonv1alpha1.ProfileGap{Field: g.Field, Reason: g.Reason})
	}
	return connect.NewResponse(res), nil
}

// PlanInstall computes what an install would do without applying anything:
// the preview leg of the CLI's preview-confirm-apply, served.
func (s *Server) PlanInstall(ctx context.Context, req *connect.Request[kelsonv1alpha1.PlanInstallRequest]) (*connect.Response[kelsonv1alpha1.PlanInstallResponse], error) {
	plan, _, err := s.planInstall(ctx, req.Msg.GetComponents(), req.Msg.GetAllMissing())
	if err != nil {
		return nil, err
	}
	res := &kelsonv1alpha1.PlanInstallResponse{Refusals: wireRefusals(plan.Refusals)}
	for _, item := range plan.Items {
		res.Items = append(res.Items, wireInstallItem(item))
	}
	return connect.NewResponse(res), nil
}

// Install re-plans and applies. The re-plan is the point: eligibility is
// detection's answer at apply time, and every manifest is fetched and
// digest-verified again before any component is applied.
func (s *Server) Install(ctx context.Context, req *connect.Request[kelsonv1alpha1.InstallRequest]) (*connect.Response[kelsonv1alpha1.InstallResponse], error) {
	plan, engine, err := s.planInstall(ctx, req.Msg.GetComponents(), req.Msg.GetAllMissing())
	if err != nil {
		return nil, err
	}
	res := &kelsonv1alpha1.InstallResponse{Refusals: wireRefusals(plan.Refusals)}
	if plan.Empty() {
		return connect.NewResponse(res), nil
	}
	report, execErr := engine.Execute(ctx, plan)
	if report != nil {
		seen := map[string]bool{}
		var kinds []string
		var applied int
		for _, cr := range report.Components {
			res.Components = append(res.Components, wireComponentReport(cr))
			applied += cr.Created + cr.Adopted
			for _, result := range cr.Results {
				if kind := result.Ref.Kind; kind != "" && !seen[kind] {
					seen[kind] = true
					kinds = append(kinds, kind)
				}
			}
		}
		// The audit record counts objects and names the kinds touched: a
		// platform install is exactly the kind of wide write the trail exists
		// for (ADR-0026).
		auditChange(ctx, controlstore.AuditChange{
			Source:    controlstore.ChangeFromRendered,
			Resources: applied,
			Kinds:     kinds,
		})
	}
	if execErr != nil {
		return nil, failRequest(execErr)
	}
	return connect.NewResponse(res), nil
}

// planInstall is the shared front half of PlanInstall and Install: capture the
// profile, build the engine, plan.
func (s *Server) planInstall(ctx context.Context, components []string, allMissing bool) (*install.Plan, Installer, error) {
	if s.profile == nil {
		return nil, nil, unimplemented("live profile capture")
	}
	if s.install == nil {
		return nil, nil, unimplemented("the platform-component installer")
	}
	req := install.Request{Components: components, AllMissing: allMissing}
	if err := req.Validate(); err != nil {
		return nil, nil, failRequest(err)
	}
	profile, err := s.profile.Capture(ctx)
	if err != nil {
		return nil, nil, fail(connect.CodeUnavailable, err)
	}
	req.Profile = profile
	engine, err := s.install(ctx)
	if err != nil {
		return nil, nil, fail(connect.CodeUnavailable, err)
	}
	plan, err := engine.Plan(ctx, req)
	if err != nil {
		return nil, nil, failRequest(err)
	}
	return plan, engine, nil
}

func wireInstallItem(item install.Item) *kelsonv1alpha1.InstallItem {
	out := &kelsonv1alpha1.InstallItem{
		Component:   item.Component.Name,
		Title:       item.Component.Title,
		Version:     item.Component.Version,
		Namespace:   item.Component.Namespace,
		ManifestUrl: item.Component.ManifestURL,
		Digest:      item.Digest,
		Provides:    item.Component.Provides,
	}
	for _, o := range item.Objects {
		out.Objects = append(out.Objects, &kelsonv1alpha1.InstallObject{
			ApiVersion: o.Ref.APIVersion,
			Kind:       o.Ref.Kind,
			Name:       o.Ref.Name,
			Namespace:  o.Ref.Namespace,
			Exists:     o.Exists,
			Authored:   o.Authored,
		})
	}
	return out
}

func wireRefusals(refusals []install.Refusal) []*kelsonv1alpha1.InstallRefusal {
	out := make([]*kelsonv1alpha1.InstallRefusal, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, &kelsonv1alpha1.InstallRefusal{
			Component:   r.Name,
			Outcome:     r.Outcome.String(),
			Reason:      r.Reason,
			Remediation: r.Remediation,
		})
	}
	return out
}

func wireComponentReport(cr install.ComponentReport) *kelsonv1alpha1.ComponentReport {
	out := &kelsonv1alpha1.ComponentReport{
		Component: cr.Component.Name,
		Installed: cr.Installed,
		Created:   int32(cr.Created),
		Adopted:   int32(cr.Adopted),
		Failed:    int32(cr.Failed),
	}
	for _, res := range cr.Results {
		out.Results = append(out.Results, &kelsonv1alpha1.InstallResult{
			Ref:     res.Ref.String(),
			Outcome: string(res.Outcome),
			Detail:  res.Detail,
		})
	}
	return out
}
