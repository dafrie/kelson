// Command kelson-server serves the kelson v1alpha1 schema over ConnectRPC
// (issue #139, ADR-0013).
//
// # The process holds no state
//
// Specs are `kelson.dev/v1alpha1` Project and Environment custom resources in
// the namespace the server runs against (--namespace, default kelson-system),
// written with server-side apply (ADR-0027 decision 6); agent identities and the
// audit ring are Secrets and ConfigMaps in the same namespace. A restart or a
// second replica loses and forks nothing, which is why there is no volume, no
// migration and no leader election here (ADR-0013 §1).
//
// # Deploying is not this process's job any more
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) made delivery a controller loop:
// kelson-controller renders an Environment, publishes an OCI artifact and lets
// Flux reconcile it. This server renders, validates, previews, builds, reads
// logs and stores specs; the RPCs that used to apply, roll back or list history
// answer `CodeUnimplemented` with a `delivery/not-implemented` detail naming
// issue #224 until the controller publishes.
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
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/buildkit"
	"github.com/dafrie/kelson/internal/build/buildpacks"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery/dryrun"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/forgehttp"
	"github.com/dafrie/kelson/internal/gitref"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/preview"
	"github.com/dafrie/kelson/internal/secret"
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

	// auditRetention is how many UTC days of audit records to keep. Zero
	// selects the store's default; a negative value turns the trail off.
	auditRetention int

	// password is the single shared secret web and non-browser clients
	// authenticate with (#84's interim cut, ADR-0013 §3). Empty disables
	// authentication entirely, which is the pre-#84 behaviour.
	password string

	// externalURL is where this server is reachable from outside: what the
	// GitHub App's webhook and redirect URLs are built from (ADR-0033 decision
	// 2). Empty derives it per request from the Host header and the forwarded
	// scheme, which is right behind a proxy that sets them.
	//
	// It is a flag rather than something detected because the value is baked
	// into the app GitHub creates: an app made with the wrong webhook URL has
	// to be deleted and remade, and a guess that is usually right is not good
	// enough for a value nobody can correct afterwards.
	externalURL string

	// The build plane's destination configuration. It is flags and not spec for
	// ADR-0010's reason (docs/build.md): where an image is pushed is
	// infrastructure, and the same Project must build against a team's ghcr.io
	// and against a kind cluster's localhost:5000.
	registry       string
	pushSecret     string
	buildNamespace string

	// registryConfig is a docker config.json holding the credential the
	// server-side preview publish authenticates with (ADR-0034 decision 3). It
	// is a *file* and not a Secret name, unlike --push-secret beside it,
	// because the two pushes happen in different places: a build Job pushes
	// from a pod and takes a Secret reference the kubelet projects, and this
	// process pushes an artifact itself. kelson-controller reads the same
	// mounted file for the same reason, under the same flag name.
	registryConfig string
	// insecureRegistries are registry hosts served over plain HTTP. It is an
	// operator knob and never a request field: a caller that could name a
	// registry insecure could make this server push a credential in clear to
	// any host it chose.
	insecureRegistries []string
}

// registryEnv supplies --registry, so an operator sets the destination once in
// the deployment rather than in every request. It is the same variable
// `kelson build` reads.
const registryEnv = "KELSON_REGISTRY"

// insecureRegistriesEnv supplies --insecure-registries, and is the same
// variable `kelson build` reads for the same reason: which registries have no
// TLS is a property of the cluster this server serves.
const insecureRegistriesEnv = "KELSON_INSECURE_REGISTRIES"

// registryConfigEnv supplies --registry-config, and defaultRegistryConfig is
// where a chart mounts it. Both are kelson-controller's, spelled identically:
// the two processes push to the same registries with the same credential, and
// an operator who has mounted one Secret should not discover that the other
// half of the control plane wanted it somewhere else.
const (
	registryConfigEnv     = "KELSON_REGISTRY_CONFIG"
	defaultRegistryConfig = "/etc/kelson/registry/config.json"
)

// envOr is the flag default when an environment variable may supply it and a
// built-in default exists behind that.
func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// passwordEnv supplies --password. It is the preferred way to set it: a flag
// value is visible in `ps` and in shell history, an environment variable is at
// least only readable by the process's owner. Same variable kelson-mcp reads.
const passwordEnv = "KELSON_PASSWORD"

// externalURLEnv supplies --external-url. It is where a deployment normally
// sets it: the Ingress host is known to whoever wrote the chart values and not
// to whoever runs the binary.
const externalURLEnv = "KELSON_EXTERNAL_URL"

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

	plane, err := connectServer(cfg, attribution)
	if err != nil {
		return err
	}

	auth, err := api.NewAuthWith(api.AuthOptions{Password: cfg.password, Agents: plane.agents})
	if err != nil {
		return err
	}

	// The bootstrap credential is deprecated, and a deprecation nobody is told
	// about is a deprecation nobody acts on (ADR-0033 decision 5). It goes to
	// stderr beside the bind warning rather than onto the banner: the banner
	// states the posture this process is in, and this states one an operator
	// should leave.
	if notice := plane.sources.Bootstrap.Deprecation(); notice != "" {
		_, _ = fmt.Fprintln(stderr, "warning:", notice)
	}

	// A plain net/http server, no h2c. The Connect protocol carries unary and
	// server-streaming RPCs over HTTP/1.1 — only client- and bidi-streaming
	// need HTTP/2, and the v1alpha1 schema has neither (deploy, rollback and
	// log-follow are all server-streaming). h2c would also mean
	// golang.org/x/net, which the api plane's depguard rule does not allow
	// (ADR-0013 §4: the fence extends rather than loosens). Adding gRPC-client
	// support later is an http2 server, not a schema change.
	srv := &http.Server{
		Handler: newMux(plane, auth),
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
		strings.Join([]string{authBanner(auth), auditBanner(cfg), sourceBanner(plane), webBanner()}, ", "))

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
	fs.IntVar(&cfg.auditRetention, "audit-retention", controlstore.DefaultAuditRetentionDays,
		fmt.Sprintf("days of audit records to retain (maximum %d); 0 disables the audit trail, which is a choice to make out loud (issue #78)",
			controlstore.MaxAuditRetentionDays))
	fs.StringVar(&cfg.password, "password", os.Getenv(passwordEnv),
		"shared password web clients log in with and non-browser clients send as a bearer token (default: $"+passwordEnv+"); empty disables authentication")
	fs.StringVar(&cfg.externalURL, "external-url", os.Getenv(externalURLEnv),
		"base URL this server is reachable at from the internet, e.g. https://kelson.acme.com (default: $"+externalURLEnv+"); "+
			"the GitHub App's webhook and callback URLs are built from it and are baked into the created app. "+
			"Empty derives it from each request's Host header and forwarded scheme")
	fs.StringVar(&cfg.registry, "registry", os.Getenv(registryEnv), "destination registry and namespace for builds, e.g. ghcr.io/acme (default: $"+registryEnv+"); a Build request may override it")
	fs.StringVar(&cfg.pushSecret, "push-secret", "", "name of an existing kubernetes.io/dockerconfigjson Secret in the build namespace that authenticates the push")
	fs.StringVar(&cfg.buildNamespace, "build-namespace", "", "namespace build Jobs run in (default: the environment's own namespace, as in the CLI)")
	fs.StringVar(&cfg.registryConfig, "registry-config", envOr(registryConfigEnv, defaultRegistryConfig),
		"path to a docker config.json holding the credential preview artifacts are published with (default: $"+registryConfigEnv+
			", then "+defaultRegistryConfig+"); a file that is not there is an anonymous push")
	insecure := fs.String("insecure-registries", os.Getenv(insecureRegistriesEnv),
		"comma-separated registry hosts served over plain HTTP, e.g. localhost:5000 (default: $"+insecureRegistriesEnv+"); only the listed hosts are affected")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	// Parsed at startup rather than per build: a typo here would otherwise
	// surface as a TLS error on the first push, pointing at the registry
	// instead of at the flag.
	hosts, err := registry.ParseInsecure(*insecure)
	if err != nil {
		return config{}, fmt.Errorf("--insecure-registries: %w", err)
	}
	cfg.insecureRegistries = hosts
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-server takes flags only", fs.Arg(0))
	}
	if cfg.namespace == "" {
		return config{}, errors.New("--namespace must not be empty: it is where the Project and Environment resources, the agent identities and the audit ring live")
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
func newMux(plane *serverPlane, auth *api.Auth) http.Handler {
	mux := http.NewServeMux()
	plane.api.Register(mux)
	auth.Register(mux)
	// The forge surface (ADR-0033 decision 2, ADR-0034 decisions 1 and 2):
	// /forge/github/webhook and the three parts of the app-manifest flow. It
	// sits beside the RPC routes rather than inside internal/api because none
	// of it is an RPC — a webhook body's schema is GitHub's, and the manifest
	// flow is browser redirects and a form.
	//
	// Like /healthz and /auth/*, it is outside the gate's `/kelson.v1alpha1.*`
	// prefix, so it carries its own (issue #248). The webhook's is the HMAC over
	// the body. The manifest flow's is the check handed in here — the RPC
	// routes' own, so "authenticated exactly like an RPC" is one implementation
	// rather than two — which gates the session endpoint that mints the
	// one-time ticket /start then requires.
	//
	// The check is injected at this line because this is the first moment both
	// halves exist: the gate is built from the agent store connectServer made,
	// so the forge Handler could not have been given it at construction.
	if plane.forge != nil {
		plane.forge.Register(mux, auth.CheckRequest)
	}
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

// sourceBanner says how this process authenticates to the repositories it
// reads (ADR-0033). It is on the banner because "a private repository will not
// clone" is the failure the whole decision exists to fix, and an operator who
// has connected no forge and set no bootstrap variable should learn that here
// rather than from a 404 on their first build.
func sourceBanner(plane *serverPlane) string {
	if plane.sources.Bootstrap != nil {
		return "source credentials: git connections, plus the deprecated " + forgeconn.BootstrapEnv
	}
	return "source credentials: git connections"
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

// serverPlane is what connectServer built: the RPC server plus the pieces
// run() and the mux need in their own right.
//
// It is a struct rather than a growing return list because the things beside
// the api.Server are not incidental — the agent store is what the HTTP gate
// resolves credentials against, and the connection resolver is what the source
// credential, the build clone and the forge endpoints all share. One value that
// says "this is the wiring" beats four returns nobody can name at the call
// site.
type serverPlane struct {
	api    *api.Server
	agents *controlstore.AgentStore

	// sources answers "which connection covers this repository, and what does
	// it mint" (ADR-0033 decision 4). It is never nil: with no cluster
	// connections and no bootstrap token it resolves everything to anonymous,
	// which is exactly what this server did before connections existed.
	sources *forgeconn.Resolver

	// forge serves /forge/* — the webhook listener and the app-manifest flow.
	// Nil is a server that answers 404 there, which a test mux is.
	forge *forgehttp.Handler
}

// connectServer builds the api.Server over the real cluster.
//
// The typed client is made once, at startup: the state stores only ever read
// and write ConfigMaps, and a cached discovery mapper is not involved. The
// delivery plane is rebuilt per request instead — kube.Connect's REST mapper is
// discovery-backed and never refreshed, so a long-running process reusing one
// would not see a CRD registered after it started (see kube.Connect's doc).
func connectServer(cfg config, attribution *slog.Logger) (*serverPlane, error) {
	cluster, err := kube.Connect(cfg.kubeconfig)
	if err != nil {
		return nil, err
	}
	// The spec store speaks to custom resources, which needs a client the
	// typed clientset above cannot give: controlstore builds it from the same
	// resolved credentials so the two cannot disagree about which cluster this
	// is (ADR-0027 decision 6).
	crClient, err := controlstore.NewClient(cluster.Config)
	if err != nil {
		return nil, err
	}
	specs, err := controlstore.NewSpecStore(controlstore.SpecStoreOptions{
		Client:    crClient,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}
	// The delivery verbs read `Environment.status` (ADR-0027 decision 6): the
	// phase, the revision, the history mirror and the rollback pin. It shares
	// the spec store's client — the same resolved credentials, the same
	// namespace — because a server whose two halves disagreed about which
	// cluster holds a project would be worse than one that could not read
	// either.
	environments, err := controlstore.NewEnvironmentStore(controlstore.EnvironmentStoreOptions{
		Client:    crClient,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}
	// The forge connections (ADR-0033, issue #248). Same client and same
	// namespace as the two stores above: a connection is a custom resource in
	// kelson's own namespace, and the Secret it references is beside it — which
	// is why this store, alone in the package, reads a Secret the *user* wrote
	// rather than one kelson keeps state in.
	connections, err := controlstore.NewGitConnectionStore(controlstore.GitConnectionStoreOptions{
		Client:    crClient,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}
	// The instance's tier of declared sources (ADR-0035 decision 2). Same
	// client and same namespace again: a GitSource is a custom resource beside
	// the connections, and the build path hands them to the resolver so a
	// component bound to one by name resolves to a repository. Read-only —
	// nothing in the schema authors one.
	gitSources, err := controlstore.NewGitSourceStore(controlstore.GitSourceStoreOptions{
		Client:    crClient,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}
	// One resolver over that store, shared by everything that needs a forge
	// credential: the ref resolver, the build pod's clone, and the endpoints
	// under /forge. `KELSON_GIT_TOKEN` joins it as an implicit connection rather
	// than as a second code path beside it (ADR-0033 decision 5).
	sources := &forgeconn.Resolver{
		Store:     connections,
		Bootstrap: forgeconn.NewBootstrap(os.Getenv(forgeconn.BootstrapEnv), ""),
	}
	logs, err := observation.NewLogQuery(observation.LogQueryConfig{Client: cluster.Typed})
	if err != nil {
		return nil, err
	}
	// The audit trail (issue #78, ADR-0026). It rides the startup clientset for
	// the same reason the other state stores do: ConfigMaps in one namespace,
	// no discovery mapper involved. A zero retention leaves it nil, which makes
	// every capture point a no-op and AuditService answer unimplemented — the
	// pre-#78 posture, chosen deliberately rather than arrived at.
	var audit api.AuditSink
	if cfg.auditRetention != 0 {
		store, err := controlstore.NewAuditStore(controlstore.AuditOptions{
			Client:     cluster.Typed,
			Namespace:  cfg.namespace,
			RetainDays: cfg.auditRetention,
		})
		if err != nil {
			return nil, err
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
	agents, err := controlstore.NewAgentStore(controlstore.AgentStoreOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}

	// The preview trigger's ResourceSetInputProvider poke (ADR-0034 decision
	// 2). One value, two triggers: the webhook path below stamps the same
	// annotation on the same object, and building two would be two answers to
	// "how does kelson ask flux-operator to look now".
	poker := flux.InputProviderPoker{Client: cluster.Dynamic}

	server := api.New(api.Options{
		Specs:        specs,
		Environments: environments,
		Agents:       agents,
		Audit:        audit,
		Connections:  connections,
		GitSources:   gitSources,
		Logger:       attribution,
		// The ReportBuild trigger (ADR-0034 decision 3). The publisher is
		// internal/preview's — the same package `kelson preview publish` calls,
		// which is what ADR-0017 decision 10 promised a server-side caller
		// would be — and it needs no cluster connection of its own: it renders,
		// packages and speaks the distribution API with the credential in the
		// mounted docker config.
		Publish: &preview.Publisher{
			RegistryConfig:     cfg.registryConfig,
			InsecureRegistries: cfg.insecureRegistries,
		},
		Poke:     poker,
		Statuses: forgeStatuses{sources: sources, externalURL: cfg.externalURL},
		Profile: api.CaptureFunc(func(context.Context) (clusterprofile.ClusterProfile, error) {
			return detect.FromCluster(cfg.kubeconfig)
		}),
		// The installer is rebuilt per request for the reason the
		// cluster-reading plane is: its REST mapper is discovery-backed and
		// never refreshed, and installing is exactly the operation that
		// registers new CRDs.
		Install: func(context.Context) (api.Installer, error) {
			c, err := kube.Connect(cfg.kubeconfig)
			if err != nil {
				return nil, err
			}
			return install.New(install.Options{
				Client: c.Dynamic,
				Mapper: c.Mapper,
				Fetch:  install.HTTPFetcher{},
			})
		},
		// The node inventory rides the startup clientset like the state stores
		// do — nodes and metrics.k8s.io involve no discovery mapper.
		Nodes:    observation.NodeSource{Typed: cluster.Typed, Dynamic: cluster.Dynamic},
		Delivery: observationConnector(cfg),
		Preview:  previewConnector(cfg),
		Logs:     api.LogQueryEngine{Engine: logs},
		Build:    buildConnector(cfg, sources),
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
	})

	// The /forge surface. It shares the connection resolver with the build
	// plane above, the spec store with the RPC handlers, and the dynamic client
	// with everything else that talks to Flux objects — one process, one answer
	// to "which connections does this instance hold".
	forgeEndpoints, err := forgehttp.New(forgehttp.Options{
		Sources:     sources,
		Connections: connections,
		Specs:       specs,
		Secrets:     forgehttp.KubeSecrets{Client: cluster.Typed, Namespace: cfg.namespace},
		Previews:    poker,
		ExternalURL: cfg.externalURL,
		Logger:      attribution,
	})
	if err != nil {
		return nil, err
	}

	return &serverPlane{api: server, agents: agents, sources: sources, forge: forgeEndpoints}, nil
}

// observationConnector is the server's connectObservation: one cluster
// connection per request and a workload probe over it, mirroring
// cmd/kelson/deploy.go's function of the same purpose.
//
// It used to assemble a delivery registry as well — the direct adapter over the
// cluster-backed history store, the flux adapter when the environment named a
// deployment repository — and that is what ADR-0028 deleted. What survives is
// the half that reads the cluster: the workload probe behind Status and
// Explain, and the preview reader behind ListPreviews.
func observationConnector(cfg config) api.DeliveryConnector {
	return func(_ context.Context, _ api.Target) (*api.Plane, error) {
		cluster, err := kube.Connect(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		probe, err := observation.NewProbe(observation.ProbeConfig{
			Client: cluster.Dynamic,
			Logs:   observation.ClientGoLogSource{Client: cluster.Typed},
		})
		if err != nil {
			return nil, err
		}
		return &api.Plane{
			Health: probe,
			// Reading which pull requests are running needs no deployment
			// repository and no write credential, only the cluster this
			// connection already opened. FluxOperator is nil because nothing
			// resolves a ClusterProfile on this path any more; the reader
			// probes rather than acting on an absence nobody established
			// (internal/delivery/flux/status.go).
			Previews: flux.DynamicStatusReader{Client: cluster.Dynamic},
		}, nil
	}
}

// buildConnector is the server's connectBuild: one cluster connection per
// build, the driver the resolved strategy selects over the Kubernetes build
// executor, and a remote ref resolver reading through the project's connection.
// It is cmd/kelson/build.go's connectBuild with the CLI's kubeconfig flag
// replaced by the server's (issues #48, #49, #54).
//
// The Job's deadline and the RPC's budget are the same constant deliberately.
// If the Job outlived the watch, a cancelled stream would leave a build running
// with nothing left that could ever report its outcome; if the watch outlived
// the Job, the server would wait past the moment the answer became impossible.
//
// # Why the connection arrives on the target rather than being looked up here
//
// It used to be read back out of the stored Project: `spec.source.connection`
// is the author's override, api.BuildTarget carried only the project's *name*,
// and a target that carried a connection would have put a credential-selection
// decision in a request field. ADR-0035 makes that reading wrong rather than
// merely indirect — a project may declare several sources with a connection
// each, and which one a build reads is decided by the component bindings the
// handler resolved, not by the first `source:` in the document. So the handler
// resolves it and the target carries it, still a resolution of the spec and
// still never a field the caller may set. A source that names none resolves by
// host match, which is the zero-configuration case ADR-0033 decision 4 is
// written for.
func buildConnector(cfg config, sources *forgeconn.Resolver) api.BuildConnector {
	return func(_ context.Context, t api.BuildTarget) (*api.BuildPlane, error) {
		cluster, err := kube.Connect(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		credentials := gitref.Connections{Resolver: sources, Connection: t.SourceConnection}
		driver, err := buildDriver(cfg, t, kube.NewBuildExecutor(cluster.Typed),
			cloneCredentials{sources: sources})
		if err != nil {
			return nil, err
		}
		return &api.BuildPlane{
			Builder: driver,
			// The same credential resolution the clone init container gets, so
			// "resolve the ref" and "fetch the commit" cannot disagree about
			// which connection a build acts as (ADR-0033 decisions 4 and 5).
			Revisions: gitref.RemoteResolver{Source: credentials},
		}, nil
	}
}

// There is no projectConnection here any more.
//
// It re-read the stored Project and returned `spec.source.connection`, which
// was the whole answer while a project had one source. ADR-0035 gives each
// declared source its own `connection:`, so the answer depends on which source
// the build's components are bound to — a question only the resolved spec can
// settle, and one internal/api's Build handler has already settled by the time
// this connector runs. It arrives on api.BuildTarget.SourceConnection instead,
// which also removes a second decode of a document the handler was holding.

// forgeStatuses writes a delivery outcome back onto a commit
// ([ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 5).
//
// It lives here rather than in internal/api for the reason every seam
// implementation does: joining a repository to the connection that covers it
// and minting a token from that connection's Secret is internal/forgeconn's,
// which reads cluster state. What the handler holds is [api.CommitStatus] and
// an interface.
//
// # Everything it cannot do is silent
//
// No connection covers the repository, or the connection's provider has no
// StatusReporter — a `generic` token connection has none — and nothing is
// written and nothing fails. ADR-0034 makes that the rule rather than an
// accident: "statuses are a courtesy of the integration, not a delivery
// dependency", and a preview that published is a preview that published even if
// the forge never hears about it.
//
// What is *not* silent is a resolution that failed rather than found nothing —
// two connections covering one host, a Secret that cannot be read, a provider
// no adapter speaks. Something is configured and broken there, and the report's
// message says so.
type forgeStatuses struct {
	sources *forgeconn.Resolver
	// externalURL is where this server is reachable from outside. Empty sends
	// the status with no link rather than a guessed one: internal/forgehttp can
	// derive an origin from a request's Host header because it *has* a request,
	// and an RPC handler publishing an artifact has none.
	externalURL string
}

// ReportCommitStatus implements api.CommitStatusReporter.
func (f forgeStatuses) ReportCommitStatus(ctx context.Context, s api.CommitStatus) error {
	if f.sources == nil {
		return nil
	}
	// No `source.connection` override is consulted, and that is deliberate:
	// the field pins which connection the project's *source* is read with, and
	// the repository here is `previews.repo`, which ADR-0017 decision 1 keeps
	// separate from it with no defaulting either way. Host matching is the
	// zero-configuration path ADR-0033 decision 4 is written for.
	res, ok, err := f.sources.Resolve(ctx, s.Repo, "")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	reporter, ok := res.Provider.(forge.StatusReporter)
	if !ok {
		return nil
	}
	return reporter.ReportStatus(ctx, res.Conn, s.FullName, s.SHA, forge.Status{
		State:       s.State,
		Context:     s.Context,
		Description: s.Description,
		TargetURL:   f.link(s.Path),
	})
}

func (f forgeStatuses) link(path string) string {
	base := strings.TrimRight(strings.TrimSpace(f.externalURL), "/")
	if base == "" || path == "" {
		return ""
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

// cloneCredentials mints the credential a build pod's clone init container
// fetches with (ADR-0033 decision 5, internal/build's CloneAuth).
//
// It is the same [forgeconn.Resolver] and the same `connection:` the ref
// resolver beside it uses, on purpose: "which commit does main name" and "fetch
// that commit" are the same repository read, and answering them through two
// credentials would make a build that resolves and then cannot fetch — which is
// precisely the gap ADR-0033's Context describes.
//
// The connection is read off the Request rather than held here, because it is a
// property of the *source* since ADR-0035: the plane is assembled once per
// build and the credential is chosen per repository, so the Request — which is
// what names the repository — is where the two must agree.
type cloneCredentials struct {
	sources *forgeconn.Resolver
}

// CloneCredential implements build.CloneAuth. A source no connection covers
// mints nothing and the clone stays anonymous, which is correct for a public
// repository and is what every build did before this existed.
func (c cloneCredentials) CloneCredential(ctx context.Context, req build.Request) (build.CloneCredential, error) {
	if c.sources == nil {
		return build.CloneCredential{}, nil
	}
	cred, _, ok, err := c.sources.Credential(ctx, req.SourceGit, req.SourceConnection)
	if err != nil || !ok {
		return build.CloneCredential{}, err
	}
	return build.CloneCredential{Username: cred.Username, Password: cred.Password}, nil
}

// buildExecutor is what both drivers need from the cluster: submit a rendered
// build Job and stream it to completion. kube.BuildExecutor satisfies
// buildkit.Cluster and buildpacks.Cluster structurally, and one executor
// serves both because it reads what a build pushed off the Job's annotations
// rather than out of a builder's argv.
type buildExecutor interface {
	Submit(ctx context.Context, manifest []byte) (string, error)
	Wait(ctx context.Context, name string, w io.Writer) (build.Result, error)
}

// buildDriver constructs the driver the resolved strategy selects (ADR-0010),
// mirroring cmd/kelson's function of the same name. The strategy arrives on
// the target because it was decided by the plan both callers share; this only
// maps it to a driver, and refuses a strategy it has none for rather than
// falling back to one that would fail obscurely.
//
// The insecure-registry list comes from cfg rather than the target: it is what
// the operator started this server with, and no request may extend it.
func buildDriver(cfg config, t api.BuildTarget, cluster buildExecutor, clone build.CloneAuth) (build.Builder, error) {
	switch t.Strategy {
	case buildkit.StrategyName:
		return buildkit.New(buildkit.Options{
			Cluster:   cluster,
			CloneAuth: clone,
			Config: buildkit.Config{
				Namespace:          t.Namespace,
				PushSecret:         t.PushSecret,
				InsecureRegistries: cfg.insecureRegistries,
				Timeout:            buildkit.Duration(api.DefaultBuildTimeout),
			},
		})
	case buildpacks.StrategyName:
		// No Rebaser: the server exposes no rebase RPC, and Driver.Rebase
		// fails closed without one rather than pretending to patch a run image.
		return buildpacks.New(buildpacks.Options{
			Cluster:   cluster,
			CloneAuth: clone,
			Config: buildpacks.Config{
				Namespace:          t.Namespace,
				PushSecret:         t.PushSecret,
				InsecureRegistries: cfg.insecureRegistries,
				Timeout:            buildpacks.Duration(api.DefaultBuildTimeout),
			},
		})
	default:
		return nil, fmt.Errorf("no build driver for strategy %q", t.Strategy)
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

// There is no sourceAuth here any more.
//
// It read `KELSON_GIT_TOKEN` and handed the same credential to every repository
// this process touched. ADR-0033 decision 4 replaces that with the project's
// own connection, resolved by host or named outright, and decision 5 keeps the
// variable alive as one implicit connection *inside* that resolution rather
// than as a branch beside it (internal/forgeconn.Bootstrap). One credential
// story, not two — which is what makes the deprecation notice on startup a
// thing an operator can act on rather than a warning about a path that is
// still special.
