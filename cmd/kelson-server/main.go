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
// # v0 is loopback-only, and says so
//
// There is no authentication and no TLS. That is not a security posture, it is
// the absence of one: the server binds 127.0.0.1 by default and refuses a
// non-loopback --listen without --insecure-bind. The threat model (#84) owns
// the real answer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dafrie/kelson/internal/api"
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
	"github.com/dafrie/kelson/internal/serverstate"
	"github.com/dafrie/kelson/internal/version"
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
}

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
	if err := checkBind(cfg.listen, cfg.insecureBind); err != nil {
		return err
	}

	server, err := connectServer(cfg)
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
		Handler: newMux(server),
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
	_, _ = fmt.Fprintf(stdout, "kelson-server %s serving the kelson.v1alpha1 schema on http://%s (namespace %s)\n",
		version.String(), listener.Addr(), cfg.namespace)

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
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-server takes flags only", fs.Arg(0))
	}
	if cfg.namespace == "" {
		return config{}, errors.New("--namespace must not be empty: it is where the spec and history ConfigMaps live")
	}
	return cfg, nil
}

// checkBind enforces the v0 trust model: loopback unless the operator says
// otherwise, out loud (ADR-0013 §3).
func checkBind(listen string, insecure bool) error {
	if insecure {
		return nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q is not a host:port address: %w", listen, err)
	}
	if isLoopback(host) {
		return nil
	}
	return fmt.Errorf("--listen %s would expose kelson-server beyond loopback, and v0 has no authentication and no TLS "+
		"(ADR-0013 §3; the threat model is issue #84). Bind 127.0.0.1 and forward a port, or pass --insecure-bind to accept the risk deliberately", listen)
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

// newMux mounts the API and the health endpoint. It is separate from run so a
// test can exercise the routing without a cluster or a listener.
func newMux(server *api.Server) *http.ServeMux {
	mux := http.NewServeMux()
	server.Register(mux)
	mux.HandleFunc("/healthz", healthz)
	return mux
}

// healthz reports liveness and the build it is reporting for. Version is part
// of the answer because "the server is up" and "the server is the build you
// deployed" are different questions and an operator asks both at once.
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
func connectServer(cfg config) (*api.Server, error) {
	cluster, err := kube.Connect(cfg.kubeconfig)
	if err != nil {
		return nil, err
	}
	specs, err := serverstate.NewSpecStore(serverstate.SpecStoreOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
	})
	if err != nil {
		return nil, err
	}
	history, err := serverstate.NewHistoryStore(serverstate.HistoryOptions{
		Client:    cluster.Typed,
		Namespace: cfg.namespace,
		Keep:      cfg.keep,
	})
	if err != nil {
		return nil, err
	}
	logs, err := observation.NewLogQuery(observation.LogQueryConfig{Client: cluster.Typed})
	if err != nil {
		return nil, err
	}

	return api.New(api.Options{
		Specs: specs,
		Profile: api.CaptureFunc(func(context.Context) (clusterprofile.ClusterProfile, error) {
			return detect.FromCluster(cfg.kubeconfig)
		}),
		Delivery: deliveryConnector(cfg, history),
		Preview:  previewConnector(cfg),
		Logs:     api.LogQueryEngine{Engine: logs},
	}), nil
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
