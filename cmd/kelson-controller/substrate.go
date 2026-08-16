package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

// --ensure-substrate: install the delivery substrate once, then exit.
//
// # Why the controller binary carries this
//
// ADR-0028 makes Flux the only reconciliation path, so a cluster without it
// deploys nothing: an Environment validates, renders, and then sits reporting
// FluxNotInstalled until somebody runs `kelson install`. Installing kelson has
// to install what kelson cannot work without — a created project must never sit
// silently unreconciled because the substrate is absent (owner decision
// 2026-08-16).
//
// This is that install, run once by a Helm hook Job
// (deploy/chart/kelson/templates/substrate-hook.yaml) under its own
// ServiceAccount, which is deleted with the hook. It is deliberately NOT a
// standing capability of the running controller: the grant an install needs —
// CRDs, cluster-scoped RBAC, a namespace — is far wider than the reconciler's,
// and a controller that could install a platform component at any moment is a
// controller whose RBAC has to be read as if it might.
//
// It lives in this binary because the image is already in the cluster, already
// pinned to the release's tag, and already knows how to talk to a cluster. A
// second image would be a second thing to publish, pull and keep in step.
//
// # Nothing here is a second install path
//
// Every apply goes through internal/delivery/install — the same catalog, the
// same digest verification, the same per-object created/adopted provenance
// (ADR-0021 decision 3), so `kelson uninstall --component` remains exactly as
// true about a hook-installed substrate as about a hand-installed one. Which
// row is installed is [install.SubstrateName]'s answer, which is ADR-0030's
// preference: flux-aio when this build carries its rendered snapshot,
// flux-operator otherwise.
//
// # Three verdicts, and the one that must never be silent
//
//	detection says Yes      adopt: print what was found, apply nothing, exit 0
//	detection says No       install, wait for the two kinds, exit 0
//	detection says Unknown  refuse, naming the permission, exit non-zero
//
// The Yes row is ADR-0003's rule surviving contact with an automatic install:
// kelson never modifies a component it did not install, and "automatic" does
// not weaken that — a cluster that already runs Flux (from `flux bootstrap`,
// flux-operator, flux-aio, a vendor's distribution) is adopted untouched. The
// Unknown row is the one worth stating: a detection gap hid the answer, and
// installing on an unknown is how a cluster ends up with two reconcilers
// fighting over the same CRDs (issue #144). An unattended caller makes that
// mistake more easily than a person does, not less, so it exits non-zero and
// says which permission would settle it.

// substrateKinds are the two kinds kelson writes (ADR-0028 decision 3). An
// install that has not reached the point of serving them has not finished, so
// they are what the wait waits for — the same condition hack/e2e/spine.sh
// blocks on before it installs the chart, for the same reason.
var substrateKinds = []schema.GroupVersionResource{flux.OCIRepositoryGVR, flux.KustomizationGVR}

// defaultSubstrateTimeout is generous because the two rows finish at very
// different speeds: flux-aio's snapshot registers the CRDs in the apply itself,
// while the flux-operator row applies an operator that then reconciles a
// FluxInstance and pulls six controller images. hack/e2e/spine.sh waits ten
// minutes across the same sequence for the same reason.
const defaultSubstrateTimeout = 10 * time.Minute

// substrateEngine is the delivery plane's install engine, as this command uses
// it. It is an interface so the three verdicts can be tested without a cluster;
// the production value is *install.Installer.
type substrateEngine interface {
	Plan(ctx context.Context, req install.Request) (*install.Plan, error)
	Execute(ctx context.Context, plan *install.Plan) (*install.Report, error)
}

// substrateCluster is everything the ensure mode does to a live cluster beyond
// detection: install, observe what is served, and restart one Deployment.
type substrateCluster struct {
	engine substrateEngine
	// serves reports whether the API server currently serves a resource. It is
	// discovery rather than a CRD read on purpose: "the kind is servable" is
	// the condition the controller's own start-up detection reads, and a CRD
	// that exists but is not yet Established would answer the wrong question.
	serves func(ctx context.Context, gvr schema.GroupVersionResource) (bool, error)
	// restart bounces a Deployment the way `kubectl rollout restart` does.
	restart func(ctx context.Context, namespace, name string) error
}

// substrateDeps is the seam. Production wiring is [liveSubstrate].
type substrateDeps struct {
	detect  func(kubeconfig string) (clusterprofile.ClusterProfile, error)
	connect func(kubeconfig string) (*substrateCluster, error)
	// poll is how often the wait re-asks discovery.
	poll time.Duration
}

func liveSubstrate() substrateDeps {
	return substrateDeps{
		detect: detect.FromCluster,
		connect: func(kubeconfig string) (*substrateCluster, error) {
			cluster, err := kube.Connect(kubeconfig)
			if err != nil {
				return nil, err
			}
			engine, err := install.New(install.Options{
				Client: cluster.Dynamic,
				Mapper: cluster.Mapper,
				Fetch:  install.HTTPFetcher{},
			})
			if err != nil {
				return nil, err
			}
			return &substrateCluster{
				engine: engine,
				serves: func(ctx context.Context, gvr schema.GroupVersionResource) (bool, error) {
					return serves(ctx, cluster.Typed.Discovery(), gvr)
				},
				restart: func(ctx context.Context, namespace, name string) error {
					return restartDeployment(ctx, cluster.Typed, namespace, name)
				},
			}, nil
		},
		poll: 3 * time.Second,
	}
}

// ensureSubstrate runs the mode. Its error is the process's exit code and its
// only diagnostic, so every failure names what was tried and what to do.
func ensureSubstrate(ctx context.Context, cfg config, out io.Writer, deps substrateDeps) error {
	row, ok := install.Substrate()
	if !ok {
		return errors.New("--ensure-substrate: kelson's install catalog has no Flux row, so there is nothing " +
			"to install; this is a kelson bug, not a cluster problem")
	}

	profile, err := deps.detect(cfg.kubeconfig)
	if err != nil {
		return fmt.Errorf("--ensure-substrate: probing the cluster: %w", err)
	}

	switch outcome, detail := row.Presence(profile); outcome {
	case clusterprofile.OutcomeYes:
		// ADR-0003, unweakened by the install being automatic. Nothing is
		// applied, nothing is restarted, and the fact is printed because
		// "kelson adopted the Flux you already had" is the one sentence that
		// stops an operator hunting for what the hook changed.
		_, _ = fmt.Fprintf(out, "substrate: %s — adopting it. Nothing was installed and nothing was modified.\n", detail)
		return nil
	case clusterprofile.OutcomeUnknown:
		return fmt.Errorf("--ensure-substrate: %s. kelson will not install a second Flux over one it could not "+
			"look for — grant the probe that permission and re-run `helm upgrade`, or install Flux yourself and "+
			"let detection adopt it; nothing was applied", detail)
	}

	cluster, err := deps.connect(cfg.kubeconfig)
	if err != nil {
		return fmt.Errorf("--ensure-substrate: %w", err)
	}

	_, _ = fmt.Fprintf(out, "substrate: this cluster has no Flux, and the delivery spine cannot reconcile "+
		"without one (ADR-0028). Installing %s %s.\n", row.Name, row.Version)

	plan, err := cluster.engine.Plan(ctx, install.Request{Components: []string{row.Name}, Profile: profile})
	if err != nil {
		return fmt.Errorf("--ensure-substrate: %w", err)
	}
	if plan.Empty() {
		return fmt.Errorf("--ensure-substrate: kelson declined to install %s: %s", row.Name, refusalDetail(plan))
	}
	report, execErr := cluster.engine.Execute(ctx, plan)
	printSubstrateReport(out, report)
	if execErr != nil {
		return fmt.Errorf("--ensure-substrate: installing %s: %w", row.Name, execErr)
	}

	if err := awaitSubstrate(ctx, cluster, cfg.substrateTimeout, deps.poll, out); err != nil {
		return err
	}

	// The bounce, and why it is conditional on having installed something.
	//
	// The controller detects Flux once, at start-up, and gates its Flux watches
	// on that answer (see this package's doc comment). This hook is a Helm
	// post-install hook, so the controller Deployment already exists when it
	// runs and its first pod may well have probed a cluster that had no Flux
	// yet. Restarting it is what makes convergence a property of the install
	// rather than of which pod won a race.
	//
	// On the adopt path there is nothing to bounce: Flux was present before the
	// release began — nothing else in the release installs it — so the
	// controller's own probe saw it. That path returns above, before this.
	if cfg.substrateRestart != "" {
		namespace, name, splitErr := splitDeploymentRef(cfg.substrateRestart)
		if splitErr != nil {
			return fmt.Errorf("--substrate-restart-deployment: %w", splitErr)
		}
		if err := cluster.restart(ctx, namespace, name); err != nil {
			return fmt.Errorf("--ensure-substrate: %s is installed and serving, but restarting Deployment "+
				"%s/%s failed: %w. The controller detects Flux once at start-up, so until it restarts it will "+
				"report FluxNotInstalled on every environment — run `kubectl -n %s rollout restart deploy/%s`",
				row.Name, namespace, name, err, namespace, name)
		}
		_, _ = fmt.Fprintf(out, "restarted Deployment %s/%s so it re-runs its start-up detection against the "+
			"substrate this hook installed\n", namespace, name)
	}

	_, _ = fmt.Fprintf(out, "substrate: %s is installed and this cluster serves both kinds kelson writes.\n", row.Name)
	return nil
}

// refusalDetail turns a plan that installs nothing into one sentence. A refusal
// carries a reason and a remediation and both are worth keeping: the reason is
// what happened, the remediation is the only thing the reader can act on.
func refusalDetail(plan *install.Plan) string {
	if len(plan.Refusals) == 0 {
		return "no reason was given, which is a kelson bug"
	}
	parts := make([]string, 0, len(plan.Refusals))
	for _, r := range plan.Refusals {
		parts = append(parts, r.Reason+" — "+r.Remediation)
	}
	return strings.Join(parts, "; ")
}

func printSubstrateReport(out io.Writer, report *install.Report) {
	if report == nil {
		return
	}
	for _, c := range report.Components {
		_, _ = fmt.Fprintf(out, "  %s: %d created, %d adopted, %d failed\n",
			c.Component.Name, c.Created, c.Adopted, c.Failed)
		for _, res := range c.Results {
			if res.Outcome == install.OutcomeFailed {
				_, _ = fmt.Fprintf(out, "    failed %s — %s\n", res.Ref, res.Detail)
			}
		}
	}
}

// awaitSubstrate blocks until the cluster serves both kinds kelson writes.
//
// It is not optional politeness. The flux-operator row applies an operator that
// installs the Flux controllers asynchronously through a FluxInstance, so
// "applied" and "usable" are minutes apart; returning at the apply would hand
// the caller a green hook and a cluster that still cannot reconcile. flux-aio's
// snapshot registers the CRDs directly and resolves in seconds — the same wait
// covers both because the condition, not the mechanism, is what matters.
func awaitSubstrate(ctx context.Context, cluster *substrateCluster, timeout, poll time.Duration, out io.Writer) error {
	if poll <= 0 {
		poll = time.Second
	}
	_, _ = fmt.Fprintf(out, "waiting up to %s for the cluster to serve %s and %s\n",
		timeout, flux.OCIRepositoryGVR.Resource, flux.KustomizationGVR.Resource)

	deadline := time.Now().Add(timeout)
	for {
		missing, err := unservedKinds(ctx, cluster)
		if err != nil {
			return fmt.Errorf("--ensure-substrate: reading the cluster's API surface: %w", err)
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("--ensure-substrate: the substrate was applied, but after %s this cluster still "+
				"does not serve %s. Nothing is in a broken state — the objects are applied and re-running "+
				"`helm upgrade` re-runs this hook — but the delivery spine cannot write an environment's objects "+
				"until these kinds exist. `kubectl -n flux-system get all` and, for the flux-operator row, "+
				"`kubectl -n flux-system describe fluxinstance flux` say what is stuck",
				timeout, strings.Join(missing, " and "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func unservedKinds(ctx context.Context, cluster *substrateCluster) ([]string, error) {
	var missing []string
	for _, gvr := range substrateKinds {
		ok, err := cluster.serves(ctx, gvr)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, gvr.Resource+"."+gvr.Group)
		}
	}
	return missing, nil
}

// serves asks discovery whether one resource is currently servable.
//
// A group the API server has never heard of comes back as a NotFound rather
// than as an error, which is the ordinary state of this call for the whole of
// an install's first minute; anything else is a real failure and is returned.
func serves(_ context.Context, disco discovery.DiscoveryInterface, gvr schema.GroupVersionResource) (bool, error) {
	list, err := disco.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}

// annSubstrateRestartedAt is the pod-template annotation the bounce writes. The
// value is a timestamp for the reason `kubectl rollout restart` uses one: the
// annotation's job is to differ from the last one, so the Deployment rolls a new
// ReplicaSet instead of noticing nothing changed.
const annSubstrateRestartedAt = "kelson.dev/substrate-restarted-at"

func restartDeployment(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		annSubstrateRestartedAt, time.Now().UTC().Format(time.RFC3339))
	_, err := client.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType,
		[]byte(patch), metav1.PatchOptions{FieldManager: "kelson-controller"})
	return err
}

// splitDeploymentRef parses the <namespace>/<name> the chart passes.
//
// Both halves are required rather than defaulted: the flag exists to name one
// specific Deployment, and a namespace guessed from somewhere else is how a
// hook ends up restarting a workload nobody meant it to touch.
func splitDeploymentRef(ref string) (namespace, name string, err error) {
	namespace, name, found := strings.Cut(strings.TrimSpace(ref), "/")
	if !found || strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("%q is not a Deployment reference; it must be <namespace>/<name>", ref)
	}
	return namespace, name, nil
}
