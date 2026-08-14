package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/rollback"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/serverstate"
)

// The handlers are assembly: they decode a request, run the same pipeline the
// CLI runs, and project the result onto the wire. What the tests here assert is
// that assembly — over a real HTTP server and the generated Connect clients, so
// the schema, the codec and the streaming framing are exercised too, which is
// the #139 smoke test. The planes behind the seams have their own tests, and
// the api plane's lint rule forbids the Kubernetes clients those tests use, so
// every seam is faked in memory here.

// serve starts an httptest server over the api handlers and returns clients for
// each service.
//
// httptest.NewServer speaks HTTP/1.1, deliberately: the Connect protocol
// carries unary and server-streaming RPCs over HTTP/1.1, which is what lets
// cmd/kelson-server run a plain net/http server with no h2c. If that ever
// stopped being true, the streaming tests below would fail here first.
type clients struct {
	spec     kelsonv1alpha1connect.SpecServiceClient
	render   kelsonv1alpha1connect.RenderServiceClient
	profile  kelsonv1alpha1connect.ProfileServiceClient
	deploy   kelsonv1alpha1connect.DeployServiceClient
	logs     kelsonv1alpha1connect.LogServiceClient
	events   kelsonv1alpha1connect.EventServiceClient
	builds   kelsonv1alpha1connect.BuildServiceClient
	secrets  kelsonv1alpha1connect.SecretServiceClient
	previews kelsonv1alpha1connect.PreviewServiceClient
	explain  kelsonv1alpha1connect.ExplainServiceClient
	installs kelsonv1alpha1connect.InstallServiceClient
	nodes    kelsonv1alpha1connect.NodeServiceClient
}

func serve(t *testing.T, opts Options) clients {
	t.Helper()
	return serveServer(t, New(opts))
}

// serveServer is serve for a test that also needs the *Server itself — the
// watch tests assert on the broker's poller count, which is a property of the
// server rather than of any response.
func serveServer(t *testing.T, server *Server) clients {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	hc := srv.Client()
	return clients{
		spec:     kelsonv1alpha1connect.NewSpecServiceClient(hc, srv.URL),
		render:   kelsonv1alpha1connect.NewRenderServiceClient(hc, srv.URL),
		profile:  kelsonv1alpha1connect.NewProfileServiceClient(hc, srv.URL),
		deploy:   kelsonv1alpha1connect.NewDeployServiceClient(hc, srv.URL),
		logs:     kelsonv1alpha1connect.NewLogServiceClient(hc, srv.URL),
		events:   kelsonv1alpha1connect.NewEventServiceClient(hc, srv.URL),
		builds:   kelsonv1alpha1connect.NewBuildServiceClient(hc, srv.URL),
		secrets:  kelsonv1alpha1connect.NewSecretServiceClient(hc, srv.URL),
		previews: kelsonv1alpha1connect.NewPreviewServiceClient(hc, srv.URL),
		explain:  kelsonv1alpha1connect.NewExplainServiceClient(hc, srv.URL),
		installs: kelsonv1alpha1connect.NewInstallServiceClient(hc, srv.URL),
		nodes:    kelsonv1alpha1connect.NewNodeServiceClient(hc, srv.URL),
	}
}

// --- spec store -------------------------------------------------------------

// fakeSpecStore is an in-memory SpecStore that reproduces serverstate's
// optimistic-concurrency and idempotency contract, including its error values.
// The ConfigMap-backed store is tested against a fake clientset in
// internal/serverstate; what matters here is that the handler maps those errors
// onto the right ConnectRPC codes and details.
type fakeSpecStore struct {
	mu      sync.Mutex
	entries map[string]*fakeSpecEntry
	version int
}

type fakeSpecEntry struct {
	stored serverstate.Stored
	key    string // the idempotency key of the write that produced it
}

func newFakeSpecStore() *fakeSpecStore {
	return &fakeSpecStore{entries: map[string]*fakeSpecEntry{}}
}

func (f *fakeSpecStore) Put(_ context.Context, project string, docs serverstate.Documents, opts serverstate.PutOptions) (serverstate.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ref := "spec/" + project
	existing, ok := f.entries[project]
	switch {
	case !ok && opts.ExpectedVersion != "" && !opts.Force:
		return serverstate.Stored{}, serverstate.NotFound(ref, "no spec is stored", "put without a version to create the project")
	case ok && opts.IdempotencyKey != "" && existing.key == opts.IdempotencyKey:
		return existing.stored, nil
	case ok && !opts.Force && opts.ExpectedVersion == "":
		return serverstate.Stored{}, serverstate.VersionConflict(ref, "the write carried no version", "read the spec and retry with its version")
	case ok && !opts.Force && opts.ExpectedVersion != existing.stored.Version:
		return serverstate.Stored{}, serverstate.VersionConflict(ref, "version mismatch", "re-read the spec and retry")
	}

	f.version++
	envs := make([]string, 0, len(docs.Environments))
	for name := range docs.Environments {
		envs = append(envs, name)
	}
	sort.Strings(envs)
	stored := serverstate.Stored{
		Project:      project,
		Documents:    serverstate.Documents{Project: docs.Project, Environments: maps.Clone(docs.Environments)},
		Version:      strconv.Itoa(f.version),
		Environments: envs,
	}
	f.entries[project] = &fakeSpecEntry{stored: stored, key: opts.IdempotencyKey}
	return stored, nil
}

func (f *fakeSpecStore) Get(_ context.Context, project string) (serverstate.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[project]
	if !ok {
		return serverstate.Stored{}, serverstate.NotFound("spec/"+project,
			fmt.Sprintf("no spec is stored for project %q", project), "create it with PutSpec")
	}
	return entry.stored, nil
}

func (f *fakeSpecStore) List(context.Context) ([]serverstate.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverstate.Stored, 0, len(f.entries))
	for _, entry := range f.entries {
		out = append(out, entry.stored)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

func (f *fakeSpecStore) Delete(_ context.Context, project string, opts serverstate.DeleteOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[project]
	if !ok {
		if opts.IdempotencyKey != "" {
			return nil
		}
		return serverstate.NotFound("spec/"+project, "no spec is stored", "list the stored projects")
	}
	if !opts.Force && opts.ExpectedVersion != entry.stored.Version {
		return serverstate.VersionConflict("spec/"+project, "version mismatch", "re-read the spec and retry")
	}
	delete(f.entries, project)
	return nil
}

// --- delivery ---------------------------------------------------------------

// fakeAdapter is a delivery.Adapter whose answers are scripted, the same shape
// cmd/kelson's tests use. Status is polled from the state machine's watch
// goroutine while the test reads the call log, so every field is guarded.
type fakeAdapter struct {
	name string
	caps delivery.Capabilities

	mu       sync.Mutex
	calls    []string
	rolledTo []delivery.Entry

	applyResult delivery.Result
	applyErr    error
	// statuses are returned in order; the last one repeats forever.
	statuses    []delivery.Status
	history     []delivery.Entry
	historyErr  error
	rollbackRes delivery.Result
	rollbackErr error
}

func newFakeAdapter(name string) *fakeAdapter {
	return &fakeAdapter{name: name, caps: delivery.Capabilities{SupportsRollback: true}}
}

func (f *fakeAdapter) Name() string                        { return f.name }
func (f *fakeAdapter) Capabilities() delivery.Capabilities { return f.caps }

func (f *fakeAdapter) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAdapter) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAdapter) Apply(context.Context, delivery.ManifestSet) (delivery.Result, error) {
	f.record("apply")
	if f.applyErr != nil {
		return delivery.Result{}, f.applyErr
	}
	res := f.applyResult
	if res.Revision == "" {
		res = delivery.Result{Revision: "rev-00000001", Applied: true}
	}
	return res, nil
}

func (f *fakeAdapter) Status(context.Context, delivery.ManifestSet) (delivery.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "status")
	switch {
	case len(f.statuses) == 0:
		return delivery.Status{Phase: delivery.PhaseHealthy}, nil
	case len(f.statuses) == 1:
		return f.statuses[0], nil
	default:
		st := f.statuses[0]
		f.statuses = f.statuses[1:]
		return st, nil
	}
}

func (f *fakeAdapter) History(context.Context, delivery.ManifestSet) ([]delivery.Entry, error) {
	f.record("history")
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history, nil
}

func (f *fakeAdapter) Rollback(_ context.Context, _ delivery.ManifestSet, to delivery.Entry) (delivery.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "rollback")
	f.rolledTo = append(f.rolledTo, to)
	f.mu.Unlock()
	if f.rollbackErr != nil {
		return delivery.Result{}, f.rollbackErr
	}
	res := f.rollbackRes
	if res.Revision == "" {
		res = delivery.Result{Revision: "rev-00000009", Applied: true}
	}
	return res, nil
}

// fakeRecorded is a rollback.Source over recorded manifest sets, the seam the
// rollback preview and the from_revision diff (#162) both read their prior
// state through. Revision returns the recorded bytes verbatim; a revision it
// does not hold fails the way the cluster-backed history store does
// (serverstate.NotFound), so the handler's mapping of that error is exercised
// rather than assumed.
type fakeRecorded struct {
	current   []delivery.Manifest
	revisions map[string][]delivery.Manifest
}

var _ rollback.Source = (*fakeRecorded)(nil)

func (f *fakeRecorded) Current(context.Context) ([]delivery.Manifest, error) {
	return f.current, nil
}

func (f *fakeRecorded) Revision(_ context.Context, revision string) ([]delivery.Manifest, error) {
	ms, ok := f.revisions[revision]
	if !ok {
		return nil, serverstate.NotFound("history/"+revision,
			fmt.Sprintf("revision %q is not in the retained history", revision),
			"list the history for the revisions that still exist")
	}
	return ms, nil
}

// connectorFor returns a DeliveryConnector serving one adapter, recording the
// targets it was asked for.
func connectorFor(adapter *fakeAdapter, recorded rollback.Source, health observation.Evaluator) (DeliveryConnector, *[]Target) {
	var targets []Target
	return func(_ context.Context, t Target) (*Plane, error) {
		targets = append(targets, t)
		reg := delivery.NewRegistry()
		if err := reg.Register(adapter); err != nil {
			return nil, err
		}
		return &Plane{Registry: reg, Health: health, Recorded: recorded}, nil
	}, &targets
}

// --- observation ------------------------------------------------------------

// fakeLogEngine answers log queries from a fixed set of lines, applying nothing
// but the seam: the bounding and merging rules belong to observation.LogQuery
// and are tested there.
type fakeLogEngine struct {
	query  observation.Query
	result observation.Result
	err    error
	follow []observation.Line
	// dropsBefore[i] is the engine's cumulative drop count as of just before
	// line i is produced, staging the backpressure the handler must report.
	// Setting it before the send (on an unbuffered channel) is what makes the
	// handler's post-line check see it deterministically.
	dropsBefore map[int]int
}

func (f *fakeLogEngine) Query(_ context.Context, q observation.Query) (observation.Result, error) {
	f.query = q
	if f.err != nil {
		return observation.Result{}, f.err
	}
	return f.result, nil
}

func (f *fakeLogEngine) Follow(ctx context.Context, q observation.Query) (<-chan observation.Line, LogStream, error) {
	f.query = q
	if f.err != nil {
		return nil, nil, f.err
	}
	stream := &fakeLogStream{}
	ch := make(chan observation.Line)
	go func() {
		defer close(ch)
		for i, l := range f.follow {
			if n, ok := f.dropsBefore[i]; ok {
				stream.set(n)
			}
			select {
			case ch <- l:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, stream, nil
}

// fakeLogStream stands in for observation.Stream's loss counter.
type fakeLogStream struct {
	mu sync.Mutex
	n  int
}

func (s *fakeLogStream) set(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = n
}

func (s *fakeLogStream) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// fakePreview is a PreviewEngine returning a scripted L2 verdict.
type fakePreview struct {
	diff *diff.Diff
	err  error
	sets []delivery.ManifestSet
}

func (f *fakePreview) Preview(_ context.Context, set delivery.ManifestSet) (*diff.Diff, error) {
	f.sets = append(f.sets, set)
	if f.err != nil {
		return nil, f.err
	}
	return f.diff, nil
}

func previewConnector(p *fakePreview) PreviewConnector {
	return func(context.Context, clusterprofile.ClusterProfile) (PreviewEngine, error) { return p, nil }
}

// fakeEvaluator answers one verdict per workload name.
type fakeEvaluator map[string]observation.Verdict

func (f fakeEvaluator) Evaluate(_ context.Context, namespace, name string) (observation.Verdict, error) {
	if v, ok := f[name]; ok {
		return v, nil
	}
	return observation.Verdict{Healthy: true, Resource: "Deployment/" + namespace + "/" + name}, nil
}

// steppingEvaluator answers healthy once and crash-looping thereafter, so the
// broker's second observation of a scope carries a real health change. It is
// called from a poller goroutine while the test reads the stream, hence the
// lock.
type steppingEvaluator struct {
	mu    sync.Mutex
	calls int
}

func (e *steppingEvaluator) Evaluate(_ context.Context, namespace, name string) (observation.Verdict, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	resource := "Deployment/" + namespace + "/" + name
	if e.calls == 1 {
		return observation.Verdict{Healthy: true, Code: observation.CodeHealthy, Resource: resource}, nil
	}
	return observation.Verdict{
		Resource:    resource,
		Code:        observation.CodeCrashLoopBackOff,
		Reason:      "back-off restarting failed container",
		Remediation: "the container keeps crashing: read its logs",
	}, nil
}

// syncingEvaluator is a fakeEvaluator that also answers sync questions,
// standing in for the real observation.Probe's optional SecretSyncEvaluator
// capability (issue #80).
type syncingEvaluator struct {
	fakeEvaluator
	sync map[string]observation.Verdict
}

func (s syncingEvaluator) EvaluateSecretSync(_ context.Context, namespace, name string) (observation.Verdict, error) {
	if v, ok := s.sync[name]; ok {
		return v, nil
	}
	return observation.Verdict{
		Healthy:  true,
		Code:     observation.CodeHealthy,
		Resource: "external-secrets.io/ExternalSecret/" + namespace + "/" + name,
	}, nil
}

// fakeProfile is a ProfileCapture returning a fixture.
func fakeProfile(p clusterprofile.ClusterProfile) ProfileCapture {
	return CaptureFunc(func(context.Context) (clusterprofile.ClusterProfile, error) { return p, nil })
}
