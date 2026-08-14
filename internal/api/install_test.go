package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/observation"
)

// fakeInstallEngine stages a plan and records what Execute received.
type fakeInstallEngine struct {
	plan     *install.Plan
	report   *install.Report
	executed bool
	request  install.Request
}

func (f *fakeInstallEngine) Plan(_ context.Context, req install.Request) (*install.Plan, error) {
	f.request = req
	return f.plan, nil
}

func (f *fakeInstallEngine) Execute(context.Context, *install.Plan) (*install.Report, error) {
	f.executed = true
	return f.report, nil
}

func installConnector(engine *fakeInstallEngine) InstallConnector {
	return func(context.Context) (Installer, error) { return engine, nil }
}

// TestListComponents: the pins table crossed with detection — presence is the
// tri-state and the deferred row still says why it will not install.
func TestListComponents(t *testing.T) {
	captured := clusterprofile.ClusterProfile{
		CertManager: &clusterprofile.CertManager{Version: "v1.20.0"},
		Incomplete: []clusterprofile.Gap{{
			Field:  "cnpg",
			Reason: "forbidden: needs get on /apis",
		}},
	}
	c := serve(t, Options{Profile: fakeProfile(captured)})

	res, err := c.installs.ListComponents(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListComponentsRequest{}))
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	byName := map[string]*kelsonv1alpha1.ComponentStatus{}
	for _, comp := range res.Msg.GetComponents() {
		byName[comp.GetName()] = comp
	}
	if len(byName) != len(install.Components) {
		t.Fatalf("components = %d, want the whole pins table (%d)", len(byName), len(install.Components))
	}
	if cm := byName["cert-manager"]; cm.GetPresence() != "yes" || cm.GetPresenceDetail() == "" {
		t.Fatalf("cert-manager = %+v, want presence yes with the detected version", cm)
	}
	if cnpg := byName["cnpg"]; cnpg.GetPresence() != "unknown" {
		t.Fatalf("cnpg presence = %q, want unknown under a detection gap", cnpg.GetPresence())
	}
	eg := byName["envoy-gateway"]
	if !eg.GetInstallable() || eg.GetPresence() != "no" || eg.GetVersion() == "" || eg.GetSha256() == "" {
		t.Fatalf("envoy-gateway = %+v, want an installable absent row with its pin", eg)
	}
	eso := byName["external-secrets"]
	if eso.GetInstallable() || eso.GetFollowUp() == "" {
		t.Fatalf("external-secrets = %+v, want a deferred row that says why", eso)
	}
	if len(res.Msg.GetProfileGaps()) != 1 {
		t.Fatalf("profile gaps = %v, want the capture's one", res.Msg.GetProfileGaps())
	}
}

// TestPlanInstallServesThePreview: the plan leg carries objects, the verified
// digest, and refusals — and executes nothing.
func TestPlanInstallServesThePreview(t *testing.T) {
	eg, ok := install.Lookup("envoy-gateway")
	if !ok {
		t.Fatal("no envoy-gateway pin")
	}
	engine := &fakeInstallEngine{plan: &install.Plan{
		Items: []install.Item{{
			Component: eg,
			Digest:    eg.SHA256,
			Objects: []install.Object{{
				Ref:    install.Ref{APIVersion: "v1", Kind: "Namespace", Name: "envoy-gateway-system"},
				Exists: true,
			}},
		}},
		Refusals: []install.Refusal{{
			Name: "flux", Outcome: clusterprofile.OutcomeYes, Reason: "flux-operator v0.58.0 is present",
		}},
	}}
	c := serve(t, Options{Profile: fakeProfile(clusterprofile.ClusterProfile{}), Install: installConnector(engine)})

	res, err := c.installs.PlanInstall(context.Background(), connect.NewRequest(&kelsonv1alpha1.PlanInstallRequest{
		AllMissing: true,
	}))
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if engine.executed {
		t.Fatal("PlanInstall executed the plan")
	}
	if !engine.request.AllMissing {
		t.Fatal("the request's addressing did not reach the engine")
	}
	items := res.Msg.GetItems()
	if len(items) != 1 || items[0].GetComponent() != "envoy-gateway" || items[0].GetDigest() != eg.SHA256 {
		t.Fatalf("items = %v, want the staged envoy-gateway item with its digest", items)
	}
	if len(items[0].GetObjects()) != 1 || !items[0].GetObjects()[0].GetExists() {
		t.Fatalf("objects = %v, want the existing namespace flagged", items[0].GetObjects())
	}
	refusals := res.Msg.GetRefusals()
	if len(refusals) != 1 || refusals[0].GetOutcome() != "yes" {
		t.Fatalf("refusals = %v, want flux refused with outcome yes", refusals)
	}
}

// TestInstallExecutesAndReports: Install re-plans, applies, and reports per
// object; addressing nothing is a refused request.
func TestInstallExecutesAndReports(t *testing.T) {
	eg, ok := install.Lookup("envoy-gateway")
	if !ok {
		t.Fatal("no envoy-gateway pin")
	}
	engine := &fakeInstallEngine{
		plan: &install.Plan{Items: []install.Item{{Component: eg, Digest: eg.SHA256}}},
		report: &install.Report{Components: []install.ComponentReport{{
			Component: eg,
			Installed: true,
			Created:   41,
			Adopted:   1,
			Results: []install.Result{{
				Ref:     install.Ref{APIVersion: "v1", Kind: "Namespace", Name: "envoy-gateway-system"},
				Outcome: install.OutcomeAdopted,
				Detail:  "it was already there",
			}},
		}}},
	}
	c := serve(t, Options{Profile: fakeProfile(clusterprofile.ClusterProfile{}), Install: installConnector(engine)})

	res, err := c.installs.Install(context.Background(), connect.NewRequest(&kelsonv1alpha1.InstallRequest{
		Components: []string{"envoy-gateway"},
	}))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !engine.executed {
		t.Fatal("Install did not execute the plan")
	}
	reports := res.Msg.GetComponents()
	if len(reports) != 1 || !reports[0].GetInstalled() || reports[0].GetCreated() != 41 || reports[0].GetAdopted() != 1 {
		t.Fatalf("reports = %v, want the staged counts", reports)
	}
	if len(reports[0].GetResults()) != 1 || reports[0].GetResults()[0].GetOutcome() != "adopted" {
		t.Fatalf("results = %v, want the adopted namespace", reports[0].GetResults())
	}

	if _, err := c.installs.Install(context.Background(), connect.NewRequest(&kelsonv1alpha1.InstallRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("addressing nothing = %v, want InvalidArgument", err)
	}
}

// TestInstallUnwired: both seams answer unimplemented by name.
func TestInstallUnwired(t *testing.T) {
	c := serve(t, Options{Profile: fakeProfile(clusterprofile.ClusterProfile{})})
	_, err := c.installs.Install(context.Background(), connect.NewRequest(&kelsonv1alpha1.InstallRequest{AllMissing: true}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented without an install seam", connect.CodeOf(err))
	}
}

// --- NodeService -------------------------------------------------------------

type fakeNodes struct{ inv observation.NodeInventory }

func (f fakeNodes) Nodes(context.Context) (observation.NodeInventory, error) { return f.inv, nil }

// TestGetNodes: capacity always, usage only when it was read — nil usage stays
// absent on the wire rather than becoming a zero.
func TestGetNodes(t *testing.T) {
	cpu := int64(250)
	mem := int64(1 << 30)
	c := serve(t, Options{Nodes: fakeNodes{inv: observation.NodeInventory{
		UsageGap: "the cluster does not serve metrics.k8s.io — install metrics-server to see live CPU and memory usage",
		Nodes: []observation.Node{
			{
				Name: "control-a", Roles: []string{"control-plane"}, Ready: true,
				KubeletVersion: "v1.31.2", Architecture: "arm64",
				CPUCapacityMilli: 4000, CPUAllocatableMilli: 3800,
				MemoryCapacityBytes: 8 << 30, MemoryAllocatableBytes: 7 << 30,
				CPUUsageMilli: &cpu, MemoryUsageBytes: &mem,
			},
			{Name: "worker-b", Ready: false, CPUCapacityMilli: 4000, MemoryCapacityBytes: 8 << 30},
		},
	}}})

	res, err := c.nodes.GetNodes(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetNodesRequest{}))
	if err != nil {
		t.Fatalf("GetNodes: %v", err)
	}
	nodes := res.Msg.GetNodes()
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	control := nodes[0]
	if control.GetName() != "control-a" || !control.GetReady() || control.GetCpuAllocatableMilli() != 3800 {
		t.Fatalf("control = %+v", control)
	}
	if control.CpuUsageMilli == nil || control.GetCpuUsageMilli() != 250 {
		t.Fatalf("control usage = %v, want 250", control.CpuUsageMilli)
	}
	worker := nodes[1]
	if worker.CpuUsageMilli != nil || worker.MemoryUsageBytes != nil {
		t.Fatal("worker usage must stay absent, not read as zero")
	}
	if res.Msg.GetUsageGap() == "" {
		t.Fatal("the usage gap was dropped on the wire")
	}

	cNo := serve(t, Options{})
	if _, err := cNo.nodes.GetNodes(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetNodesRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented without a node seam", connect.CodeOf(err))
	}
}
