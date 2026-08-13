// Package detect captures a ClusterProfile from a live cluster.
//
// It is deliberately a sibling of — and never imported by — internal/renderer:
// the renderer is a pure function whose ClusterProfile arrives as an input
// (issue #20, ADR-0001). Everything that talks to a cluster lives out here.
//
// # Absent, present, and unknown are three different answers
//
// Detection tells the renderer what a cluster provides so it can adapt without
// installing competing software (ADR-0003). The contract (clusterprofile.go)
// is that a nil component means "not detected", a non-nil one means "present"
// (version empty when unknown), and anything the probe was *not allowed to
// look at* lands in Incomplete as a Gap rather than being collapsed into
// absence. A probe that gets Forbidden listing ClusterIssuers and reports
// CertManager as nil would make the renderer emit an Ingress with no TLS and
// the manifest would look deliberate (issue #56).
//
// Presence is established once, from the single /apis discovery call: an API
// group that is registered means the component that owns it is installed. The
// per-resource listings (classes, issuers, stores) are details layered on top,
// and each is individually allowed to fail into a Gap without demoting the
// component to absent — a cluster that lists no ClusterIssuers and one that
// forbids listing them are different facts.
package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// FromCluster probes a live cluster for its capabilities (Gateway API, ingress
// classes, cert-manager, ...) and returns them as a ClusterProfile.
// kubeconfig context selection follows the usual precedence — an explicit path
// wins, then $KUBECONFIG, then in-cluster credentials, then ~/.kube/config.
// Passing "" selects that default chain.
//
// The probe never fails because a specific component could not be read: it
// records the gap in ClusterProfile.Incomplete so a partially-visible cluster
// still yields a profile that is honest about what it could not see. It fails
// only when the cluster itself is unusable (no credentials, unreachable), the
// same loud failure kube.Connect reports for a preview that requested a server.
func FromCluster(kubeconfig string) (clusterprofile.ClusterProfile, error) {
	p, err := connect(kubeconfig)
	if err != nil {
		return clusterprofile.ClusterProfile{}, err
	}
	return p.probe(context.Background())
}

// connect builds the two clients detection needs. It mirrors the kubeconfig
// loading in internal/delivery/kube.Connect (NewNonInteractiveDeferredLoadingClientConfig)
// rather than inventing a third loader, but returns a discovery and a dynamic
// client directly because detection needs both in their raw form — it reads
// /apis for presence and lists specific resources, so a wrapped mapper-only
// handle would not suffice.
func connect(kubeconfig string) (*prober, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("detect: no usable cluster credentials: %w "+
			"(pass --kubeconfig, set $KUBECONFIG, or run inside the cluster)", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("detect: building the discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("detect: building the dynamic client: %w", err)
	}
	return &prober{disco: disco, dyn: dyn}, nil
}

// prober holds the clients and the gaps accumulated while probing. gaps are
// appended in the order the probe walks the components, which keeps the output
// stable for a given cluster.
type prober struct {
	disco discovery.DiscoveryInterface
	dyn   dynamic.Interface
	gaps  []clusterprofile.Gap
}

func (p *prober) gap(field, reason string) {
	p.gaps = append(p.gaps, clusterprofile.Gap{Field: field, Reason: reason})
}

// The fixed resource coordinates. Versions are stable for the CRD groups kelson
// cares about; gateways/networking and storage are core groups that have not
// bumped past these versions.
var (
	coreV1Nodes            = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	deploymentGVR          = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gatewayClassGVR        = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}
	ingressClassGVR        = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"}
	clusterIssuerGVR       = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
	clusterSecretStoreGVR  = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}
	secretStoreGVR         = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}
	storageClassGVR        = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	volumeSnapshotClassGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
)

// The API groups that mark a component as present, mapped to the kind token a
// verb would target (used to name the missing permission in a Gap's reason).
var componentGroups = []struct {
	group string
	field string
}{
	{"gateway.networking.k8s.io", "gatewayAPI"},
	{"cert-manager.io", "certManager"},
	{"external-secrets.io", "externalSecrets"},
	{"postgresql.cnpg.io", "cnpg"},
	{"source.toolkit.fluxcd.io", "flux"},
	// flux-operator owns this group, and it is the same group the delivery
	// plane reads FluxReport from (internal/delivery/flux/dynamic.go) — the two
	// must name the same coordinates or the profile would promise a CR the
	// adapter looks for somewhere else. LICENSE: flux-operator is AGPL-3.0 and
	// kelson is MIT, so detection stays at discovery of the group; no Go module
	// of theirs is imported (issue #157, docs/architecture.md).
	{"fluxcd.controlplane.io", "fluxOperator"},
	{"argoproj.io", "argocd"},
	{"metrics.k8s.io", "metricsServer"},
	{"monitoring.coreos.com", "prometheus"},
}

func (p *prober) probe(ctx context.Context) (clusterprofile.ClusterProfile, error) {
	prof := clusterprofile.ClusterProfile{}

	// One discovery round trip establishes what is registered at all. Its
	// single error poisons every group-presence check that follows: when /apis
	// itself is forbidden there is no way to tell a missing component from an
	// invisible one, so every group-backed field becomes a gap.
	serverGroups, groupsErr := p.disco.ServerGroups()
	groupPresent := func(name string) (present, known bool) {
		if groupsErr != nil {
			return false, false
		}
		for _, g := range serverGroups.Groups {
			if g.Name == name {
				return true, true
			}
		}
		return false, true
	}

	prof.Kubernetes = p.probeKubernetes(ctx)
	prof.IngressClasses = p.probeIngressClasses(ctx)
	prof.StorageClasses = p.probeStorageClasses(ctx)

	for _, cg := range componentGroups {
		prof = p.applyGroupPresence(prof, cg.group, cg.field, groupPresent)
	}
	prof.PolicyEngines = p.probePolicyEngines(groupPresent)

	if prof.GatewayAPI != nil {
		prof.GatewayAPI.Version = preferredVersion(serverGroups, "gateway.networking.k8s.io", "v1")
		prof.GatewayAPI.Classes = p.probeGatewayClasses(ctx)
	}
	if prof.CloudNativePG != nil {
		p.probeCloudNativePG(ctx, prof.CloudNativePG, preferredVersion(serverGroups, cnpgGroup, "v1"))
	}
	if prof.CertManager != nil {
		p.probeClusterIssuers(ctx, &prof.CertManager.ClusterIssuers)
	}
	if prof.ExternalSecrets != nil {
		p.probeExternalSecretStores(ctx, prof.ExternalSecrets)
	}
	if prof.Prometheus != nil {
		p.probePrometheusKinds(ctx, prof.Prometheus)
	}

	prof.Incomplete = p.gaps
	return prof, nil
}

// applyGroupPresence turns a group-presence answer into a component pointer.
// known=false means /apis failed, so the component is left absent but a Gap
// records that absence is not a finding.
func (p *prober) applyGroupPresence(prof clusterprofile.ClusterProfile, group, field string, present func(string) (bool, bool)) clusterprofile.ClusterProfile {
	ok, known := present(group)
	if !known {
		p.gap(field, errAPIsForbidden)
		return prof
	}
	if !ok {
		return prof
	}
	switch field {
	case "gatewayAPI":
		prof.GatewayAPI = &clusterprofile.GatewayAPI{}
	case "certManager":
		prof.CertManager = &clusterprofile.CertManager{}
	case "externalSecrets":
		prof.ExternalSecrets = &clusterprofile.ExternalSecrets{}
	case "cnpg":
		prof.CloudNativePG = &clusterprofile.CloudNativePG{}
	case "flux":
		prof.Flux = &clusterprofile.Component{}
	case "fluxOperator":
		prof.FluxOperator = &clusterprofile.Component{}
	case "argocd":
		prof.ArgoCD = &clusterprofile.Component{}
	case "metricsServer":
		prof.MetricsServer = &clusterprofile.Component{}
	case "prometheus":
		prof.Prometheus = &clusterprofile.Prometheus{}
	}
	return prof
}

// probeKubernetes reads server version, distribution, and node architectures.
// Version and platform are best-effort from the /version endpoint; node
// architectures need a nodes list and can fail into a Gap on their own.
func (p *prober) probeKubernetes(ctx context.Context) *clusterprofile.Kubernetes {
	k := &clusterprofile.Kubernetes{}
	sv, err := p.disco.ServerVersion()
	if err != nil {
		p.gap("kubernetes.version", p.reasonFor(err, "server version"))
	} else {
		k.Version = sv.GitVersion
		k.Platform = detectPlatform(sv.GitVersion)
	}
	k.NodeArchitectures = p.probeNodeArchitectures(ctx)
	return k
}

func (p *prober) probeNodeArchitectures(ctx context.Context) []string {
	nodes, err := p.dyn.Resource(coreV1Nodes).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("kubernetes.nodeArchitectures", p.reasonFor(err, "nodes"))
		return nil
	}
	seen := map[string]bool{}
	for _, n := range nodes.Items {
		if arch, _, _ := unstructured.NestedString(n.Object, "status", "nodeInfo", "architecture"); arch != "" {
			seen[arch] = true
		}
	}
	return sortedKeys(seen)
}

// detectPlatform identifies a Kubernetes distribution from the server version
// string. It returns "" when nothing is recognizable — guessing a platform the
// cluster does not run speeds no decision along and would be checked nowhere.
func detectPlatform(gitVersion string) string {
	switch {
	case strings.Contains(gitVersion, "k3s"):
		return "k3s"
	case strings.Contains(gitVersion, "gke"):
		return "gke"
	case strings.Contains(gitVersion, "eks"):
		return "eks"
	case strings.Contains(gitVersion, "talos"):
		return "talos"
	}
	return ""
}

// preferredVersion picks the version of group the server prefers, falling back
// to the version kelson targets when the group list says nothing useful. Used
// for the Gateway API's reported version and to address the CNPG group's
// resource list at the version the server actually prefers rather than a
// hard-coded one.
func preferredVersion(groups *metav1.APIGroupList, group, fallback string) string {
	if groups == nil {
		return fallback
	}
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		if g.PreferredVersion.Version != "" {
			return g.PreferredVersion.Version
		}
		if len(g.Versions) > 0 {
			return g.Versions[0].Version
		}
	}
	return fallback
}

func (p *prober) probeGatewayClasses(ctx context.Context) []string {
	list, err := p.dyn.Resource(gatewayClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("gatewayAPI.classes", p.reasonFor(err, "gatewayclasses.gateway.networking.k8s.io"))
		return nil
	}
	var names []string
	for _, c := range list.Items {
		names = append(names, c.GetName())
	}
	sort.Strings(names)
	return names
}

// probeIngressClasses reads every ingress class and whether each is the cluster
// default, a decision recorded as an annotation on the class itself.
func (p *prober) probeIngressClasses(ctx context.Context) []clusterprofile.IngressClass {
	list, err := p.dyn.Resource(ingressClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("ingressClasses", p.reasonFor(err, "ingressclasses.networking.k8s.io"))
		return nil
	}
	var out []clusterprofile.IngressClass
	for _, ic := range list.Items {
		cls := clusterprofile.IngressClass{Name: ic.GetName()}
		cls.Controller, _, _ = unstructured.NestedString(ic.Object, "spec", "controller")
		cls.Default = strings.EqualFold(ic.GetAnnotations()["ingressclass.kubernetes.io/is-default-class"], "true")
		out = append(out, cls)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// probeClusterIssuers fills the ClusterIssuers slice in place; a forbidden list
// leaves the slice empty but records the gap, so an issuerless cluster and one
// the probe could not see never collapse into the same profile.
func (p *prober) probeClusterIssuers(ctx context.Context, out *[]string) {
	list, err := p.dyn.Resource(clusterIssuerGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("certManager.clusterIssuers", p.reasonFor(err, "clusterissuers.cert-manager.io"))
		return
	}
	for _, ci := range list.Items {
		*out = append(*out, ci.GetName())
	}
	sort.Strings(*out)
}

func (p *prober) probeExternalSecretStores(ctx context.Context, es *clusterprofile.ExternalSecrets) {
	clusters, err := p.dyn.Resource(clusterSecretStoreGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("externalSecrets.clusterSecretStores", p.reasonFor(err, "clustersecretstores.external-secrets.io"))
	} else {
		for _, s := range clusters.Items {
			es.ClusterSecretStores = append(es.ClusterSecretStores, s.GetName())
		}
		sort.Strings(es.ClusterSecretStores)
	}

	namespaced, err := p.dyn.Resource(secretStoreGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("externalSecrets.secretStores", p.reasonFor(err, "secretstores.external-secrets.io"))
	} else {
		for _, s := range namespaced.Items {
			es.SecretStores = append(es.SecretStores, s.GetName())
		}
		sort.Strings(es.SecretStores)
	}
}

// CNPG detection coordinates (issue #90).
//
// The version matters more here than for any other adopted component: kelson
// renders databases declaratively — managed roles, the Database CRD, schemas
// and extensions — and each of those arrived in a different CNPG release, so
// "present" alone cannot answer whether a preset is hostable. The judgement
// (internal/clusterprofile/postgres) needs the version and the served CRDs;
// this is where they come from.
const (
	cnpgGroup = "postgresql.cnpg.io"
	// cnpgOperatorSelector matches the operator Deployment in both supported
	// install methods: the upstream release manifest and the Helm chart both
	// label it app.kubernetes.io/name=cloudnative-pg. Selecting by label keeps
	// the read narrow — detection wants one Deployment, not an inventory of the
	// cluster's workloads — even though a ClusterRole cannot express that.
	cnpgOperatorSelector = "app.kubernetes.io/name=cloudnative-pg"
	// cnpgVersionLabel is the Helm chart's appVersion label, used when the
	// operator image is pinned by digest and carries no readable tag.
	cnpgVersionLabel = "app.kubernetes.io/version"
)

// probeCloudNativePG fills in the operator's version, namespace and served
// CRDs. Both reads may fail into their own Gap without demoting CNPG to absent:
// an operator whose Deployment cannot be read is still installed, and reporting
// it as absent would tell a caller to install a second one — which CNPG's
// cluster-scoped resources make actively harmful (ADR-0005).
func (p *prober) probeCloudNativePG(ctx context.Context, c *clusterprofile.CloudNativePG, groupVersion string) {
	p.probeCNPGOperator(ctx, c)
	p.probeCNPGResources(c, groupVersion)
}

// probeCNPGOperator reads the operator Deployment for the running version. The
// container image tag is preferred over the chart's version label because it is
// what actually runs: a chart can be upgraded with the image pinned, and the
// tag is present in the plain-manifest install where the label is not.
//
// Finding no labeled Deployment is not a gap. It means the operator was
// installed in a shape this probe does not recognise, and an empty Version
// already carries "installed, version unknown" (clusterprofile.go) — which the
// judgement reports as Unknown rather than as too old.
func (p *prober) probeCNPGOperator(ctx context.Context, c *clusterprofile.CloudNativePG) {
	list, err := p.dyn.Resource(deploymentGVR).List(ctx, metav1.ListOptions{LabelSelector: cnpgOperatorSelector})
	if err != nil {
		p.gap("cnpg.version", p.reasonFor(err, "deployments.apps"))
		return
	}
	items := append([]unstructured.Unstructured(nil), list.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].GetNamespace() != items[j].GetNamespace() {
			return items[i].GetNamespace() < items[j].GetNamespace()
		}
		return items[i].GetName() < items[j].GetName()
	})
	if len(items) == 0 {
		return
	}
	d := &items[0]
	c.Namespace = d.GetNamespace()
	if v := operatorImageVersion(d); v != "" {
		c.Version = v
		return
	}
	c.Version = d.GetLabels()[cnpgVersionLabel]
}

// operatorImageVersion extracts the operator version from the image tag of the
// container running the operator, e.g.
// ghcr.io/cloudnative-pg/cloudnative-pg:1.26.0 -> 1.26.0. Returns "" when the
// image is digest-pinned or the tag is not a version — guessing would be worse
// than the Unknown an empty version produces.
func operatorImageVersion(d *unstructured.Unstructured) string {
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		image, _ := container["image"].(string)
		if !strings.Contains(image, "cloudnative-pg") {
			continue
		}
		if tag := imageTag(image); tag != "" {
			return tag
		}
	}
	return ""
}

// imageTag returns the tag of an image reference, or "" when there is none.
// A digest suffix is dropped first, and a colon that belongs to a registry port
// (host:5000/repo) is not mistaken for a tag separator.
func imageTag(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	i := strings.LastIndexByte(image, ':')
	if i < 0 {
		return ""
	}
	tag := image[i+1:]
	if tag == "" || strings.Contains(tag, "/") {
		return ""
	}
	return tag
}

// probeCNPGResources records which resources the API server actually serves in
// the CNPG group — clusters, databases, poolers and the rest — because that is
// the capability a manifest meets. A cluster whose operator version says 1.26
// but whose Database CRD was never applied would accept a Cluster and reject a
// Database, and the served set is the half of that answer discovery can give.
func (p *prober) probeCNPGResources(c *clusterprofile.CloudNativePG, groupVersion string) {
	gv := cnpgGroup + "/" + groupVersion
	res, err := p.disco.ServerResourcesForGroupVersion(gv)
	if err != nil {
		p.gap("cnpg.crds", p.reasonFor(err, gv))
		return
	}
	for _, r := range res.APIResources {
		// Subresources ("clusters/status") are not CRDs a manifest targets.
		if strings.Contains(r.Name, "/") {
			continue
		}
		c.CRDs = append(c.CRDs, r.Name)
	}
	sort.Strings(c.CRDs)
}

// probePolicyEngines reports each admission-policy controller whose
// registration was visible. Kyverno owns one group; Gatekeeper spreads its CRDs
// across two, so either one marks it present.
func (p *prober) probePolicyEngines(present func(string) (bool, bool)) []clusterprofile.PolicyEngine {
	var out []clusterprofile.PolicyEngine

	kyverno, kyKnown := present("kyverno.io")
	if kyKnown && kyverno {
		out = append(out, clusterprofile.PolicyEngine{Name: "kyverno"})
	}
	var gkKnown, gkPresent bool
	gkKnown = true
	for _, group := range []string{"templates.gatekeeper.sh", "constraints.gatekeeper.sh"} {
		ok, known := present(group)
		if !known {
			gkKnown = false
			continue
		}
		if ok {
			gkPresent = true
		}
	}
	if gkKnown && gkPresent {
		out = append(out, clusterprofile.PolicyEngine{Name: "gatekeeper"})
	}

	// If /apis itself was forbidden neither engine's absence is a finding; the
	// other group-backed components get gapped in applyGroupPresence too, so
	// this stays consistent with them.
	if !kyKnown || !gkKnown {
		p.gap("policyEngines", errAPIsForbidden)
	}
	return out
}

func (p *prober) probeStorageClasses(ctx context.Context) []clusterprofile.StorageClass {
	scs, err := p.dyn.Resource(storageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("storageClasses", p.reasonFor(err, "storageclasses.storage.k8s.io"))
		return nil
	}

	vscByDriver := map[string]string{}
	vscs, err := p.dyn.Resource(volumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.gap("storageClasses", p.reasonFor(err, "volumesnapshotclasses.snapshot.storage.k8s.io"))
	} else {
		for _, v := range vscs.Items {
			if driver, _, _ := unstructured.NestedString(v.Object, "driver"); driver != "" {
				if _, exists := vscByDriver[driver]; !exists {
					vscByDriver[driver] = v.GetName()
				}
			}
		}
	}

	var out []clusterprofile.StorageClass
	for _, sc := range scs.Items {
		cls := clusterprofile.StorageClass{Name: sc.GetName()}
		cls.Provisioner, _, _ = unstructured.NestedString(sc.Object, "provisioner")
		cls.Default = strings.EqualFold(sc.GetAnnotations()["storageclass.kubernetes.io/is-default-class"], "true")
		// A snapshot class whose driver matches this class's provisioner is
		// the enablement for database branching (issue #108); without one the
		// restore-based path applies and the field stays empty.
		if cls.Provisioner != "" {
			cls.VolumeSnapshotClass = vscByDriver[cls.Provisioner]
		}
		out = append(out, cls)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// probePrometheusKinds records whether the ServiceMonitor and PodMonitor kinds
// specifically exist. A cluster can serve the monitoring group with one kind
// and not the other, and emitting the kind nobody reads is a silent loss.
func (p *prober) probePrometheusKinds(ctx context.Context, pr *clusterprofile.Prometheus) {
	res, err := p.disco.ServerResourcesForGroupVersion("monitoring.coreos.com/v1")
	if err != nil {
		if !apierrors.IsNotFound(err) {
			p.gap("prometheus", p.reasonFor(err, "servicemonitors.podmonitors.monitoring.coreos.com"))
		}
		return
	}
	for _, r := range res.APIResources {
		switch r.Name {
		case "servicemonitors":
			pr.ServiceMonitor = true
		case "podmonitors":
			pr.PodMonitor = true
		}
	}
}

// errAPIsForbidden is the gap reason recorded when the single /apis call was
// forbidden, which makes every group-presence answer unknowable at once.
const errAPIsForbidden = "forbidden: needs get on /apis"

// reasonFor renders a Gap reason a human can act on: forbidden names the
// permission that would fix it, not-found says the kind is unregistered, and
// anything else is the raw error.
func (p *prober) reasonFor(err error, resource string) string {
	if apierrors.IsForbidden(err) {
		return fmt.Sprintf("forbidden: needs get,list on %s", resource)
	}
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("not found: %s is not registered", resource)
	}
	return fmt.Sprintf("read failed: %v", err)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
