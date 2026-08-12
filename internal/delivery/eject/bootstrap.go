package eject

// Bootstrap configuration for the ejected repository.
//
// # Why these manifests are emitted and not committed
//
// An ejected repository is useless until something reconciles it, so eject
// emits the Flux or Argo object that points at the path it just wrote. It does
// NOT commit those objects into the delivery path, for the reason that decides
// most of this package: the acceptance criterion of issue #41 is that the
// final tip is byte-identical to the latest recorded direct-mode revision.
// Committing a Kustomization into the same directory would add a file the
// direct-mode history never had, breaking that guarantee and — worse — feeding
// the reconciler its own configuration, which is not what either tool expects.
//
// The reconciler's own objects belong to the cluster's bootstrap path
// (flux-system, argocd), which is the operator's territory, not kelson's. So
// they are printed, or written where the user asks, and applied by hand.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// Bootstrap defaults. The namespaces are each tool's conventional install
// namespace; the interval is Flux's usual reconcile cadence for an app path.
const (
	DefaultFluxNamespace = "flux-system"
	DefaultArgoNamespace = "argocd"
	DefaultArgoProject   = "default"
	DefaultInterval      = "5m"
	// DefaultSourceInterval is how often Flux polls the repository for new
	// commits. kelson triggers reconciliation on commit (internal/delivery/
	// flux/reconcile.go), so the poll is a safety net, not the happy path.
	DefaultSourceInterval = "1m"
	// argoDestinationServer is the in-cluster API server address Argo CD uses
	// for its own cluster.
	argoDestinationServer = "https://kubernetes.default.svc"
)

// BootstrapOptions configures the reconciler manifests emitted alongside a
// replay.
type BootstrapOptions struct {
	// Disabled suppresses emission entirely.
	Disabled bool
	// Namespace is where the reconciler's objects live; empty selects the
	// tool's conventional namespace.
	Namespace string
	// Name overrides the derived "<project>-<environment>" object name.
	Name string
	// Interval is the reconcile interval for the Flux Kustomization.
	Interval string
	// ArgoProject is the Argo CD AppProject the Application joins.
	ArgoProject string
}

// Bootstrap is the emitted reconciler configuration.
type Bootstrap struct {
	// Mode is the delivery mode it configures.
	Mode string
	// Filename is the suggested file name when writing it out.
	Filename string
	// YAML is the manifest stream, ready for `kubectl apply -f -`.
	YAML []byte
	// Summary is a one-line description of what applying it does.
	Summary string
}

// bootstrap builds the reconciler configuration for the target mode.
func (e *Ejector) bootstrap() (*Bootstrap, error) {
	if e.opts.Bootstrap.Disabled {
		return nil, nil
	}
	name := e.opts.Bootstrap.Name
	if name == "" {
		name = slugify(e.opts.Project + "-" + e.opts.Environment)
	}
	switch e.opts.Mode {
	case ModeFlux:
		return e.fluxBootstrap(name)
	case ModeArgoCD:
		return e.argoBootstrap(name)
	default:
		return nil, configErr("mode", fmt.Sprintf("no bootstrap configuration for mode %q", e.opts.Mode),
			"eject supports "+ModeFlux+" and "+ModeArgoCD)
	}
}

// fluxBootstrap emits the GitRepository that tracks the ejected repository and
// the Kustomization that applies the path kelson owns.
//
// The path is written Flux-style ("./dir"), which is exactly what the flux
// adapter's coverage check normalises when it answers "is anything watching
// where I wrote" (internal/delivery/flux/status.go, covers). Emitting the
// shape the adapter recognises is the point: a delivery/not-watched error
// after an eject would mean the bootstrap and the adapter disagree.
func (e *Ejector) fluxBootstrap(name string) (*Bootstrap, error) {
	ns := e.opts.Bootstrap.Namespace
	if ns == "" {
		ns = DefaultFluxNamespace
	}
	interval := e.opts.Bootstrap.Interval
	if interval == "" {
		interval = DefaultInterval
	}

	source := map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   objectMeta(name, ns, e.opts.Project, e.opts.Environment),
		"spec": map[string]any{
			"interval": DefaultSourceInterval,
			"url":      e.opts.Target.Repo,
			"ref":      map[string]any{"branch": e.opts.Target.Branch},
		},
	}
	kustomization := map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   objectMeta(name, ns, e.opts.Project, e.opts.Environment),
		"spec": map[string]any{
			"interval": interval,
			"path":     fluxPath(e.opts.Target.Path),
			// Prune mirrors direct mode: a resource dropped from the spec is
			// deleted, not orphaned. Direct mode prunes by provenance labels,
			// so an environment that ejects keeps the behaviour it had.
			"prune": true,
			"sourceRef": map[string]any{
				"kind": "GitRepository",
				"name": name,
			},
		},
	}

	yamlBytes, err := encodeStream(source, kustomization)
	if err != nil {
		return nil, err
	}
	return &Bootstrap{
		Mode:     ModeFlux,
		Filename: fmt.Sprintf("flux-%s.yaml", name),
		YAML:     yamlBytes,
		Summary: fmt.Sprintf("Flux GitRepository + Kustomization %s/%s reconciling %s in %s (%s)",
			ns, name, fluxPath(e.opts.Target.Path), e.opts.Target.Repo, e.opts.Target.Branch),
	}, nil
}

// argoBootstrap emits the Argo CD Application that syncs the ejected path.
//
// spec.destination carries no namespace on purpose: kelson renders every
// namespaced resource with an explicit metadata.namespace, so pinning a
// destination namespace here could only override what the manifests already
// say — a delivery adapter influencing the applied output, which ADR-0001
// forbids.
func (e *Ejector) argoBootstrap(name string) (*Bootstrap, error) {
	ns := e.opts.Bootstrap.Namespace
	if ns == "" {
		ns = DefaultArgoNamespace
	}
	project := e.opts.Bootstrap.ArgoProject
	if project == "" {
		project = DefaultArgoProject
	}
	path := e.opts.Target.Path
	if path == "" {
		path = "."
	}

	app := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   objectMeta(name, ns, e.opts.Project, e.opts.Environment),
		"spec": map[string]any{
			"project": project,
			"source": map[string]any{
				"repoURL":        e.opts.Target.Repo,
				"targetRevision": e.opts.Target.Branch,
				"path":           path,
			},
			"destination": map[string]any{
				"server": argoDestinationServer,
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{
					"prune":    true,
					"selfHeal": true,
				},
			},
		},
	}

	yamlBytes, err := encodeStream(app)
	if err != nil {
		return nil, err
	}
	return &Bootstrap{
		Mode:     ModeArgoCD,
		Filename: fmt.Sprintf("argocd-%s.yaml", name),
		YAML:     yamlBytes,
		Summary: fmt.Sprintf("Argo CD Application %s/%s syncing %s in %s (%s)",
			ns, name, path, e.opts.Target.Repo, e.opts.Target.Branch),
	}, nil
}

// objectMeta stamps the same provenance labels the renderer puts on every
// resource, so the bootstrap object is recognisable as kelson's and correlates
// with the environment it reconciles.
func objectMeta(name, namespace, project, environment string) map[string]any {
	return map[string]any{
		"name":      name,
		"namespace": namespace,
		"labels": map[string]any{
			"app.kubernetes.io/managed-by": "kelson",
			"kelson.dev/project":           project,
			"kelson.dev/environment":       environment,
		},
	}
}

// fluxPath writes a repository path the way Flux manifests conventionally do.
func fluxPath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "./"
	}
	return "./" + p
}

// encodeStream marshals objects into one multi-document YAML stream. Keys are
// emitted in sorted order, so the same inputs always produce the same bytes —
// the bootstrap output is as diffable as the rendered manifests are.
func encodeStream(objects ...map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	for _, obj := range objects {
		data, err := sigsyaml.Marshal(obj)
		if err != nil {
			return nil, wrap(err, "encoding the bootstrap manifest")
		}
		buf.WriteString("---\n")
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	out := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "kelson"
	}
	return out
}
