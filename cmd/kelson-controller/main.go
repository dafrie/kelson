// Command kelson-controller reconciles the kelson.dev custom resources
// (ADR-0027 decision 2, ADR-0028 decision 1).
//
// # A hand-rolled main, on controller-runtime
//
// This builds a manager, registers two reconcilers and runs. There is no
// kubebuilder scaffold, and ADR-0027 decision 2 is a repository-shape argument
// rather than a taste one: kelson already has per-plane depguard fences,
// committed generated artifacts with drift tests, and its own schema pipeline.
// Adopting the scaffold means adopting a second Makefile convention, a second
// generated-artifact convention and a `config/` kustomize layout — a second
// place a reader has to look to find out what the API is. The library is taken
// whole; the scaffolding around it is not.
//
// # What it does today
//
// Validate, detect, resolve, render — steps 1 to 3 of ADR-0028's six. Publishing
// the OCI artifact and applying the Flux objects are behind an interface with a
// no-op implementation (internal/controller, issue #224), so this process reads
// specs and writes statuses and changes nothing else in the cluster. That is
// also why its RBAC is as small as it is (deploy/chart/kelson).
//
// # Leader election is off by default
//
// A controller that only writes statuses is safe to run twice: two replicas
// would compute the same verdict from the same spec and write the same status,
// and the second write is a no-op patch. That stops being true the moment
// delivery is real — two replicas pushing artifacts and applying Flux objects is
// a race — so the flag exists now and its default will change with #224 rather
// than being invented under pressure.
//
// # Logging
//
// One JSON object per line on stderr, the same shape kelson-server's attribution
// log has, so a cluster collecting both gets one format. controller-runtime logs
// through logr; slog is bridged into it rather than the reverse, because the
// process's own messages should look like every other kelson binary's.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controller"
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

// config is the controller's resolved command line.
type config struct {
	kubeconfig  string
	metricsAddr string
	probeAddr   string

	// leaderElect and leaderElectionNamespace decide whether two replicas may
	// both reconcile. See the package doc for why the default is off.
	leaderElect             bool
	leaderElectionNamespace string

	// clusterProfile is the path to a ClusterProfile document the renderer is
	// told about. It is the stub half of ADR-0028's step 2: live detection is
	// a cluster probe with its own caching and RBAC, and wiring it is separate
	// work. An empty path means an empty profile, which is the honest answer
	// for a controller that has not looked.
	clusterProfile string
}

// leaderElectionID is the name of the Lease two replicas would contend for. It
// is namespaced by the group so it cannot collide with another operator's.
const leaderElectionID = "controller.kelson.dev"

// defaultNamespace is where the Lease goes when leader election is on and no
// namespace is named. It matches kelson-server's default and the namespace the
// chart installs into.
const defaultNamespace = "kelson-system"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	// One JSON object per line on stderr, bridged into logr because that is the
	// interface controller-runtime's API speaks.
	handler := slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	ctrllog.SetLogger(logr.FromSlogHandler(handler))

	profile, err := loadProfile(cfg.clusterProfile)
	if err != nil {
		return err
	}

	restCfg, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}

	s, err := newScheme()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  s,
		Metrics:                 metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:  cfg.probeAddr,
		LeaderElection:          cfg.leaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: cfg.leaderElectionNamespace,
	})
	if err != nil {
		return fmt.Errorf("building the manager: %w", err)
	}

	if err := (&controller.ProjectReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering the project reconciler: %w", err)
	}
	if err := (&controller.EnvironmentReconciler{
		Client:   mgr.GetClient(),
		Profiles: controller.StaticProfileSource{ClusterProfile: profile},
		Delivery: controller.NoopDeliverer{},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering the environment reconciler: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the liveness check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the readiness check: %w", err)
	}

	// The banner is courtesy, not a result: a failed write to stdout must not
	// stop a controller that is otherwise ready to run.
	_, _ = fmt.Fprintf(stdout, "kelson-controller %s reconciling %s/%s (%s, %s)\n",
		version.String(), v1alpha1.Group, v1alpha1.Version,
		leaderBanner(cfg), profileBanner(cfg))

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running the manager: %w", err)
	}
	return nil
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("kelson-controller", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0",
		"address the metrics endpoint binds to; 0 disables it")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081",
		"address the liveness and readiness endpoints bind to")
	fs.BoolVar(&cfg.leaderElect, "leader-elect", false,
		"contend for leadership so only one replica reconciles; off is safe while the controller only writes statuses (issue #224)")
	fs.StringVar(&cfg.leaderElectionNamespace, "leader-election-namespace", defaultNamespace,
		"namespace holding the leader-election Lease")
	fs.StringVar(&cfg.clusterProfile, "cluster-profile", "",
		"path to a ClusterProfile document the renderer is told about; empty means an empty profile (live detection is not wired yet)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-controller takes flags only", fs.Arg(0))
	}
	if cfg.leaderElect && cfg.leaderElectionNamespace == "" {
		return config{}, errors.New("--leader-election-namespace must not be empty when --leader-elect is set: it is where the Lease lives")
	}
	return cfg, nil
}

// newScheme registers the kinds this process serves. It is only kelson's two:
// the manager's client reads and writes nothing else, which is the same fence
// the RBAC in the chart draws.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("registering the kelson scheme: %w", err)
	}
	return s, nil
}

// restConfig resolves the cluster connection the same way every other kelson
// binary does: an explicit --kubeconfig wins, then $KUBECONFIG, then in-cluster
// credentials, then ~/.kube/config.
func restConfig(kubeconfig string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving the cluster connection: %w", err)
	}
	return cfg, nil
}

// loadProfile reads the ClusterProfile the renderer is told about. A missing
// file is an error rather than an empty profile: an operator who passed a path
// asked for that document, and silently rendering against an empty profile
// would emit manifests missing every optional resource, which looks intentional.
func loadProfile(path string) (clusterprofile.ClusterProfile, error) {
	if path == "" {
		return clusterprofile.ClusterProfile{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return clusterprofile.ClusterProfile{}, fmt.Errorf("--cluster-profile: %w", err)
	}
	profile, err := clusterprofile.Unmarshal(data)
	if err != nil {
		return clusterprofile.ClusterProfile{}, fmt.Errorf("--cluster-profile %s: %w", path, err)
	}
	return profile, nil
}

// leaderBanner says which posture the process started in, for the same reason
// kelson-server names its authentication posture on stdout: an operator who
// scaled to two replicas expecting leader election must not have to discover it
// was off by watching two controllers do the same work.
func leaderBanner(cfg config) string {
	if cfg.leaderElect {
		return "leader election: on, Lease " + leaderElectionID + " in " + cfg.leaderElectionNamespace
	}
	return "leader election: off"
}

// profileBanner says what the renderer will be told about the cluster. An empty
// profile is a real answer and it changes what renders, so it is said out loud.
func profileBanner(cfg config) string {
	if cfg.clusterProfile == "" {
		return "cluster profile: empty (detection is not wired yet)"
	}
	return "cluster profile: " + cfg.clusterProfile
}
