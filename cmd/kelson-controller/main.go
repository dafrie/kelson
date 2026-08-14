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
// # What it does
//
// All six steps of ADR-0028 decision 1: validate, detect, resolve and render,
// publish an immutable OCI artifact, server-side apply the OCIRepository and
// Kustomization pair that consume it, and read the result back into
// `Environment.status`. Everything below the flags lives in
// internal/controller.
//
// # One detection at start-up, and why the Flux watches depend on it
//
// The controller probes the cluster once before the manager is built
// (`internal/clusterprofile/detect`), and that finding does two jobs: it is the
// ClusterProfile the renderer is told about, and it decides whether the watches
// on Flux's own CRDs are registered at all. An informer on a CRD the API server
// does not serve does not fail — it blocks the manager's cache sync forever —
// so a cluster with no Flux would get a controller that never becomes ready and
// therefore never tells anyone that Flux is missing. Gating the watches is what
// keeps `FluxNotInstalled` a *status* on a running controller.
//
// The cost is stated where it is paid: installing Flux under a running
// controller needs a restart before the watches exist, and until then
// environments converge on the requeue timer. Live refresh of the profile is
// tracked separately.
//
// # Leader election is on by default
//
// This is a controller that publishes artifacts and applies Flux objects, so
// two replicas racing is real — and unlike a controller that only writes
// statuses, there is no version of "safe with two" that does not cost a Lease.
// The default was off while delivery did not exist yet; now that it does, a
// single-replica Deployment (still the chart's default) wins its own Lease
// immediately and pays nothing for it, which is what makes "on" the default
// that costs a scaled-down operator nothing and a scaled-up one everything.
// The flag still exists — --leader-elect=false is for a single hand-run
// instance with no Lease RBAC at all — and an operator who flips it is told on
// stdout which posture they started in.
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
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
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
	// both reconcile. See the package doc for why the default is on.
	leaderElect             bool
	leaderElectionNamespace string

	// clusterProfile is a path to a ClusterProfile document that *replaces* the
	// start-up detection. It is for a test, an air-gapped render or a cluster
	// the controller may not probe; the normal path is detection.
	clusterProfile string

	// registry is the push prefix artifacts go under:
	// <registry>/kelson/<project>-<environment>. Where an artifact is pushed is
	// infrastructure configuration and not application description (ADR-0028
	// decision 2), which is why it is a flag and not a spec field.
	registry string

	// registryConfig is a docker config.json holding the push credential,
	// mounted from a Secret. Absent is an anonymous push.
	registryConfig string

	// insecureRegistries are the hosts served over plain HTTP. An operator
	// knob, never a guess: silently downgrading a push because TLS failed is
	// how a credential ends up on the wire in clear.
	insecureRegistries []string

	// fluxNamespace holds the OCIRepository and Kustomization pair.
	fluxNamespace string

	// pullSecret names a dockerconfigjson Secret in fluxNamespace that
	// source-controller authenticates its *pull* with. It is a different
	// credential from registryConfig, in a different process.
	pullSecret string

	// reconcileInterval is what the two Flux objects reconcile on. It is drift
	// correction and not deploy latency: a new revision reaches Flux through an
	// apply, not through a poll.
	reconcileInterval time.Duration
}

// leaderElectionID is the name of the Lease two replicas would contend for. It
// is namespaced by the group so it cannot collide with another operator's.
const leaderElectionID = "controller.kelson.dev"

// defaultNamespace is where the Lease goes when leader election is on and no
// namespace is named. It matches kelson-server's default and the namespace the
// chart installs into.
const defaultNamespace = "kelson-system"

// The environment variables the flags fall back to. --registry and
// --insecure-registries read the same two variables `kelson build` and
// kelson-server read, because which registry a cluster pushes to and which
// hosts have no TLS are properties of the cluster rather than of the binary.
const (
	registryEnv           = "KELSON_REGISTRY"
	registryConfigEnv     = "KELSON_REGISTRY_CONFIG"
	insecureRegistriesEnv = "KELSON_INSECURE_REGISTRIES"
	fluxNamespaceEnv      = "KELSON_FLUX_NAMESPACE"
	pullSecretEnv         = "KELSON_PULL_SECRET"
)

// defaultRegistryConfig is where the chart mounts the push credential. It is a
// path and not a Secret name because the controller reads a file: the same
// docker config a `docker login` writes and the same parser `kelson preview
// publish` uses, so one credential shape serves both (internal/artifact).
const defaultRegistryConfig = "/etc/kelson/registry/config.json"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	// One JSON object per line on stderr, bridged into logr because that is the
	// interface controller-runtime's API speaks.
	handler := slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	ctrllog.SetLogger(logr.FromSlogHandler(handler))

	restCfg, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}

	profile, source, err := startupProfile(cfg)
	if err != nil {
		return err
	}

	s, err := newScheme()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  s,
		Cache:                   cacheOptions(cfg, profile),
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
		Delivery: &controller.FluxDeliverer{
			Client:             mgr.GetClient(),
			Registry:           cfg.registry,
			RegistryConfig:     cfg.registryConfig,
			InsecureRegistries: cfg.insecureRegistries,
			FluxNamespace:      cfg.fluxNamespace,
			PullSecret:         cfg.pullSecret,
			Interval:           cfg.reconcileInterval,
		},
	}).SetupWithManager(mgr, profile.Flux != nil); err != nil {
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
	_, _ = fmt.Fprintf(stdout, "kelson-controller %s reconciling %s/%s (%s, %s, %s, %s)\n",
		version.String(), v1alpha1.Group, v1alpha1.Version,
		leaderBanner(cfg), source, registryBanner(cfg), fluxBanner(cfg, profile))

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running the manager: %w", err)
	}
	return nil
}

// cacheOptions scopes the manager's cache.
//
// kelson's own kinds are watched cluster-wide, because Projects and
// Environments live wherever an application team puts them. The two Flux kinds
// are not: kelson owns exactly the pair in its own namespace, and a cache that
// listed every Kustomization in the cluster would hold every other team's
// GitOps state in this process's memory and re-reconcile on every change to it.
//
// The entries are only added when Flux is present, for the same reason the
// watches are (see EnvironmentReconciler.SetupWithManager): a cache entry for
// an unserved kind is an informer that never syncs.
func cacheOptions(cfg config, profile clusterprofile.ClusterProfile) cache.Options {
	if profile.Flux == nil {
		return cache.Options{}
	}
	byObject := map[client.Object]cache.ByObject{}
	for _, apiVersionKind := range [][2]string{
		{controller.OCIRepositoryAPIVersion, controller.KindOCIRepository},
		{controller.KustomizationAPIVersion, controller.KindKustomization},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(apiVersionKind[0])
		obj.SetKind(apiVersionKind[1])
		byObject[obj] = cache.ByObject{
			Namespaces: map[string]cache.Config{cfg.fluxNamespace: {}},
		}
	}
	return cache.Options{ByObject: byObject}
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
	fs.BoolVar(&cfg.leaderElect, "leader-elect", true,
		"contend for leadership so only one replica publishes and applies")
	fs.StringVar(&cfg.leaderElectionNamespace, "leader-election-namespace", defaultNamespace,
		"namespace holding the leader-election Lease")
	fs.StringVar(&cfg.clusterProfile, "cluster-profile", "",
		"path to a ClusterProfile document to use instead of probing the cluster")
	fs.StringVar(&cfg.registry, "registry", os.Getenv(registryEnv),
		"registry and namespace artifacts are published to, e.g. ghcr.io/acme (default: $"+registryEnv+")")
	fs.StringVar(&cfg.registryConfig, "registry-config", envOr(registryConfigEnv, defaultRegistryConfig),
		"path to a docker config.json holding the push credential (default: $"+registryConfigEnv+
			"); a missing file is an anonymous push")
	fs.StringVar(&cfg.fluxNamespace, "flux-namespace", envOr(fluxNamespaceEnv, controller.DefaultFluxNamespace),
		"namespace kelson's OCIRepository and Kustomization pair lives in (default: $"+fluxNamespaceEnv+")")
	fs.StringVar(&cfg.pullSecret, "pull-secret", os.Getenv(pullSecretEnv),
		"dockerconfigjson Secret in --flux-namespace that source-controller pulls artifacts with "+
			"(default: $"+pullSecretEnv+")")
	fs.DurationVar(&cfg.reconcileInterval, "reconcile-interval", controller.DefaultInterval,
		"how often Flux re-checks the objects kelson owns; this is drift correction, not deploy latency")
	insecure := fs.String("insecure-registries", os.Getenv(insecureRegistriesEnv),
		"comma-separated registry hosts served over plain HTTP, e.g. localhost:5000 (default: $"+
			insecureRegistriesEnv+"); only the listed hosts are affected")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-controller takes flags only", fs.Arg(0))
	}
	if cfg.leaderElect && cfg.leaderElectionNamespace == "" {
		return config{}, errors.New("--leader-election-namespace must not be empty when --leader-elect is set: it is where the Lease lives")
	}
	if strings.TrimSpace(cfg.fluxNamespace) == "" {
		return config{}, errors.New("--flux-namespace must not be empty: it is where kelson's OCIRepository and Kustomization live")
	}
	if cfg.reconcileInterval <= 0 {
		return config{}, fmt.Errorf("--reconcile-interval must be positive, got %s", cfg.reconcileInterval)
	}
	// A malformed entry is refused here rather than written into an
	// OCIRepository's insecure field, where it would match no registry at all
	// and surface as a TLS error pointing at the wrong thing.
	hosts, err := registry.ParseInsecure(*insecure)
	if err != nil {
		return config{}, fmt.Errorf("--insecure-registries: %w", err)
	}
	cfg.insecureRegistries = hosts
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// newScheme registers the kinds this process serves with a typed client.
//
// It is only kelson's two. The Flux objects are read and written as
// unstructured content on purpose (internal/delivery/flux says why): the Flux
// API types would each drag their own module and API-version pin into a control
// plane that needs six fields out of each object, and kelson's whole
// relationship with those projects is "consume the contract, import nothing".
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

// startupProfile answers "what does this cluster provide?", once, before the
// manager exists — because the answer decides how the manager is built.
//
// A failed probe is not fatal. Detection appends to a profile's Incomplete list
// rather than failing the whole capture (internal/clusterprofile), and the same
// discipline applies here: a controller that refused to start because it could
// not read one API group would be a controller that cannot report the gap. What
// it costs is the Flux watches, and the banner says so.
func startupProfile(cfg config) (clusterprofile.ClusterProfile, string, error) {
	if cfg.clusterProfile != "" {
		profile, err := loadProfile(cfg.clusterProfile)
		if err != nil {
			return clusterprofile.ClusterProfile{}, "", err
		}
		return profile, "cluster profile: " + cfg.clusterProfile, nil
	}
	profile, err := detect.FromCluster(cfg.kubeconfig)
	if err != nil {
		return clusterprofile.ClusterProfile{}, "cluster profile: detection failed (" + err.Error() + ")", nil
	}
	return profile, "cluster profile: detected", nil
}

// loadProfile reads a ClusterProfile from a file. A missing file is an error
// rather than an empty profile: an operator who passed a path asked for that
// document, and silently rendering against an empty profile would emit
// manifests missing every optional resource, which looks intentional.
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

// profileBanner names the profile's source for a config that has one.
func profileBanner(cfg config) string {
	if cfg.clusterProfile == "" {
		return "cluster profile: detected"
	}
	return "cluster profile: " + cfg.clusterProfile
}

// registryBanner says where artifacts will go. An unset registry is said out
// loud rather than discovered on the first reconcile: a registry is a hard
// requirement of the spine (ADR-0028, "Consequences") and a controller running
// without one deploys nothing.
func registryBanner(cfg config) string {
	if cfg.registry == "" {
		return "registry: unset (--registry or $" + registryEnv + "); no environment can be published"
	}
	return "registry: " + cfg.registry + "/" + controller.ArtifactNamespace
}

// fluxBanner says whether the Flux watches were registered, because it is the
// difference between an environment that converges on a watch and one that
// converges on a timer — and the fix (install Flux, restart) is not one anybody
// would guess from a status alone.
func fluxBanner(cfg config, profile clusterprofile.ClusterProfile) string {
	if profile.Flux == nil {
		return "flux: not detected; watches are off and environments will report FluxNotInstalled " +
			"(restart the controller after installing Flux)"
	}
	return "flux: watching " + cfg.fluxNamespace
}
