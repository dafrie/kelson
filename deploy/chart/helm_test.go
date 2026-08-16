package chart

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests drive the real `helm` binary, because the properties worth
// pinning are properties of a render: that the chart produces valid YAML, that
// it refuses to render an unauthenticated server, and that the ClusterRole it
// actually emits — not the one in the file — is the deploy/rbac grant.
//
// Without helm on PATH they skip with a message naming the install. Skipping is
// right rather than failing: `go test ./...` must pass on a checkout that has
// never heard of Helm, and the file-level drift check in chart_test.go still
// runs there.

const chartDir = "kelson"

// helmBin finds helm on PATH or in the Go bin directory, where
// `go install helm.sh/helm/v3/cmd/helm@latest` puts it.
func helmBin(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("helm"); err == nil {
		return path
	}
	for _, dir := range goBinDirs() {
		candidate := filepath.Join(dir, "helm")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("helm not found on PATH or in the Go bin directory; install it with " +
		"`go install helm.sh/helm/v3/cmd/helm@latest` to run the chart render tests")
	return ""
}

func goBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		for _, p := range filepath.SplitList(gopath) {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// authValues is the smallest set that renders: a tag, because the chart never
// picks one, and a password source, because the chart never assumes one.
var authValues = []string{
	"--set", "image.tag=v0.1.0",
	"--set", "auth.existingSecret.name=kelson-auth",
}

func helmRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmBin(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func helmTemplate(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{"template", "kelson", chartDir, "--namespace", "kelson-system"}, extra...)
	out, err := helmRun(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// TestHelmLint is the cheapest statement that the chart is a chart.
func TestHelmLint(t *testing.T) {
	args := append([]string{"lint", chartDir}, authValues...)
	if out, err := helmRun(t, args...); err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
}

// TestTemplateRendersValidYAML renders the documented install and checks every
// document parses and carries the uninstall label. The label is the #59 claim
// in mechanical form: `kubectl delete … -l kelson.dev/install=<release>` is only
// a complete uninstall if nothing the chart creates is missing it.
func TestTemplateRendersValidYAML(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, authValues...))
	if len(docs) < 6 {
		t.Fatalf("rendered %d documents, expected the full install (namespace-scoped RBAC, SA, Deployment, Service, ClusterRole+Binding)", len(docs))
	}
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		name, _ := meta["name"].(string)
		if labels["kelson.dev/install"] != "kelson" {
			t.Errorf("%s/%s has no kelson.dev/install label — it would survive an uninstall by selector (issue #59)", kind, name)
		}
		if labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Errorf("%s/%s is not labelled app.kubernetes.io/managed-by: Helm", kind, name)
		}
	}
}

// TestTemplateServerFlags pins the flags the values surface produces, including
// the one that is not a value: in-cluster the listen address is always
// 0.0.0.0, because a Service cannot reach loopback.
func TestTemplateServerFlags(t *testing.T) {
	out := helmTemplate(t, append(authValues,
		"--set", "server.registry=ghcr.io/acme",
		"--set", "server.pushSecret=ghcr-push",
		"--set", "server.buildNamespace=kelson-builds",
		"--set", "server.insecureRegistries={localhost:5000,registry.internal:5000}",
	)...)
	container := serverContainer(t, decodeDocs(t, out))

	args := stringsOf(container["args"])
	for _, want := range []string{
		"--listen=0.0.0.0:8420",
		"--namespace=kelson-system",
		"--registry=ghcr.io/acme",
		"--push-secret=ghcr-push",
		"--build-namespace=kelson-builds",
		// A list value, one flag: the server splits on commas, and a host that
		// was not listed is never reached over plain HTTP.
		"--insecure-registries=localhost:5000,registry.internal:5000",
	} {
		if !contains(args, want) {
			t.Errorf("rendered args %v are missing %q", args, want)
		}
	}
	if contains(args, "--insecure-bind") {
		t.Error("--insecure-bind was rendered for a release that has a password")
	}
	if !strings.Contains(out, "KELSON_PASSWORD") {
		t.Error("KELSON_PASSWORD is not in the pod spec, so the server would start unauthenticated")
	}
	if strings.Contains(out, "--password") {
		t.Error("the password is passed as a flag: it would be visible in the container's command line")
	}
}

// registryConfigPath is where both binaries default --registry-config to and
// where the chart mounts it for both. It is spelled here so a template that
// moved one and not the other fails a test rather than an install.
const registryConfigPath = "/etc/kelson/registry/config.json"

// TestServerMountsTheArtifactPushSecret is ADR-0034 decision 3 at install time:
// the server publishes preview artifacts itself now, so a chart-installed
// server needs the same mounted docker config the controller has had. Without
// it the push is anonymous — which is silent, and fails at the registry rather
// than at the install.
func TestServerMountsTheArtifactPushSecret(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "server.artifactPushSecret=ghcr-push")...))
	container := serverContainer(t, docs)

	if !contains(stringsOf(container["args"]), "--registry-config="+registryConfigPath) {
		t.Errorf("rendered args %v do not point --registry-config at the mount", stringsOf(container["args"]))
	}
	mount := namedEntry(container["volumeMounts"], "registry-config")
	if mount == nil {
		t.Fatal("no registry-config volumeMount: the flag names a file nothing puts there")
	}
	if mount["mountPath"] != filepath.Dir(registryConfigPath) {
		t.Errorf("registry-config is mounted at %v, not at the directory %s lives in", mount["mountPath"], registryConfigPath)
	}
	if mount["readOnly"] != true {
		t.Error("the credential is mounted writable")
	}

	volume := namedEntry(serverPodSpec(t, docs)["volumes"], "registry-config")
	if volume == nil {
		t.Fatal("the volumeMount names a volume the pod does not declare")
	}
	secret, _ := volume["secret"].(map[string]any)
	if secret["secretName"] != "ghcr-push" {
		t.Errorf("the volume references %v, not the named Secret", secret["secretName"])
	}
	// The projection is the whole trick: a dockerconfigjson Secret's data key is
	// `.dockerconfigjson`, and what reads it wants a plain `config.json`.
	items := anySlice(secret["items"])
	if len(items) != 1 {
		t.Fatalf("expected one projected key, got %d — an unprojected mount puts the file at the wrong name", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["key"] != ".dockerconfigjson" || item["path"] != filepath.Base(registryConfigPath) {
		t.Errorf("the Secret key is projected as %v/%v, not onto %s", item["key"], item["path"], registryConfigPath)
	}
}

// TestServerArtifactPushSecretIsOptional keeps the default install unchanged: a
// registry with no auth (`kelson install registry`'s) takes an anonymous push,
// and a chart that mounted a Secret nobody named would fail to schedule.
func TestServerArtifactPushSecretIsOptional(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, authValues...))
	container := serverContainer(t, docs)

	for _, arg := range stringsOf(container["args"]) {
		if strings.HasPrefix(arg, "--registry-config") {
			t.Errorf("--registry-config was rendered with no Secret to mount: %q", arg)
		}
	}
	if container["volumeMounts"] != nil {
		t.Errorf("the server container mounts %v with no artifactPushSecret set", container["volumeMounts"])
	}
	if volumes := serverPodSpec(t, docs)["volumes"]; volumes != nil {
		t.Errorf("the server pod declares volumes with no artifactPushSecret set: %v", volumes)
	}
}

// TestServerAndControllerMountTheCredentialAlike is the claim values.yaml makes
// out loud: the two deployments read the same flag from the same path out of
// the same Secret shape, so an operator who has mounted one has not learned a
// second convention. They are two values rather than one only because the two
// Deployments are enabled independently.
func TestServerAndControllerMountTheCredentialAlike(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues,
		"--set", "controller.enabled=true",
		"--set", "controller.pushSecret=ghcr-push",
		"--set", "server.artifactPushSecret=ghcr-push",
	)...))

	var mounts, volumes []any
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers := anySlice(podSpec["containers"])
		c, _ := containers[0].(map[string]any)
		if !contains(stringsOf(c["args"]), "--registry-config="+registryConfigPath) {
			t.Errorf("%s does not read the credential from %s", nameOf(doc), registryConfigPath)
		}
		mounts = append(mounts, namedEntry(c["volumeMounts"], "registry-config"))
		volumes = append(volumes, namedEntry(podSpec["volumes"], "registry-config"))
	}
	if len(mounts) != 2 {
		t.Fatalf("expected the server's and the controller's Deployments, got %d", len(mounts))
	}
	if !reflect.DeepEqual(mounts[0], mounts[1]) {
		t.Errorf("the two mounts differ:\n%s\n%s", mustYAML(t, mounts[0]), mustYAML(t, mounts[1]))
	}
	if !reflect.DeepEqual(volumes[0], volumes[1]) {
		t.Errorf("the two volumes differ:\n%s\n%s", mustYAML(t, volumes[0]), mustYAML(t, volumes[1]))
	}
}

// TestAuthGateRefusesWithoutPassword is the posture check. Serving 0.0.0.0 with
// no authentication has to be a decision somebody made, not a default somebody
// inherited (cmd/kelson-server's checkBind, ADR-0013 §3).
func TestAuthGateRefusesWithoutPassword(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system", "--set", "image.tag=v0.1.0")
	if err == nil {
		t.Fatalf("helm template succeeded with no auth values; the chart must refuse:\n%s", out)
	}
	for _, want := range []string{"no password configured", "auth.existingSecret.name", "auth.insecure=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q — it has to say how to get out of it:\n%s", want, out)
		}
	}
}

// TestAuthGateRefusesAmbiguousPasswords: two password sources is a question the
// chart cannot answer, so it asks rather than picking.
func TestAuthGateRefusesAmbiguousPasswords(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system",
		"--set", "image.tag=v0.1.0", "--set", "auth.password=hunter2", "--set", "auth.existingSecret.name=kelson-auth")
	if err == nil {
		t.Fatalf("helm template accepted both auth.password and auth.existingSecret:\n%s", out)
	}
	if !strings.Contains(out, "Pick one") {
		t.Errorf("the refusal does not explain the ambiguity:\n%s", out)
	}
}

// TestInsecureRendersInsecureBind: the escape hatch works, and it is loud. The
// server would refuse a non-loopback bind without the flag, so a chart that
// allowed auth.insecure without rendering it would produce a CrashLoopBackOff.
func TestInsecureRendersInsecureBind(t *testing.T) {
	out := helmTemplate(t, "--set", "image.tag=v0.1.0", "--set", "auth.insecure=true")
	container := serverContainer(t, decodeDocs(t, out))
	if !contains(stringsOf(container["args"]), "--insecure-bind") {
		t.Errorf("auth.insecure did not render --insecure-bind; the server would refuse to start:\n%v", container["args"])
	}
	if strings.Contains(out, "KELSON_PASSWORD") {
		t.Error("an unauthenticated release still references a password secret")
	}
}

// TestPasswordLiteralCreatesSecret covers the documented-lesser path: the value
// still reaches the pod as a secretKeyRef, never as a literal env value.
func TestPasswordLiteralCreatesSecret(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, "--set", "image.tag=v0.1.0", "--set", "auth.password=hunter2"))
	var found bool
	for _, doc := range docs {
		if doc["kind"] == "Secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("auth.password did not produce a Secret")
	}
	container := serverContainer(t, docs)
	for _, e := range anySlice(container["env"]) {
		env, _ := e.(map[string]any)
		if env["name"] == "KELSON_PASSWORD" {
			if _, ok := env["value"]; ok {
				t.Error("KELSON_PASSWORD is a literal in the pod spec; it must be a secretKeyRef")
			}
			return
		}
	}
	t.Error("KELSON_PASSWORD is missing from the pod spec")
}

// TestImageTagIsRequired pins the other refusal: no default tag, ever.
func TestImageTagIsRequired(t *testing.T) {
	out, err := helmRun(t, "template", "kelson", chartDir, "--namespace", "kelson-system",
		"--set", "auth.existingSecret.name=kelson-auth")
	if err == nil {
		t.Fatalf("helm template succeeded with no image.tag:\n%s", out)
	}
	if !strings.Contains(out, "image.tag is required") {
		t.Errorf("the refusal does not name image.tag:\n%s", out)
	}
}

// TestRenderedDetectRoleMatchesDeployRBAC repeats the drift check against what
// helm actually emits, so a templating change that alters the rules on the way
// out is caught as well as a file edit.
func TestRenderedDetectRoleMatchesDeployRBAC(t *testing.T) {
	var got []policyRule
	for _, doc := range decodeDocs(t, helmTemplate(t, authValues...)) {
		// By name, not by kind: since the opt-in deploy grant exists there can
		// be more than one ClusterRole in a render, and comparing the wrong one
		// against deploy/rbac would be a confusing failure at best.
		if doc["kind"] != "ClusterRole" || !strings.HasSuffix(nameOf(doc), "-detect") {
			continue
		}
		remarshal(t, doc["rules"], &got)
	}
	if got == nil {
		t.Fatal("no ClusterRole was rendered")
	}
	want := readClusterRole(t, deployRBACPath).Rules
	if !reflect.DeepEqual(want, got) {
		t.Errorf("the rendered ClusterRole differs from %s:\nwant:\n%s\ngot:\n%s",
			deployRBACPath, mustYAML(t, want), mustYAML(t, got))
	}
}

// TestTargetNamespacesGetTheirOwnRole covers how docs/server.md says to bound
// the Secret grant today: name the namespaces the server may serve.
func TestTargetNamespacesGetTheirOwnRole(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "rbac.targetNamespaces={apps-prod,apps-staging}")...))
	seen := map[string]bool{}
	for _, doc := range docs {
		if doc["kind"] != "Role" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		seen[ns] = true
	}
	for _, ns := range []string{"kelson-system", "apps-prod", "apps-staging"} {
		if !seen[ns] {
			t.Errorf("no Role was rendered in %q; the server could not serve it", ns)
		}
	}
}

// TestDeployClusterRoleAbsentByDefault: the documented install must not quietly
// hand the server cluster-wide write. This is the render-level half of
// TestValuesDefaultDeployClusterRoleOff — the value could be false and the
// template still render if its guard were wrong.
func TestDeployClusterRoleAbsentByDefault(t *testing.T) {
	for _, doc := range decodeDocs(t, helmTemplate(t, authValues...)) {
		kind, _ := doc["kind"].(string)
		if (kind == "ClusterRole" || kind == "ClusterRoleBinding") && strings.HasSuffix(nameOf(doc), "-deploy") {
			t.Errorf("a default install rendered %s/%s. The deploy grant is cluster-wide write and "+
				"has to be opted into (rbac.createDeployClusterRole).", kind, nameOf(doc))
		}
	}
}

// TestDeployClusterRoleRendersWhenEnabled covers the fix end to end at render
// level: the value produces a ClusterRole that grants the namespace patch the
// reported failure named, and a binding that points it at the server's own
// ServiceAccount and no one else's.
func TestDeployClusterRoleRendersWhenEnabled(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "rbac.createDeployClusterRole=true")...))

	var role, binding map[string]any
	for _, doc := range docs {
		if !strings.HasSuffix(nameOf(doc), "-deploy") {
			continue
		}
		switch doc["kind"] {
		case "ClusterRole":
			role = doc
		case "ClusterRoleBinding":
			binding = doc
		}
	}
	if role == nil {
		t.Fatal("rbac.createDeployClusterRole=true rendered no ClusterRole")
	}
	if binding == nil {
		t.Fatal("rbac.createDeployClusterRole=true rendered no ClusterRoleBinding; an unbound ClusterRole grants nothing")
	}

	var rules []policyRule
	remarshal(t, role["rules"], &rules)
	if !granted(rules, "", "namespaces", "patch") {
		t.Error("the rendered deploy ClusterRole does not grant patch on namespaces — " +
			"that is the exact call that failed, and the reason this template exists")
	}
	// A spot check that the rendered rules are the file's rules: the drift
	// guard in deploy_rbac_test.go reads the template, and this proves helm
	// does not transform it on the way out.
	if !granted(rules, "apps", "deployments", "patch") || !granted(rules, "", "pods/log", "get") {
		t.Errorf("the rendered rules are missing the apply or the log read:\n%s", mustYAML(t, rules))
	}

	roleRef, _ := binding["roleRef"].(map[string]any)
	if roleRef["kind"] != "ClusterRole" || roleRef["name"] != nameOf(role) {
		t.Errorf("the binding points at %v, not at the ClusterRole it ships with", roleRef)
	}
	subjects := anySlice(binding["subjects"])
	if len(subjects) != 1 {
		t.Fatalf("the binding has %d subjects; a cluster-wide write grant names exactly one", len(subjects))
	}
	subject, _ := subjects[0].(map[string]any)
	if subject["kind"] != "ServiceAccount" || subject["name"] != "kelson" || subject["namespace"] != "kelson-system" {
		t.Errorf("the binding's subject is %v, want the release's own ServiceAccount", subject)
	}
}

// TestControllerClusterRoleGrantsTheFinalizerPatch pins the grant whose absence
// broke the first end-to-end run of the delivery spine.
//
// EnvironmentReconciler.addFinalizer issues a merge PATCH against the
// `environments` resource itself, because that is where a CustomResourceDefinition
// keeps metadata.finalizers — a CRD serves no `/finalizers` subresource, so
// `environments/finalizers` authorizes nothing by itself. With only the
// subresource granted, every reconcile failed with
//
//	environments.kelson.dev "live" is forbidden: User
//	"system:serviceaccount:kelson-system:kelson-controller" cannot patch
//	resource "environments" in API group "kelson.dev"
//
// and returned before it could write `.status` — an environment whose artifact
// was published and whose Flux Kustomization was healthy, reporting nothing at
// all. Asserting the verb here is cheap; discovering it needs a kind cluster,
// Flux, a registry and six minutes.
func TestControllerClusterRoleGrantsTheFinalizerPatch(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "controller.enabled=true")...))

	var rules []policyRule
	for _, doc := range docs {
		if doc["kind"] == "ClusterRole" && nameOf(doc) == "kelson-controller" {
			remarshal(t, doc["rules"], &rules)
		}
	}
	if rules == nil {
		t.Fatal("controller.enabled=true rendered no kelson-controller ClusterRole")
	}

	if !granted(rules, "kelson.dev", "environments", "patch") {
		t.Errorf("the controller ClusterRole does not grant patch on environments. That is the write "+
			"addFinalizer makes; environments/finalizers is the kubebuilder spelling and a CRD does not "+
			"serve it:\n%s", mustYAML(t, rules))
	}
	// The status write and the event the recorder emits at start-up: the other
	// two verbs the same run found missing or would have.
	if !granted(rules, "kelson.dev", "environments/status", "patch") {
		t.Errorf("the controller ClusterRole does not grant patch on environments/status:\n%s", mustYAML(t, rules))
	}
	if !granted(rules, "", "events", "create") {
		t.Errorf("the controller ClusterRole does not grant create on events, so leader election and "+
			"`kubectl describe environment` both go silent:\n%s", mustYAML(t, rules))
	}
	// And the separation the grant is bounded by: the controller must not be
	// able to write a Project spec, only its status.
	if granted(rules, "kelson.dev", "projects", "patch") || granted(rules, "kelson.dev", "projects", "update") {
		t.Errorf("the controller ClusterRole grants a write on projects. Only status is the "+
			"controller's to write (ADR-0027 decision 1):\n%s", mustYAML(t, rules))
	}
}

// TestControllerClusterRoleWorkloadReadback pins the grant the observation
// readback needs and, more usefully, the shape of what it must not have
// (issue #240, ADR-0028 decision 1 step 6).
//
// The grant is the smaller assertion: without it every reconcile writes
// `status.workloads.unavailable: … is forbidden` — honest, and useless, and
// discovered only in a cluster. The absences are the ones worth a test, because
// each of them is a plausible "while I'm here" edit that nothing else would
// catch: this controller reads workloads to *classify* them and for no other
// reason, and every verb past that is reach nobody asked for.
func TestControllerClusterRoleWorkloadReadback(t *testing.T) {
	rules := controllerClusterRole(t)

	// internal/controller/workloads.go: one label-selected list of Deployments,
	// then one selector list of Pods per Deployment.
	for _, want := range []struct {
		group, resource string
	}{
		{"apps", "deployments"},
		{"", "pods"},
	} {
		for _, verb := range []string{"get", "list"} {
			if !granted(rules, want.group, want.resource, verb) {
				t.Errorf("the controller ClusterRole does not grant %q on %s (apiGroup %q). "+
					"ClusterWorkloads.Observe needs it, and without it every environment reports "+
					"status.workloads.unavailable instead of which pod is failing:\n%s",
					verb, want.resource, want.group, mustYAML(t, rules))
			}
		}
	}

	// No watch: the readback rides a direct client on a loop that already
	// requeues. `watch` would be a stream of every pod event in the cluster,
	// for a call site that does not exist.
	for _, r := range []struct{ group, resource string }{{"apps", "deployments"}, {"", "pods"}} {
		if granted(rules, r.group, r.resource, "watch") {
			t.Errorf("the controller ClusterRole grants watch on %s. Nothing informs on it — "+
				"internal/controller/workloads.go lists through a direct client:\n%s",
				r.resource, mustYAML(t, rules))
		}
	}

	// No logs. The readback names the pod and the container; a container's
	// output is where a secret leaks and an Environment's status is not an
	// access-controlled place to put one.
	if granted(rules, "", "pods/log", "get") {
		t.Errorf("the controller ClusterRole grants get on pods/log. The workload readback writes "+
			"into a status any reader of the Environment can read, and logs must not go there — "+
			"`kelson logs` reads them under the server's own grant:\n%s", mustYAML(t, rules))
	}

	// And no write, anywhere in the cluster, on anything that is not one of
	// kelson's own kinds. Observation observes.
	for _, r := range rules {
		if contains(r.APIGroups, "kelson.dev") || contains(r.APIGroups, "coordination.k8s.io") {
			continue
		}
		for _, verb := range r.Verbs {
			switch verb {
			case "get", "list", "watch", "create", "patch":
				// `create`/`patch` survive here for exactly one resource, and
				// the next check names it.
			default:
				t.Errorf("the controller ClusterRole grants %q on %v (apiGroup %v) — a write verb "+
					"outside kelson's own kinds:\n%s", verb, r.Resources, r.APIGroups, mustYAML(t, rules))
			}
		}
		if contains(r.Verbs, "create") || contains(r.Verbs, "patch") {
			if !contains(r.Resources, "events") {
				t.Errorf("the controller ClusterRole grants a write on %v (apiGroup %v). Events are "+
					"the one non-kelson resource it may write:\n%s", r.Resources, r.APIGroups, mustYAML(t, rules))
			}
		}
	}
}

// controllerClusterRole renders the chart with the controller on and returns
// its ClusterRole's rules.
func controllerClusterRole(t *testing.T) []policyRule {
	t.Helper()
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "controller.enabled=true")...))
	var rules []policyRule
	for _, doc := range docs {
		if doc["kind"] == "ClusterRole" && nameOf(doc) == "kelson-controller" {
			remarshal(t, doc["rules"], &rules)
		}
	}
	if rules == nil {
		t.Fatal("controller.enabled=true rendered no kelson-controller ClusterRole")
	}
	return rules
}

// --- helpers ----------------------------------------------------------------

// nameOf reads metadata.name off a decoded document.
func nameOf(doc map[string]any) string {
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

func decodeDocs(t *testing.T, out string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(out))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the rendered output is not valid YAML: %v\n%s", err, out)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func serverContainer(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	containers := anySlice(serverPodSpec(t, docs)["containers"])
	if len(containers) != 1 {
		t.Fatalf("expected one container in the Deployment, got %d", len(containers))
	}
	c, _ := containers[0].(map[string]any)
	return c
}

// serverPodSpec is the pod spec of the first Deployment rendered, which is the
// server's: the controller's is off unless a test turns it on, and the tests
// that do name their own document.
func serverPodSpec(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		return podSpec
	}
	t.Fatal("no Deployment was rendered")
	return nil
}

// namedEntry finds the entry of a rendered list whose `name` is the one asked
// for — a volume, a volumeMount — so a test can assert about it by name rather
// than by position.
func namedEntry(list any, name string) map[string]any {
	for _, item := range anySlice(list) {
		entry, _ := item.(map[string]any)
		if entry["name"] == name {
			return entry
		}
	}
	return nil
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func stringsOf(v any) []string {
	items := anySlice(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func remarshal(t *testing.T, from any, into any) {
	t.Helper()
	data, err := yaml.Marshal(from)
	if err != nil {
		t.Fatalf("re-marshalling the rendered rules: %v", err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("re-parsing the rendered rules: %v", err)
	}
}

// TestGitSourceRBAC pins the two grants ADR-0035 decision 2 needs and the two
// it must not have.
//
// The server *reads* GitSources: the build path lists them and hands them to
// the resolver, so a component bound to one by name resolves to a repository
// (internal/api's globalSources). Without the grant a build of such a project
// fails with "this instance offers: nothing" — a true sentence about a listing
// that was forbidden and a false one about the instance, which is exactly the
// class of confusion RBAC assertions are cheap insurance against.
//
// The controller *writes their status*, and only that: GitSourceReconciler
// validates a document and records the verdict.
//
// The two absences are the interesting half. The server may not create,
// patch or delete a GitSource, because nothing in the schema authors one — an
// operator applies it with kubectl — and a server that could write one could
// repoint the repository every project builds from. And the server may not
// write `gitsources/status`, which is where this differs from the
// `gitconnections/status` grant beside it: that exception exists because no
// GitConnection reconciler does, and here one does.
func TestGitSourceRBAC(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "controller.enabled=true")...))

	var serverRules, controllerRules []policyRule
	for _, doc := range docs {
		switch {
		case doc["kind"] == "Role" && nameOf(doc) == "kelson-state":
			remarshal(t, doc["rules"], &serverRules)
		case doc["kind"] == "ClusterRole" && nameOf(doc) == "kelson-controller":
			remarshal(t, doc["rules"], &controllerRules)
		}
	}
	if serverRules == nil {
		t.Fatal("no kelson-state Role was rendered; the server holds no state grants at all")
	}
	if controllerRules == nil {
		t.Fatal("controller.enabled=true rendered no kelson-controller ClusterRole")
	}

	for _, verb := range []string{"get", "list"} {
		if !granted(serverRules, "kelson.dev", "gitsources", verb) {
			t.Errorf("the server's state Role does not grant %s on gitsources, so the build path cannot "+
				"read the instance's global tier (ADR-0035 decision 2):\n%s", verb, mustYAML(t, serverRules))
		}
	}
	for _, verb := range []string{"create", "patch", "update", "delete"} {
		if granted(serverRules, "kelson.dev", "gitsources", verb) {
			t.Errorf("the server's state Role grants %s on gitsources. Nothing in the schema authors a "+
				"GitSource, and a server that could write one could repoint what every project builds "+
				"from:\n%s", verb, mustYAML(t, serverRules))
		}
	}
	if granted(serverRules, "kelson.dev", "gitsources/status", "update") ||
		granted(serverRules, "kelson.dev", "gitsources/status", "patch") {
		t.Errorf("the server's state Role writes gitsources/status. That is the reconciler's, unlike "+
			"gitconnections/status, which the server owns only because no connection reconciler "+
			"exists:\n%s", mustYAML(t, serverRules))
	}

	for _, verb := range []string{"get", "list", "watch"} {
		if !granted(controllerRules, "kelson.dev", "gitsources", verb) {
			t.Errorf("the controller ClusterRole does not grant %s on gitsources, so the GitSource "+
				"reconciler's informer never syncs:\n%s", verb, mustYAML(t, controllerRules))
		}
	}
	if !granted(controllerRules, "kelson.dev", "gitsources/status", "patch") {
		t.Errorf("the controller ClusterRole does not grant patch on gitsources/status, so the "+
			"validation verdict is computed and never written:\n%s", mustYAML(t, controllerRules))
	}
	// And the same bound the Project rule carries: a validation loop must not
	// be able to edit the document it is judging (ADR-0027 decision 1).
	for _, verb := range []string{"patch", "update", "create", "delete"} {
		if granted(controllerRules, "kelson.dev", "gitsources", verb) {
			t.Errorf("the controller ClusterRole grants %s on gitsources. Only status is the "+
				"controller's to write:\n%s", verb, mustYAML(t, controllerRules))
		}
	}
}

// TestServerRoleCanCreateTheFirstProject pins the verb whose absence broke the
// first real "New project" click: the spec store writes with server-side
// apply, and an apply that brings a new object into existence is authorized as
// a create. With only `patch` granted the server could update every project
// that already existed and could not create one — and the e2e never notices,
// because it installs the chart with replicaCount=0 and the server pod is
// never scheduled there.
func TestServerRoleCanCreateTheFirstProject(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, authValues...))

	var serverRules []policyRule
	for _, doc := range docs {
		if doc["kind"] == "Role" && nameOf(doc) == "kelson-state" {
			remarshal(t, doc["rules"], &serverRules)
		}
	}
	if serverRules == nil {
		t.Fatal("no kelson-state Role was rendered; the server holds no state grants at all")
	}

	for _, resource := range []string{"projects", "environments"} {
		for _, verb := range []string{"create", "patch"} {
			if !granted(serverRules, "kelson.dev", resource, verb) {
				t.Errorf("the server's state Role does not grant %s on %s — PutSpec's server-side "+
					"apply needs both create and patch:\n%s", verb, resource, mustYAML(t, serverRules))
			}
		}
	}

	// The event stream: deploy streaming and the UI's transition feed both ride
	// controlstore.EnvironmentStore.Watch, which is a Kubernetes watch on
	// environments. Projects deliberately stay unwatched.
	if !granted(serverRules, "kelson.dev", "environments", "watch") {
		t.Errorf("the server's state Role does not grant watch on environments — the event "+
			"stream cannot start:\n%s", mustYAML(t, serverRules))
	}
	if granted(serverRules, "kelson.dev", "projects", "watch") {
		t.Errorf("the server's state Role grants watch on projects, which nothing performs:\n%s",
			mustYAML(t, serverRules))
	}
}

// TestServiceSelectsOnlyTheServer pins the selector the e2e server smoke found
// missing on its first run: the bare selector labels are carried by the
// controller's pods too, so a Service matching only name+instance sends a
// share of every connection to a pod with no API port. The component label on
// both sides is what keeps the Service's endpoints the server's alone.
func TestServiceSelectsOnlyTheServer(t *testing.T) {
	docs := decodeDocs(t, helmTemplate(t, append(authValues, "--set", "controller.enabled=true")...))

	var selector map[string]any
	var serverPodLabels, controllerPodLabels map[string]any
	for _, doc := range docs {
		switch {
		case doc["kind"] == "Service" && nameOf(doc) == "kelson":
			spec, _ := doc["spec"].(map[string]any)
			selector, _ = spec["selector"].(map[string]any)
		case doc["kind"] == "Deployment":
			spec, _ := doc["spec"].(map[string]any)
			tmpl, _ := spec["template"].(map[string]any)
			meta, _ := tmpl["metadata"].(map[string]any)
			labels, _ := meta["labels"].(map[string]any)
			if nameOf(doc) == "kelson" {
				serverPodLabels = labels
			} else if nameOf(doc) == "kelson-controller" {
				controllerPodLabels = labels
			}
		}
	}
	if selector == nil {
		t.Fatal("no kelson Service selector was rendered")
	}
	if serverPodLabels == nil || controllerPodLabels == nil {
		t.Fatal("expected both the server and controller Deployments to render")
	}

	matches := func(labels map[string]any) bool {
		for k, v := range selector {
			if labels[k] != v {
				return false
			}
		}
		return true
	}
	if !matches(serverPodLabels) {
		t.Errorf("the Service selector %v does not match the server pod labels %v", selector, serverPodLabels)
	}
	if matches(controllerPodLabels) {
		t.Errorf("the Service selector %v matches the controller pod labels %v — a share of every "+
			"connection would reach a pod with no API port", selector, controllerPodLabels)
	}
}
