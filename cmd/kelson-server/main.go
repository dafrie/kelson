// Command kelson-server serves the kelson v1alpha1 schema over ConnectRPC
// (issue #139, ADR-0013).
//
// # The process holds no state
//
// Specs and deployment history live in ConfigMaps in the namespace the server
// runs against (--namespace, default kelson-system). A restart or a second
// replica loses and forks nothing, which is why there is no volume, no
// migration and no leader election here (ADR-0013 §1).
//
// # Loopback by default; one shared password buys more than that
//
// Without --password (or KELSON_PASSWORD) the posture is v0's: no
// authentication, no TLS, loopback by default, and a non-loopback --listen
// refused unless --insecure-bind accepts the risk out loud.
//
// With a password set, every /kelson.v1alpha1.* route requires a session — a
// signed cookie for browsers, the password as a bearer token for everything
// else (internal/api/auth.go) — and a non-loopback bind is allowed with a
// warning instead of refused, because the port is no longer an open door. There
// is still no TLS: put a TLS-terminating proxy in front. This is #84's interim
// cut (ADR-0013 §3, amended 2026-08-13); #84 still owns the real answer.
//
// # Agents authenticate as themselves
//
// Beside the password there are agent identities (issue #74, ADR-0024): named
// principals with a scope, an expiry and a request budget, stored as Secrets in
// the state namespace. An agent sends its own token as `Authorization: Bearer`;
// the server resolves it from cluster state on every request, so a revocation
// takes effect immediately, and enforces its scope in a ConnectRPC interceptor
// before any handler runs. Issue them with `kelson agent create` or
// AgentService — both refuse an agent credential, because an agent that could
// mint an agent could mint one wider than itself.
//
// Every authenticated request leaves one JSON line on stderr naming the
// principal, its type, the method and the outcome.
//
// # And every mutation leaves a durable record
//
// Beside that line there is an audit trail (issue #78, ADR-0026): one record per
// mutation and per refusal, in ConfigMaps in the state namespace, naming the
// principal, its scope, the target, the outcome, the revision the change
// produced and the reason the caller stated. `kelson audit` reads it, and so
// does AuditService for a human at the API — never an agent, whatever its scope.
// Retention is --audit-retention days of a bounded ring; a query says when its
// window is incomplete rather than letting a partial answer read as a whole one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dafrie/kelson/internal/api"
	"github.com/dafrie/kelson/internal/build/buildkit"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/dryrun"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/delivery/rollback"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/secret"
	"github.com/dafrie/kelson/internal/serverstate"
	"github.com/dafrie/kelson/internal/version"
	"github.com/dafrie/kelson/internal/webui"
)

func main() { os.Exit(cli()) }

// cli exists so main holds no deferred work: os.Exit skips defers, and the
// signal handler's stop must run before the process leaves.
func cli() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err) //nolint:errcheck // nothing to do if stderr is gone
		return 1
	}
	return 0
}

// config is the server's resolved command line.
type config struct {
	listen       string
	insecureBind bool
	kubeconfig   string
	namespace    string
	keep         int

	// auditRetention is how many UTC days of audit records to keep. Zero
	// selects the store's default; a negative value turns the trail off.
	auditRetention int

	// password is the single shared secret web and non-browser clients
	// authenticate with (#84's interim cut, ADR-0013 §3). Empty disables
	// authentication entirely, which is the pre-#84 behaviour.
	password string

	// The build plane's destination configuration. It is flags and not spec for
	// ADR-0010's reason (docs/build.md): where an image is pushed is
	// infrastructure, and the same Project must build against a team's ghcr.io
	// and against a kind cluster's localhost:5000.
	registry       string
	pushSecret     string
	buildNamespace string
}

// registryEnv supplies --registry, so an operator sets the destination once in
// the deployment rather than in every request. It is the same variable
// `kelson build` reads.
const registryEnv = "KELSON_REGISTRY"

// passwordEnv supplies --password. It is the preferred way to set it: a flag
// value is visible in `ps` and in shell history, an environment variable is at
// least only readable by the process's owner. Same variable kelson-mcp reads.
const passwordEnv = "KELSON_PASSWORD"

// defaultNamespace is where the state ConfigMaps live. It matches the ADR's
// default and the RBAC the deploy manifests grant.
const defaultNamespace = "kelson-system"

// shutdownGrace is how long in-flight requests get to finish after a signal.
// Server-streaming RPCs (a deploy being watched) can legitimately outlive it,
// which is why the deadline exists at all: an operator asking for a stop must
// get one.
const shutdownGrace = 15 * time.Second

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	warning, err := checkBind(cfg.listen, cfg.insecureBind, cfg.password != "")
	if err != nil {
		return err
	}
	if warning != "" {
		_, _ = fmt.Fprintln(stderr, "warning:", warning)
	}

	// The attribution line every authenticated request leaves (issue #74) goes
	// to stderr as JSON, beside the banner and the warnings. stdout is left to
	// the banner alone so a caller piping it is not handed a stream of records
	// it did not ask for.
	//
	// It is not the audit trail — that is durable and lives in the cluster
	// (issue #78) — but it is what carries an audit *write failure* to whatever
	// collects this process's stderr, which is the visibility half of "a lost
	// record must never be silent" (ADR-0026 §3).
	attribution := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	server, agents, err := connectServer(cfg, attribution)
	if err != nil {
		return err
	}

	auth, err := api.NewAuthWith(api.AuthOptions{Password: cfg.password, Agents: agents})
	if err != nil {
		return err
	}

	// A plain net/http server, no h2c. The Connect protocol carries unary and
	// server-streaming RPCs over HTTP/1.1 — only client- and bidi-streaming
	// need HTTP/2, and the v1alpha1 schema has neither (deploy, rollback and
	// log-follow are all server-streaming). h2c would also mean
	// golang.org/x/net, which the api plane's depguard rule does not allow
	// (ADR-0013 §4: the fence extends rather than loosens). Adding gRPC-client
	// support later is an http2 server, not a schema change.
	srv := &http.Server{
		Handler: newMux(server, auth),
		// Only the header deadline is set. A read or write deadline would kill
		// the server-streaming RPCs this API exists to serve — a deploy stream
		// lives as long as the deployment does.
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.listen, err)
	}
	// The banner is courtesy, not a result: a failed write to stdout must not
	// stop a server that is already listening.
	_, _ = fmt.Fprintf(stdout, "kelson-server %s serving the kelson.v1alpha1 schema on http://%s (namespace %s, %s)\n",
		version.String(), listener.Addr(), cfg.namespace,
		strings.Join([]string{authBanner(auth), auditBanner(cfg), webBanner()}, ", "))

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		_, _ = fmt.Fprintln(stdout, "shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		return <-errs
	}
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("kelson-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.listen, "listen", "127.0.0.1:8420", "address to serve on")
	fs.BoolVar(&cfg.insecureBind, "insecure-bind", false, "allow binding a non-loopback address; v0 has no authentication (see #84)")
	fs.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	fs.StringVar(&cfg.namespace, "namespace", defaultNamespace, "namespace holding kelson-server's state ConfigMaps")
	fs.IntVar(&cfg.keep, "keep", direct.DefaultKeep, "number of deployment revisions to retain per environment")
	fs.IntVar(&cfg.auditRetention, "audit-retention", serverstate.DefaultAuditRetentionDays,
		fmt.Sprintf("days of audit records to retain (maximum %d); 0 disables the audit trail, which is a choice to make out loud (issue #78)",
			serverstate.MaxAuditRetentionDays))
	fs.StringVar(&cfg.password, "password", os.Getenv(passwordEnv),
		"shared password web clients log in with and non-browser clients send as a bearer token (default: $"+passwordEnv+"); empty disables authentication")
	fs.StringVar(&cfg.registry, "registry", os.Getenv(registryEnv), "destination registry and namespace for builds, e.g. ghcr.io/acme (default: $"+registryEnv+"); a Build request may override it")
	fs.StringVar(&cfg.pushSecret, "push-secret", "", "name of an existing kubernetes.io/dockerconfigjson Secret in the build namespace that authenticates the push")
	fs.StringVar(&cfg.buildNamespace, "build-namespace", "", "namespace build Jobs run in (default: the environment's own namespace, as in the CLI)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-server takes flags only", fs.Arg(0))
	}
	if cfg.namespace == "" {
		return config{}, errors.New("--namespace must not be empty: it is where the spec and history ConfigMaps live")
	}
	if cfg.auditRetention < 0 {
		return config{}, fmt.Errorf("--audit-retention %d is not a number of days; pass 0 to disable the audit trail",
			cfg.auditRetention)
	}
	return cfg, nil
}

// checkBind decides whether this listen address may be served, and what the
// operator should be told about it. It returns a warning to print and an error
// that stops the process (ADR-0013 §3, as amended 2026-08-13).
//
// The split is on the password, not on the flag, because the password is what
// changes the fact on the ground:
//
//   - Loopback is always fine, and says nothing.
//   - Non-loopback with a password is allowed, with a warning: every API route
//     now demands a secret, so the port is no longer an open door. There is
//     still no TLS, so the warning says to put a TLS-terminating proxy in
//     front — a password sent in clear is a password anyone on the path has.
//   - Non-loopback with no password is refused, exactly as before, naming #84
//     and both ways out: set a password, or pass --insecure-bind.
//   - --insecure-bind still overrides the refusal, and still warns: it remains
//     the escape hatch for "I know, I am on a private network", and it must not
//     go quiet just because it works.
func checkBind(listen string, insecure, password bool) (string, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("--listen %q is not a host:port address: %w", listen, err)
	}
	if isLoopback(host) {
		return "", nil
	}
	switch {
	case password:
		return fmt.Sprintf("--listen %s serves beyond loopback. Every kelson.v1alpha1 route requires the shared password, "+
			"but there is no TLS: put a TLS-terminating proxy in front, or the password crosses the network in clear (issue #84)", listen), nil
	case insecure:
		return fmt.Sprintf("--listen %s serves beyond loopback with no authentication and no TLS (--insecure-bind). "+
			"Anything that can reach this port can deploy to your cluster (ADR-0013 §3; the threat model is issue #84)", listen), nil
	default:
		return "", fmt.Errorf("--listen %s would expose kelson-server beyond loopback with no authentication and no TLS "+
			"(ADR-0013 §3; the threat model is issue #84). Set --password (or $%s) so clients must authenticate, "+
			"bind 127.0.0.1 and forward a port, or pass --insecure-bind to accept the risk deliberately", listen, passwordEnv)
	}
}

// isLoopback reports whether host names only this machine. An empty host (":8420")
// binds every interface, which is exactly the exposure the flag guards.
func isLoopback(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newMux mounts the API, the session endpoints, the health endpoint and the web
// UI, behind the auth gate. It is separate from run so a test can exercise the
// routing without a cluster or a listener.
//
// The gate wraps the whole mux rather than only the RPC handlers because it has
// to see the path to decide: it protects /kelson.v1alpha1.* and passes
// everything else — /healthz answers a probe that holds no secret, /auth/* is
// how a client stops being unauthenticated, and the UI must load before there
// is any way to log in (internal/api/auth.go).
//
// The UI is the catch-all and is registered last, but neither fact is what
// gives the API precedence: Go's ServeMux matches the most specific pattern,
// so every route above wins over "/" whatever the order here. The UI handler
// refuses those prefixes itself as well (internal/webui).
func newMux(server *api.Server, auth *api.Auth) http.Handler {
	mux := http.NewServeMux()
	server.Register(mux)
	auth.Register(mux)
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/", webui.Handler())
	return auth.Middleware(mux)
}

// authBanner says which posture the process started in. An operator who set the
// password in the environment and typo'd the variable name must not have to
// discover it by finding the API open.
//
// Agent identities are named separately because they are a second credential
// rather than a stronger version of the first: a server with no password still
// resolves and enforces agent tokens (issue #74).
func authBanner(auth *api.Auth) string {
	if auth.Enabled() {
		return "authentication: shared password, plus agent identities"
	}
	return "authentication: none, plus agent identities"
}

// healthz reports liveness and the build it is reporting for. Version is part
// of the answer because "the server is up" and "the server is the build you
// deployed" are different questions and an operator asks both at once.
// auditBanner says whether this process keeps a trail and for how long. A
// server whose audit trail is off must say so at startup: discovering it by
// finding no records after an incident is the worst possible moment to learn it
// (issue #78).
func auditBanner(cfg config) string {
	if cfg.auditRetention == 0 {
		return "audit trail: OFF (--audit-retention 0)"
	}
	return fmt.Sprintf("audit trail: %d days", cfg.auditRetention)
}

// webBanner says whether this binary carries the web UI or the placeholder that
// stands in for it (internal/webui). It is on the banner for the same reason
// the other two are: a binary built without `make ui` still serves the API
// perfectly, so the only other way to find out is to open a browser and be
// confused.
func webBanner() string {
	if webui.Built() {
		return "web UI: served at /"
	}
	return "web UI: not built into this binary (`make ui`)"
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, err := json.Marshal(map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
	})
	if err != nil {
		http.Error(w, "encoding health response", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// --- production wiring ------------------------------------------------------

// connectServer builds the api.Server over the real cluster.
//
// The typed client is made once, at startup: the state stores only ever read
// and write ConfigMaps, and a cached discovery mapper is not involved. The
// delivery plane is rebuilt per request instead — kube.Connect's REST mapper is
// discovery-backed and never refreshed, so a long-running process reusing one
// would not see a CRD registered after it started (see kube.Connect's doc).
func connectServer(cfg config, attribution *slog.Logger) (*api.Server, *serverstate.AgentStore, error) {
	cluster, err := kube.Connect(cfg.kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	specs, err := serverstate.NewSpecStore(serverstate.SpecStoreOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, nil, err
	}
	history, err := serverstate.NewHistoryStore(serverstate.HistoryOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
		Keep:      cfg.keep,
	})
	if err != nil {
		return nil, nil, err
	}
	logs, err := observation.NewLogQuery(observation.LogQueryConfig{Client: cluster.Typed})
	if err != nil {
		return nil, nil, err
	}
	// The audit trail (issue #78, ADR-0026). It rides the startup clientset for
	// the same reason the other state stores do: ConfigMaps in one namespace,
	// no discovery mapper involved. A zero retention leaves it nil, which makes
	// every capture point a no-op and AuditService answer unimplemented — the
	// pre-#78 posture, chosen deliberately rather than arrived at.
	var audit api.AuditSink
	if cfg.auditRetention != 0 {
		store, err := serverstate.NewAuditStore(serverstate.AuditOptions{
			Client:     cluster.Typed,
			Namespace:  cfg.namespace,
			RetainDays: cfg.auditRetention,
		})
		if err != nil {
			return nil, nil, err
		}
		audit = store
	}
	// The agent identity store rides the startup clientset for the same reason
	// the state stores do: it only ever gets, lists, creates and updates
	// Secrets in one namespace, so no discovery mapper is involved (issue #74).
	// It is returned as well as wired in because the HTTP gate needs it too —
	// the gate resolves the credential and the handlers issue them, and both
	// must be looking at the same store or a revocation would be visible to
	// only one of them.
	agents, err := serverstate.NewAgentStore(serverstate.AgentStoreOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, nil, err
	}

	return api.New(api.Options{
		Specs:  specs,
		Agents: agents,
		Audit:  audit,
		Logger: attribution,
		Profile: api.CaptureFunc(func(context.Context) (clusterprofile.ClusterProfile, error) {
			return detect.FromCluster(cfg.kubeconfig)
		}),
		Delivery: deliveryConnector(cfg, history),
		Preview:  previewConnector(cfg),
		Logs:     api.LogQueryEngine{Engine: logs},
		Build:    buildConnector(cfg),
		// The secret backend rides the startup clientset for the same reason
		// the state stores do: it only ever gets, lists, applies and deletes
		// Secrets, so no discovery mapper is involved and nothing about it goes
		// stale when a CRD is registered later (issue #116).
		Secrets: secret.New(cluster.Typed),
		BuildDefaults: api.BuildDefaults{
			Registry:   cfg.registry,
			PushSecret: cfg.pushSecret,
			Namespace:  cfg.buildNamespace,
		},
	}), agents, nil
}

// deliveryConnector is the server's connectDelivery: one cluster connection per
// request, the direct adapter over the cluster-backed history store, the flux
// adapter when the environment names a deployment repository, and an
// observation probe for status verdicts. It mirrors cmd/kelson/deploy.go's
// function of the same purpose — the difference is where history lives.
func deliveryConnector(cfg config, history *serverstate.HistoryStore) api.DeliveryConnector {
	return func(ctx context.Context, t api.Target) (*api.Plane, error) {
		cluster, err := kube.Connect(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		// WithContext hands the request's context through direct.History, which
		// is context-free; without it a cancelled RPC would leave a ConfigMap
		// write in flight (ADR-0013 §1).
		store := history.WithContext(ctx)

		reg := delivery.NewRegistry()
		if _, err := direct.RegisterDirect(reg, direct.Options{
			Client:  cluster.Dynamic,
			Mapper:  cluster.Mapper,
			History: store,
			// A failed release command quotes its own output back (issue #104),
			// which needs the typed client this connection already has.
			Logs: observation.ClientGoLogSource{Client: cluster.Typed},
		}); err != nil {
			return nil, err
		}
		if t.Git != nil && t.Git.Repo != "" {
			if _, err := flux.RegisterFlux(reg, flux.Options{
				Writer: git.Config{
					Target:   git.Target{Repo: t.Git.Repo, Branch: t.Git.Branch, Path: t.Git.Path},
					Mode:     git.ModeCommit,
					Identity: git.IdentityFromEnv(nil),
					Auth:     gitAuth(),
				},
				Reconciler: flux.AnnotationReconciler{Client: cluster.Dynamic},
				Status:     flux.DynamicStatusReader{Client: cluster.Dynamic, FluxOperator: t.FluxOperator},
			}); err != nil {
				return nil, err
			}
		}

		probe, err := observation.NewProbe(observation.ProbeConfig{
			Client: cluster.Dynamic,
			Logs:   observation.ClientGoLogSource{Client: cluster.Typed},
		})
		if err != nil {
			return nil, err
		}
		return &api.Plane{
			Registry: reg,
			Health:   probe,
			Recorded: &rollback.DirectSource{Store: store, Project: t.Project, Environment: t.Environment},
			// The preview reader is wired unconditionally, unlike the flux
			// adapter above: reading which pull requests are running needs no
			// deployment repository and no write credential, only the cluster
			// this connection already opened. An environment whose mode cannot
			// run previews never reaches here — the gate answers first
			// (internal/api/preview.go).
			Previews: flux.DynamicStatusReader{Client: cluster.Dynamic, FluxOperator: t.FluxOperator},
		}, nil
	}
}

// buildConnector is the server's connectBuild: one cluster connection per
// build, the BuildKit driver over the Kubernetes build executor, and a remote
// ref resolver sharing the delivery credential. It is cmd/kelson/build.go's
// connectBuild with the CLI's kubeconfig flag replaced by the server's
// (issues #48, #54).
//
// The Job's deadline and the RPC's budget are the same constant deliberately.
// If the Job outlived the watch, a cancelled stream would leave a build running
// with nothing left that could ever report its outcome; if the watch outlived
// the Job, the server would wait past the moment the answer became impossible.
func buildConnector(cfg config) api.BuildConnector {
	return func(_ context.Context, t api.BuildTarget) (*api.BuildPlane, error) {
		cluster, err := kube.Connect(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		driver, err := buildkit.New(buildkit.Options{
			Cluster: kube.NewBuildExecutor(cluster.Typed),
			Config: buildkit.Config{
				Namespace:  t.Namespace,
				PushSecret: t.PushSecret,
				Timeout:    buildkit.Duration(api.DefaultBuildTimeout),
			},
		})
		if err != nil {
			return nil, err
		}
		return &api.BuildPlane{
			Builder: driver,
			// The source repository and the deployment repository are commonly
			// the same forge, so the build reuses the delivery credential
			// rather than inventing a second one — the CLI's choice, unchanged.
			Revisions: git.RemoteResolver{Auth: gitAuth()},
		}, nil
	}
}

// previewConnector builds the L2 dry-run engine for --dry-run=server previews.
func previewConnector(cfg config) api.PreviewConnector {
	return func(_ context.Context, profile clusterprofile.ClusterProfile) (api.PreviewEngine, error) {
		cluster, err := kube.Connect(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		return dryrun.New(dryrun.Options{Client: cluster.Dynamic, Mapper: cluster.Mapper, ClusterProfile: profile})
	}
}

// gitAuth reads the delivery credential from the environment, exactly as the
// CLI does. A missing token is anonymous, which is correct for public remotes
// and fails loudly at push time for anything else.
func gitAuth() git.Auth {
	if token := strings.TrimSpace(os.Getenv("KELSON_GIT_TOKEN")); token != "" {
		return git.Token{Token: token}
	}
	return git.Anonymous{}
}
