// Package api serves the kelson v1alpha1 schema over ConnectRPC (issue #139,
// ADR-0013 §2). It is the transport half of kelson-server: one Server
// implements every generated service interface, and cmd/kelson-server
// mounts them on an http.ServeMux.
//
// # Everything that touches a cluster arrives as a seam
//
// A handler must be testable without a cluster, and the api plane's depguard
// rule (.golangci.yml) forbids the Kubernetes client libraries outright — the
// cluster stays behind internal/controlstore, internal/delivery and
// internal/observation. So every cluster-facing capability enters through a
// narrow interface declared here: [SpecStore] for the spec store,
// [ProfileCapture] for live detection, [DeliveryConnector] for the observation
// plane, [PreviewConnector] for the server-side dry-run engine, [LogEngine]
// for log queries and [BuildConnector] for the build plane.
// cmd/kelson-server supplies the production implementations;
// the tests in this package supply in-memory ones. It is the same seam shape
// `kelson deploy` uses for its deliveryConnector (cmd/kelson/deploy.go).
//
// # The handlers re-implement the CLI's pipeline rather than importing it
//
// cmd/kelson's spec pipeline is unexported and filesystem-shaped: it reads -f
// files and resolves overlay paths relative to them. A stored or inline spec
// has no filesystem, so the server's copy of that pipeline lives here
// (pipeline.go) and deliberately refuses overlays rather than ignoring them.
// Everything downstream — model.Resolve, renderer.Render, internal/diff, the
// observation plane — is the identical code path the CLI runs, which is what
// keeps `kelson status` and this API from disagreeing about what a verdict
// means (issue #37).
//
// # The delivery verbs are gated, and the schema is not
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the delivery adapters,
// the rendered-history store and the rollback machinery. The wire schema is
// unchanged — ADR-0027 decision 6 keeps the ConnectRPC surface exactly as
// ADR-0013 §2 defined it — so the RPCs that needed those things still exist and
// answer `CodeUnimplemented` with a `delivery/not-implemented` detail naming the
// tracking issue, rather than being removed from the schema or, worse, half
// answering. Deploy keeps both of its dry-run rungs, because rendering and
// previewing never needed an adapter (deploy.go).
//
// # An invalid spec is an answer, not a transport failure
//
// Validation and render errors travel in the response's `errors` field with the
// RPC reporting success; only a failure of the server itself (an unreachable
// cluster, a store that refused a write) becomes a ConnectRPC error, and then
// the same structured [kelsonv1alpha1.Error] values ride along as error details
// so an agent branches on one taxonomy either way (errors.go).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/secret"
)

// DefaultDeployTimeout is the budget a deployment gets to reach a healthy
// phase when the request names none. It matches statemachine.DefaultTimeout and
// the CLI's --timeout default, so the three do not invent three numbers for the
// same question.
const DefaultDeployTimeout = 5 * time.Minute

// DefaultPollInterval is how often an adapter is asked for its status while a
// deployment is in flight, mirroring cmd/kelson's deployPollInterval.
const DefaultPollInterval = 2 * time.Second

// SpecStore is the spec-store seam. *controlstore.SpecStore implements it; a
// CRD-backed store is the recorded successor and lands behind this same
// interface (ADR-0013 §1).
type SpecStore interface {
	Put(ctx context.Context, project string, docs controlstore.Documents, opts controlstore.PutOptions) (controlstore.Stored, error)
	Get(ctx context.Context, project string) (controlstore.Stored, error)
	List(ctx context.Context) ([]controlstore.Stored, error)
	Delete(ctx context.Context, project string, opts controlstore.DeleteOptions) error
}

// AgentStore is the agent-identity seam (issue #74, ADR-0024).
// *controlstore.AgentStore implements it. A nil one is a server with no agent
// principals: AgentService answers CodeUnimplemented and the gate in auth.go
// recognises no agent tokens, which is the pre-#74 posture exactly.
//
// Authenticate is on this interface rather than on a narrower one because the
// gate and the handlers must not be able to disagree about which store an
// identity lives in — a revocation written through one and not seen by the
// other would be precisely the bug #74 exists to prevent.
type AgentStore interface {
	Create(ctx context.Context, spec controlstore.AgentSpec) (controlstore.Agent, string, error)
	List(ctx context.Context) ([]controlstore.Agent, error)
	Revoke(ctx context.Context, name string) (controlstore.Agent, error)
	Authenticate(ctx context.Context, token string) (controlstore.Agent, error)
}

// The audit-trail seam is [AuditSink] in audit.go, beside the capture points
// that write through it.

// ProfileCapture captures a ClusterProfile from the server's own cluster. In
// production it is detect.FromCluster; in tests it is a fixture, which is the
// whole point of the seam — capability detection needs a cluster and handler
// wiring does not.
type ProfileCapture interface {
	Capture(ctx context.Context) (clusterprofile.ClusterProfile, error)
}

// CaptureFunc adapts a plain function to [ProfileCapture].
type CaptureFunc func(ctx context.Context) (clusterprofile.ClusterProfile, error)

// Capture implements [ProfileCapture].
func (f CaptureFunc) Capture(ctx context.Context) (clusterprofile.ClusterProfile, error) {
	return f(ctx)
}

// Target is what a cluster-reading RPC resolved from its spec: which
// application model, in which namespace. It mirrors cmd/kelson's
// observationTarget minus the CLI-only kubeconfig.
//
// It used to carry a delivery mode, a git target and the profile's
// flux-operator finding, because it also selected an adapter. ADR-0028 decision
// 9 deleted the adapters, so what remains is addressing.
type Target struct {
	Project     string
	Environment string
	// Namespace is the environment's resolved namespace, used when a rendered
	// manifest carries none of its own.
	Namespace string
}

// Plane is the assembled cluster-reading plane for one request, mirroring
// cmd/kelson's observationPlane.
type Plane struct {
	// Health is the observation-plane verdict source used by Status. Nil when
	// the plane could not build one.
	Health observation.Evaluator
	// Previews reads an environment's PR previews back out of the cluster
	// (ADR-0017 stage 3). It rides this plane rather than an Options seam of
	// its own because a preview is delivery state: the objects it reads are the
	// ones flux-operator instantiated from the ResourceSet kelson delivered,
	// and they are in the same cluster this plane just connected to. Nil means
	// this build cannot read previews, which ListPreviews reports as
	// unimplemented rather than as an empty list.
	Previews flux.PreviewReader
}

// DeliveryConnector builds the cluster-reading plane for one request. It takes
// the request's context because everything behind it is a cluster call a
// cancelled RPC must not leave in flight.
type DeliveryConnector func(ctx context.Context, t Target) (*Plane, error)

// PreviewEngine is the L2 (server-side dry-run) preview seam, the same shape
// `kelson diff --dry-run=server` injects. The only in-tree implementation is
// *dryrun.DryRun, which needs a dynamic client this plane may not import.
type PreviewEngine interface {
	Preview(ctx context.Context, set delivery.ManifestSet) (*diff.Diff, error)
}

// PreviewConnector constructs a [PreviewEngine] against the target cluster. It
// receives the resolved ClusterProfile so the engine can consult
// HasPolicyEngine() when attributing a rejection (issue #45).
type PreviewConnector func(ctx context.Context, profile clusterprofile.ClusterProfile) (PreviewEngine, error)

// LogEngine is the log-query seam. The engine owns the bounding rules and this
// package only translates the wire message onto them (issue #54).
// [LogQueryEngine] adapts observation.LogQuery to it.
type LogEngine interface {
	Query(ctx context.Context, q observation.Query) (observation.Result, error)
	Follow(ctx context.Context, q observation.Query) (<-chan observation.Line, LogStream, error)
}

// LogStream is the loss half of a follow: how many lines the engine dropped
// under backpressure so far. It is an interface rather than *observation.Stream
// so a test can drive the drop-reporting path — the counter inside the engine's
// Stream is written by the engine alone and cannot be staged from outside it.
type LogStream interface {
	Dropped() int
}

// LogQueryEngine adapts *observation.LogQuery to [LogEngine].
type LogQueryEngine struct {
	Engine *observation.LogQuery
}

var _ LogEngine = LogQueryEngine{}

// Query implements [LogEngine].
func (e LogQueryEngine) Query(ctx context.Context, q observation.Query) (observation.Result, error) {
	return e.Engine.Query(ctx, q)
}

// Follow implements [LogEngine].
func (e LogQueryEngine) Follow(ctx context.Context, q observation.Query) (<-chan observation.Line, LogStream, error) {
	lines, stream, err := e.Engine.Follow(ctx, q)
	if err != nil {
		// Returning the typed nil would hand the caller a non-nil interface
		// wrapping a nil pointer, which panics on the first Dropped().
		return nil, nil, err
	}
	return lines, stream, nil
}

// BuildTarget is what a Build RPC resolved from its request and spec: which
// strategy, for which project, in which namespace, pushing with which
// credential. It is the build plane's counterpart to [Target] and mirrors
// cmd/kelson's buildTarget minus the CLI-only kubeconfig.
type BuildTarget struct {
	Project     string
	Environment string
	// Strategy is what build.ResolveStrategy decided, and it selects the
	// driver the connector builds: `dockerfile` builds with BuildKit,
	// `buildpacks` with the CNB lifecycle (ADR-0010).
	//
	// It travels on the target rather than being re-derived by the connector
	// because the decision belongs to the shared plan (internal/build/plan.go)
	// that this handler and `kelson build` both run — a connector deciding it
	// again is a second answer to a question with one right one. It is also
	// why this stays a string: the concrete drivers are constructed in
	// cmd/kelson-server, and this plane may not import client-go to reach
	// them.
	Strategy string
	// Namespace is where the build Job runs. It defaults to the environment's
	// resolved namespace, exactly like the CLI's --namespace.
	Namespace string
	// PushSecret names an existing dockerconfigjson Secret in Namespace. It is
	// a reference, never a value (ADR-0009), and empty means an unauthenticated
	// push.
	PushSecret string
}

// SecretStore is the secret-authoring seam of ADR-0009's cluster backend
// (issue #116). *secret.Store implements it against a live clientset; the tests
// here supply an in-memory one.
//
// It is the same shape as every other seam in this file and exists for the same
// reason: writing a Kubernetes Secret needs a client this plane's depguard rule
// forbids (.golangci.yml). Note what the interface cannot do — there is no
// method that returns a value. The masked read-back of ADR-0009 is a property
// of secret.Secret, which has no field a value could travel in, so no handler
// here could send one even by mistake.
type SecretStore interface {
	Set(ctx context.Context, req secret.SetRequest) (secret.Secret, error)
	List(ctx context.Context, t secret.Target) ([]secret.Secret, error)
	Delete(ctx context.Context, req secret.DeleteRequest) error
}

// RevisionResolver answers "what commit does this ref name?" against a remote
// repository. It is an interface for the same reason the CLI's is: the answer
// needs the git libraries, which this plane's depguard rule forbids
// (.golangci.yml). internal/gitref's RemoteResolver implements it.
type RevisionResolver interface {
	Resolve(ctx context.Context, repo, ref string) (string, error)
}

// BuildPlane is the assembled build plane for one request: a driver that can
// run a build, and a resolver that can turn a branch name into the commit the
// build records.
type BuildPlane struct {
	Builder   build.Builder
	Revisions RevisionResolver
}

// BuildConnector builds the build plane for one request, mirroring
// [DeliveryConnector] and cmd/kelson's buildConnector. It needs the target
// because the build Job's namespace and push credential are per-request
// resolutions, not process-wide configuration.
type BuildConnector func(ctx context.Context, t BuildTarget) (*BuildPlane, error)

// BuildDefaults is the server's own build configuration: where images go, who
// may push them, and where the Job runs.
//
// It is configuration and not spec on purpose (ADR-0010, docs/build.md): the
// same Project must build against a team's ghcr.io and against a kind cluster's
// localhost:5000, so the destination belongs to the operator running the
// server. A request may override the registry and the push secret because a
// caller may legitimately push elsewhere; these are what it falls back to.
type BuildDefaults struct {
	Registry   string
	PushSecret string
	// Namespace overrides the environment's resolved namespace for build Jobs.
	// Empty keeps the environment's, which is the CLI's default too.
	Namespace string
}

// Options configures a [Server]. Every seam is optional: a nil one makes the
// RPCs that need it answer CodeUnimplemented with a message naming what is
// missing, which is how a partially-wired server (a test, or a build with no
// cluster) fails honestly instead of panicking.
type Options struct {
	Specs    SpecStore
	Profile  ProfileCapture
	Delivery DeliveryConnector
	Preview  PreviewConnector
	Logs     LogEngine
	Build    BuildConnector
	Secrets  SecretStore
	Agents   AgentStore

	// Audit is the durable audit trail (issue #78, ADR-0026).
	// *controlstore.AuditStore implements it. A nil one is a server that keeps
	// no trail: AuditService answers CodeUnimplemented and every capture point
	// is a no-op, which is the pre-#78 posture exactly. The slog attribution
	// line of #74 is unaffected either way.
	Audit AuditSink

	// BuildDefaults is the destination configuration builds fall back to.
	BuildDefaults BuildDefaults

	// Logger receives the audit attribution line every authenticated request
	// leaves (issue #74, the seam #78 builds on). Nil discards it, which is
	// what a test wants and what a server started without --log-format gets.
	Logger *slog.Logger

	// Now is the clock the authorization interceptor checks expiry and refills
	// rate-limit buckets against. Nil selects time.Now.
	Now func() time.Time

	// DeployTimeout is the fallback budget for a Deploy whose request carries
	// no timeout. Zero selects [DefaultDeployTimeout].
	DeployTimeout time.Duration
	// PollInterval is the adapter status poll interval. Zero selects
	// [DefaultPollInterval].
	PollInterval time.Duration
	// WatchInterval is how often the event broker re-observes a watched scope.
	// Zero selects [DefaultWatchInterval].
	WatchInterval time.Duration
}

// Server implements all twelve kelson.v1alpha1 services.
type Server struct {
	specs    SpecStore
	profile  ProfileCapture
	delivery DeliveryConnector
	preview  PreviewConnector
	logs     LogEngine
	build    BuildConnector
	secrets  SecretStore
	agents   AgentStore

	// authz is the scope, rate-limit and audit interceptor. It is built here
	// and mounted by Register so no caller can serve these handlers without it
	// (issues #74, #78).
	authz *authorizer

	// audit is the interceptor's auditor, held here so QueryAudit reads the
	// same sink the capture points write to. A server whose trail was written
	// through one store and queried from another would produce a trail that
	// disagreed with itself.
	audit *auditor

	buildDefaults BuildDefaults

	deployTimeout time.Duration
	pollInterval  time.Duration

	// events is the watch broker (#76). It holds goroutines only while a Watch
	// stream is open — the last watcher of a scope leaving stops its poller —
	// so a server nobody is watching costs nothing.
	events *broker
}

var (
	_ kelsonv1alpha1connect.SpecServiceHandler    = (*Server)(nil)
	_ kelsonv1alpha1connect.RenderServiceHandler  = (*Server)(nil)
	_ kelsonv1alpha1connect.ProfileServiceHandler = (*Server)(nil)
	_ kelsonv1alpha1connect.DeployServiceHandler  = (*Server)(nil)
	_ kelsonv1alpha1connect.LogServiceHandler     = (*Server)(nil)
	_ kelsonv1alpha1connect.EventServiceHandler   = (*Server)(nil)
	_ kelsonv1alpha1connect.BuildServiceHandler   = (*Server)(nil)
	_ kelsonv1alpha1connect.SecretServiceHandler  = (*Server)(nil)
	_ kelsonv1alpha1connect.PreviewServiceHandler = (*Server)(nil)
	_ kelsonv1alpha1connect.ExplainServiceHandler = (*Server)(nil)
	_ kelsonv1alpha1connect.AgentServiceHandler   = (*Server)(nil)
	_ kelsonv1alpha1connect.AuditServiceHandler   = (*Server)(nil)
)

// New returns a Server over the given seams.
func New(opts Options) *Server {
	s := &Server{
		specs:         opts.Specs,
		profile:       opts.Profile,
		delivery:      opts.Delivery,
		preview:       opts.Preview,
		logs:          opts.Logs,
		build:         opts.Build,
		secrets:       opts.Secrets,
		agents:        opts.Agents,
		authz:         newAuthorizer(opts.Now, opts.Logger, opts.Audit),
		buildDefaults: opts.BuildDefaults,
		deployTimeout: opts.DeployTimeout,
		pollInterval:  opts.PollInterval,
	}
	if s.deployTimeout <= 0 {
		s.deployTimeout = DefaultDeployTimeout
	}
	if s.pollInterval <= 0 {
		s.pollInterval = DefaultPollInterval
	}
	s.audit = s.authz.audit
	s.events = newBroker(s.observeScope, opts.WatchInterval, watchRingSize)
	return s
}

// Register mounts every service on mux. Keeping the mounting here means a
// service added to the schema is wired in one place rather than in every
// caller that serves the API.
//
// The authorization interceptor is prepended to whatever the caller passes, so
// there is no way to serve these handlers without it (issue #74). It is inert
// for a human or anonymous principal — the password path of #84 is unchanged —
// and it is the only thing standing between an agent credential and an RPC.
func (s *Server) Register(mux *http.ServeMux, opts ...connect.HandlerOption) {
	opts = append([]connect.HandlerOption{connect.WithInterceptors(s.authz)}, opts...)
	handlers := []func() (string, http.Handler){
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewSpecServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewRenderServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewProfileServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewDeployServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewLogServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewEventServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewBuildServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewSecretServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewPreviewServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewExplainServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewAgentServiceHandler(s, opts...) },
		func() (string, http.Handler) { return kelsonv1alpha1connect.NewAuditServiceHandler(s, opts...) },
	}
	for _, build := range handlers {
		mux.Handle(build())
	}
}
