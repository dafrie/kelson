package model

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Field-coverage harness for issue #141.
//
// The model used to accept fields nothing downstream consumed: an author wrote
// a data service or `policy:`, validation passed, and the renderer emitted
// nothing for it. Nobody noticed because nothing said anything.
//
// These tests make that failure impossible to reintroduce. Every field
// reachable from a Project or Environment spec must be accounted for exactly
// once: either it is on renderedFields below (something consumes it), or a row
// in notImplementedFields gates it (validation rejects it and names the
// milestone). A new field that is on neither list fails TestSpecFieldCoverage
// with instructions, which is the point — the default for a new field is
// "explain yourself", not "silently do nothing".

// renderedFields lists the spec paths that something actually consumes, with
// the consumer named. Paths are canonical: `[]` for a sequence entry, `*` for
// a map key, matching what specFieldPaths derives from the yaml tags.
//
// Adding a field here is a claim that it has an observable effect. If it does
// not, it belongs in notImplementedFields instead.
var renderedFields = map[string]map[string]string{
	KindProject: {
		"$.apiVersion":    "decode: rejects anything but kelson.dev/v1alpha1",
		"$.kind":          "decode: selects the document type",
		"$.metadata.name": "renderer: project label and resource naming",

		"$.spec.source.name":       "model/validate: refused — the singular spelling is already named `default`, and choosing a name means writing the list (ADR-0035 decision 1)",
		"$.spec.source.git":        "internal/build: clone URL (build.Request.SourceURL)",
		"$.spec.source.ref":        "internal/build: checkout ref (build.Request.SourceRef)",
		"$.spec.source.connection": "internal/forgeconn: which GitConnection the ref resolution and the build pod's clone authenticate with (ADR-0033 decision 4)",

		// The plural spelling and the per-component binding, ADR-0035 decisions
		// 1 and 3. What consumes them today is model/resolve: each entry becomes
		// a component binding in Resolved.Sources, which is hashed into the
		// artifact tag, and a name in neither scope is refused
		// (ref/unknown-source). The build plane clones per binding in the slice
		// that follows this one (#239); until it does, no document can be built
		// from the wrong repository by mistake — a project using the plural
		// spelling has no spec.source for the build plane to fall back to, so it
		// refuses with build/no-source rather than building something else, and
		// a name that resolves nowhere never reaches a build at all.
		"$.spec.sources[].name":       "model/resolve: the name a component binds to; the binding lands in Resolved.Sources and the build plane clones per component (ADR-0035 decisions 1 and 4, #239)",
		"$.spec.sources[].git":        "model/resolve: the clone URL of the bound source (Resolved.Sources[].source.git)",
		"$.spec.sources[].ref":        "model/resolve: the checkout ref of the bound source — per source rather than per project (Resolved.Sources[].source.ref)",
		"$.spec.sources[].connection": "model/resolve: which GitConnection this source's clone authenticates with, carried per binding (ADR-0033 decision 4, ADR-0035 decision 1)",

		"$.spec.build.strategy":   "internal/build/detect: strategy selection",
		"$.spec.build.dockerfile": "internal/build/detect: Dockerfile path",
		"$.spec.build.by":         "internal/api: BuildService.ReportBuild acts on a CI report only for `ci` — it renders and publishes the change request's preview — and declines one for `kelson`, whose images come from kelson's own build plane (ADR-0034 decision 3)",

		"$.spec.image":              "renderer: container image, and the P3 fallback for components",
		"$.spec.env.*":              "renderer: container env (literal form)",
		"$.spec.env.*.from.service": "renderer: secretKeyRef name — the credentials Secret of the bound data component",
		"$.spec.env.*.from.key":     "renderer: secretKeyRef key, mapped onto the operator's own key names",
		"$.spec.env.*.secret":       "renderer: secretKeyRef name — the Secret the author names, never read by kelson (ADR-0018)",
		"$.spec.env.*.key":          "renderer: secretKeyRef key within that Secret (ADR-0018)",

		"$.spec.components[].name":                      "renderer: workload, data-service or HelmRelease resource name, and the binding target",
		"$.spec.components[].kind":                      "model: selects workload, data or chart rendering, and which operator a component delegates to (postgres → CloudNativePG, valkey → the Valkey operator, helm → helm-controller)",
		"$.spec.components[].chart":                     "renderer: HelmRelease chart name (kind: helm)",
		"$.spec.components[].chartVersion":              "renderer: HelmRelease chart version pin, and the OCIRepository tag (kind: helm)",
		"$.spec.components[].source":                    "model/resolve: the name arm of the union — which declared source this component builds from, bound into Resolved.Sources (ADR-0035 decision 3)",
		"$.spec.components[].source.repository":         "renderer: HelmRepository url (kind: helm)",
		"$.spec.components[].source.oci":                "renderer: OCIRepository url (kind: helm)",
		"$.spec.components[].values.*":                  "renderer: HelmRelease spec.values, verbatim (kind: helm)",
		"$.spec.components[].valuesFrom[].secretRef":    "renderer: HelmRelease spec.valuesFrom, kind Secret (kind: helm)",
		"$.spec.components[].valuesFrom[].configMapRef": "renderer: HelmRelease spec.valuesFrom, kind ConfigMap (kind: helm)",
		"$.spec.components[].preset":                    "renderer: operator topology and sizing (docs/data-services.md)",
		"$.spec.components[].auth.secret":               "renderer: ValkeyCluster spec.users[].passwordSecret.name, and the secretKeyRef name of the component's password binding (ADR-0015 amendment)",
		"$.spec.components[].auth.key":                  "renderer: ValkeyCluster spec.users[].passwordSecret.keys[0], and the secretKeyRef key of the component's password binding (ADR-0015 amendment)",
		"$.spec.components[].image":                     "renderer: container image (P3 override)",
		"$.spec.components[].command":                   "renderer: container command",
		"$.spec.components[].port":                      "renderer: Service, containerPort, derived kind",
		"$.spec.components[].health":                    "renderer: liveness/readiness probes",
		"$.spec.components[].schedule":                  "renderer: CronJob schedule and derived kind",
		"$.spec.components[].domains":                   "renderer: HTTPRoute hostnames and Certificate",
		"$.spec.components[].replicas.min":              "renderer: replica count and HPA floor",
		"$.spec.components[].replicas.max":              "renderer: HPA ceiling",
		"$.spec.components[].resources.requests.cpu":    "renderer: container resource requests",
		"$.spec.components[].resources.requests.memory": "renderer: container resource requests",
		"$.spec.components[].resources.limits.cpu":      "renderer: container resource limits",
		"$.spec.components[].resources.limits.memory":   "renderer: container resource limits",
		"$.spec.components[].env.*":                     "renderer: container env (literal form)",
		"$.spec.components[].env.*.from.service":        "renderer: secretKeyRef name — the credentials Secret of the bound data component",
		"$.spec.components[].env.*.from.key":            "renderer: secretKeyRef key, mapped onto the operator's own key names",
		"$.spec.components[].env.*.secret":              "renderer: secretKeyRef name — the Secret the author names, never read by kelson (ADR-0018)",
		"$.spec.components[].env.*.key":                 "renderer: secretKeyRef key within that Secret (ADR-0018)",

		"$.spec.defaults.deliveryMode":            "resolve P4 → internal/delivery: adapter selection",
		"$.spec.defaults.policy.agents":           "resolve P4 → internal/api: propose-only refuses every agent mutation server-side (ADR-0025)",
		"$.spec.defaults.policy.require":          "resolve P4 → internal/api: require: [dry-run] obliges the server-side dry-run before an agent deploy applies (ADR-0025)",
		"$.spec.defaults.policy.maxReplicas":      "resolve P4 → internal/api: the replica ceiling an agent deploy is checked against (ADR-0025)",
		"$.spec.defaults.policy.protect":          "resolve P4 → internal/api: components an agent may not remove or scale to zero (ADR-0025)",
		"$.spec.defaults.policy.forbid":           "resolve P4 → internal/api: operations refused to agents in this environment (ADR-0025)",
		"$.spec.defaults.secrets.backend":         "resolve P4 → renderer: selects the reference mechanism; cluster renders secretKeyRefs, externalSecrets also renders an ExternalSecret per referenced Secret, sops adds the Kustomization decryption block and requires flux (ADR-0022)",
		"$.spec.defaults.secrets.store":           "resolve P4 → renderer: ExternalSecret spec.secretStoreRef, resolved against the ClusterProfile's stores (ADR-0020)",
		"$.spec.defaults.secrets.refreshInterval": "resolve P4 → renderer: ExternalSecret spec.refreshInterval (default 1h, ADR-0020)",
		"$.spec.defaults.secrets.ageRecipients":   "resolve P4 → internal/secret: the age public keys `kelson secret set` encrypts to under backend sops (ADR-0022)",
		"$.spec.defaults.secrets.ageKeySecret":    "resolve P4 → renderer: Kustomization spec.decryption.secretRef.name (default sops-age, ADR-0022)",

		"$.spec.overlays[].patch":    "renderer: strategic-merge patch against rendered resources",
		"$.spec.overlays[].manifest": "renderer: extra manifest emitted as-is",
	},
	KindEnvironment: {
		"$.apiVersion":    "decode: rejects anything but kelson.dev/v1alpha1",
		"$.kind":          "decode: selects the document type",
		"$.metadata.name": "renderer: environment label; default namespace",

		"$.spec.project":   "resolve: binds the Environment to its Project",
		"$.spec.namespace": "renderer: target namespace on every resource",

		"$.spec.secrets.backend":         "renderer: selects the reference mechanism; cluster renders secretKeyRefs, externalSecrets also renders an ExternalSecret per referenced Secret, sops adds the Kustomization decryption block and requires flux (ADR-0022)",
		"$.spec.secrets.store":           "renderer: ExternalSecret spec.secretStoreRef, resolved against the ClusterProfile's stores (ADR-0020)",
		"$.spec.secrets.refreshInterval": "renderer: ExternalSecret spec.refreshInterval (default 1h, ADR-0020)",
		"$.spec.secrets.ageRecipients":   "internal/secret: the age public keys `kelson secret set` encrypts to under backend sops (ADR-0022)",
		"$.spec.secrets.ageKeySecret":    "renderer: Kustomization spec.decryption.secretRef.name (default sops-age, ADR-0022)",

		"$.spec.routing.domainSuffix": "renderer: default hostname for ported components",
		"$.spec.routing.gatewayClass": "renderer: HTTPRoute parentRef",
		"$.spec.routing.tls":          "renderer: Certificate and HTTPRoute TLS",

		"$.spec.previews.provider":             "renderer: ResourceSetInputProvider spec.type (GitHubPullRequest / GitLabMergeRequest)",
		"$.spec.previews.repo":                 "renderer: ResourceSetInputProvider spec.url",
		"$.spec.previews.secretRef":            "renderer: ResourceSetInputProvider spec.secretRef.name — the author's Secret, or the one internal/controller materializes from the git connection when the field is unset (ADR-0033 decision 4)",
		"$.spec.previews.interval":             "renderer: the fluxcd.controlplane.io/reconcileEvery annotation on the ResourceSetInputProvider",
		"$.spec.previews.filter.labels":        "renderer: ResourceSetInputProvider spec.filter.labels",
		"$.spec.previews.filter.includeBranch": "renderer: ResourceSetInputProvider spec.filter.includeBranch",
		"$.spec.previews.filter.excludeBranch": "renderer: ResourceSetInputProvider spec.filter.excludeBranch",
		"$.spec.previews.filter.limit":         "renderer: ResourceSetInputProvider spec.filter.limit (default 10, ADR-0017)",
		"$.spec.previews.skip.labels":          "renderer: ResourceSetInputProvider spec.skip.labels",
		"$.spec.previews.artifacts.repository": "renderer: the per-preview OCIRepository url in the ResourceSet template",
		"$.spec.previews.artifacts.secretRef":  "renderer: the per-preview OCIRepository secretRef.name in the ResourceSet template",

		"$.spec.autoDeploy": "resolve → Resolved.AutoDeploy → internal/api's ReportBuild and internal/forgehttp's push: the environment follows its components' sources (ADR-0036)",

		"$.spec.policy.agents":      "internal/api: propose-only refuses every agent mutation server-side (ADR-0025)",
		"$.spec.policy.require":     "internal/api: require: [dry-run] obliges the server-side dry-run before an agent deploy applies (ADR-0025)",
		"$.spec.policy.maxReplicas": "internal/api: the replica ceiling an agent deploy is checked against (ADR-0025)",
		"$.spec.policy.protect":     "internal/api: components an agent may not remove or scale to zero (ADR-0025)",
		"$.spec.policy.forbid":      "internal/api: operations refused to agents in this environment (ADR-0025)",

		// ADR-0028 deletes this block outright; until that lands, what still
		// reads it is stated exactly. `mode` decides four renderer gates, so it
		// has an observable effect on what renders. The git target has lost its
		// only consumer — the writer that committed to it — and what remains is
		// validate.go requiring it for flux mode, which is an observable effect
		// on whether a document is accepted rather than on what it renders.
		"$.spec.delivery.mode":       "renderer: the helm / previews / sops / release mode gates (ADR-0016, ADR-0017, ADR-0019, ADR-0022)",
		"$.spec.delivery.git.repo":   "model/validate: semantic/git-target-missing requires it for flux mode (the writer that used it is deleted — ADR-0028, issue #224)",
		"$.spec.delivery.git.branch": "model/validate: part of the git target semantic/git-target-missing requires (the writer that used it is deleted — ADR-0028, issue #224)",
		"$.spec.delivery.git.path":   "model/validate: part of the git target semantic/git-target-missing requires (the writer that used it is deleted — ADR-0028, issue #224)",

		"$.spec.components[].name":                      "resolve P1/P2/P5: selects the Project component to override",
		"$.spec.components[].image":                     "resolve P3 → renderer: the per-environment image pin, the promotion primitive (ADR-0016)",
		"$.spec.components[].replicas.min":              "renderer: replica count and HPA floor",
		"$.spec.components[].replicas.max":              "renderer: HPA ceiling",
		"$.spec.components[].resources.requests.cpu":    "renderer: container resource requests",
		"$.spec.components[].resources.requests.memory": "renderer: container resource requests",
		"$.spec.components[].resources.limits.cpu":      "renderer: container resource limits",
		"$.spec.components[].resources.limits.memory":   "renderer: container resource limits",
		"$.spec.components[].env.*":                     "renderer: container env (literal form)",
		"$.spec.components[].env.*.from.service":        "renderer: secretKeyRef name — the credentials Secret of the bound data component",
		"$.spec.components[].env.*.from.key":            "renderer: secretKeyRef key, mapped onto the operator's own key names",
		"$.spec.components[].env.*.secret":              "renderer: secretKeyRef name — the Secret the author names, never read by kelson (ADR-0018)",
		"$.spec.components[].env.*.key":                 "renderer: secretKeyRef key within that Secret (ADR-0018)",
		"$.spec.components[].autoDeploy":                "resolve → Resolved.AutoDeploy: this component's own tracking answer, which beats the environment's (ADR-0036 decision 1)",
		"$.spec.components[].imageTracked":              "resolve → Resolved.ImagePins: the image named here is a starting point rather than a hold, so the stale set may still move it and internal/api's trigger overwrites it on the next push (ADR-0036 decision 5)",
		"$.spec.components[].preset":                    "resolve P5 → renderer: the per-environment CNPG topology",

		"$.spec.overlays[].patch":    "renderer: strategic-merge patch against rendered resources",
		"$.spec.overlays[].manifest": "renderer: extra manifest emitted as-is",
	},
}

// specDocuments are the documents the harness covers: the two *authoring*
// documents, whose fields are read by the renderer and the planes around it.
//
// GitConnection is deliberately absent, and the omission is the harness's own
// premise rather than an oversight. Every field of a connection is read at use
// time by a plane with cluster access — the server minting a token, the build
// pod cloning, the controller calling a forge API (ADR-0033 decision 1) — and
// none of it resolves, renders or reaches a manifest. There is nothing for
// renderedFields to name a consumer of and nothing for the resolver to drop on
// the floor, which is the silence issue #141 is about. What a connection's
// fields are held to instead is validate.go, which refuses every one it can
// judge from the document alone.
func specDocuments() map[string]reflect.Type {
	return map[string]reflect.Type{
		KindProject:     reflect.TypeOf(Project{}),
		KindEnvironment: reflect.TypeOf(Environment{}),
	}
}

// TestSpecFieldCoverage is the guard of issue #141: no field may be silent by
// default. Every leaf of every spec document must be claimed by exactly one of
// the two lists.
func TestSpecFieldCoverage(t *testing.T) {
	for kind, typ := range specDocuments() {
		t.Run(kind, func(t *testing.T) {
			rendered := renderedFields[kind]
			for _, path := range specFieldPaths(typ) {
				_, isRendered := rendered[path]
				gate, isGated := gatedBy(kind, path)
				switch {
				case isRendered && isGated:
					t.Errorf("%s %s is both on renderedFields and gated by %q — decide which is true: "+
						"if the field now renders, delete the gate row from notImplementedFields; "+
						"if it does not, delete the renderedFields entry", kind, path, gate.Path)
				case !isRendered && !isGated:
					t.Errorf(`%s %s is reachable in the spec but neither rendered nor gated (issue #141).

A field that validates and renders nothing tells an author their spec worked when it did not.
Do one of these:

  1. If something consumes this field, add it to renderedFields in this file,
     naming the consumer (e.g. "renderer: container env").
  2. If nothing consumes it yet, add a row to notImplementedFields in
     internal/model/notimplemented.go with the milestone that will implement it,
     and call v.gate() from the validator so the field is rejected. Add an
     enforcement case to gateEnforcement in this file.`, kind, path)
				}
			}
		})
	}
}

// TestRenderedFieldsAreReal keeps the allow-list honest in the other
// direction: a field renamed or removed from the model must not leave a stale
// claim behind that would cover a future field of the same name.
func TestRenderedFieldsAreReal(t *testing.T) {
	for kind, typ := range specDocuments() {
		actual := map[string]bool{}
		for _, p := range specFieldPaths(typ) {
			actual[p] = true
		}
		for path := range renderedFields[kind] {
			if !actual[path] {
				t.Errorf("renderedFields[%s] claims %q, which no longer exists in the spec; remove it", kind, path)
			}
		}
	}
}

// TestGateTableIsReal is the same check for the gate table: a gate on a path
// the model no longer has would silently stop gating anything.
func TestGateTableIsReal(t *testing.T) {
	docs := specDocuments()
	for _, g := range notImplementedFields {
		typ, ok := docs[g.Kind]
		if !ok {
			t.Errorf("gate %q has unknown kind %q", g.Path, g.Kind)
			continue
		}
		covered := false
		for _, p := range specFieldPaths(typ) {
			if p == g.Path || strings.HasPrefix(p, g.Path+".") || strings.HasPrefix(p, g.Path+"[") {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("gate %s %q covers no field in the spec; remove the row or fix the path", g.Kind, g.Path)
		}
		if g.TrackedBy == "" || g.What == "" {
			t.Errorf("gate %s %q must name what it gates and where it is tracked", g.Kind, g.Path)
		}
	}
}

// gateEnforcement pins every gate row to a document that must trigger it. A
// row in the table with no call site in the validator gates nothing, which is
// exactly the silence issue #141 is about.
var gateEnforcement = map[string]string{
	KindProject + " $.spec.components[].tools": `
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
    - {name: triage, kind: agent, tools: [search, deploy]}`,

	KindProject + " $.spec.defaults.policy.deployers": `
spec:
  image: i:1
  components:
    - {name: web, port: 8080}
  defaults:
    policy: {agents: allow, deployers: [platform-team]}`,

	KindProject + " $.spec.components[].release": `
spec:
  image: i:1
  components:
    - {name: web, port: 8080, release: {command: ["./manage.py", "migrate"]}}`,

	KindEnvironment + " $.spec.cluster": `
spec:
  project: p
  cluster: prod-eu`,

	KindEnvironment + " $.spec.policy.deployers": `
spec:
  project: p
  policy: {agents: allow, require: [dry-run], deployers: [team]}`,
}

// TestGateTableIsEnforced renders each gated field into a document and demands
// a schema/not-implemented error naming it, so the table cannot drift away
// from the validator.
func TestGateTableIsEnforced(t *testing.T) {
	for _, g := range notImplementedFields {
		key := g.Kind + " " + g.Path
		t.Run(key, func(t *testing.T) {
			body, ok := gateEnforcement[key]
			if !ok {
				t.Fatalf("gate %s has no case in gateEnforcement; add a document that carries the field "+
					"so the gate is proven to fire", key)
			}
			src := "apiVersion: " + APIVersion + "\nkind: " + g.Kind + "\nmetadata: {name: p}\n" + body + "\n"
			_, errs := DecodeDocuments([]byte(src))

			// A binding case has to declare the service it binds to, so the
			// document trips more than one gate: match on the field, not on
			// "the first not-implemented error".
			var got *Error
			for i := range errs {
				if errs[i].Code != ErrNotImplemented {
					continue
				}
				if strings.HasPrefix(canonicalize(errs[i].Field), g.Path) {
					got = &errs[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("%s must be rejected as %s, got:\n%v", g.Path, ErrNotImplemented, errs)
			}
			if !strings.Contains(got.Remediation, g.TrackedBy) {
				t.Errorf("remediation must name where the work is tracked (%q), got %q", g.TrackedBy, got.Remediation)
			}
			if !strings.Contains(got.Remediation, "#141") {
				t.Errorf("remediation should cite issue #141, got %q", got.Remediation)
			}
			if got.Line == 0 {
				t.Errorf("gate error must carry a source line: %+v", got)
			}
		})
	}
}

// TestGateErrorsAreStructured holds gate errors to the same bar as every other
// code in the taxonomy (issue #28).
func TestGateErrorsAreStructured(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  cluster: eu-west
`))
	var got *Error
	for i := range errs {
		if errs[i].Code == ErrNotImplemented {
			got = &errs[i]
		}
	}
	if got == nil {
		t.Fatalf("spec.cluster must be gated, got:\n%v", errs)
	}
	if got.Field != "$.spec.cluster" {
		t.Errorf("field = %q, want $.spec.cluster", got.Field)
	}
	if got.Line != 6 {
		t.Errorf("line = %d, want 6 (the cluster: key)", got.Line)
	}
	if got.DocsURL != DocsBaseURL+"/schema-not-implemented" {
		t.Errorf("docsURL = %q", got.DocsURL)
	}
	if got.Resource == "" || got.Message == "" || got.Remediation == "" {
		t.Errorf("gate error missing structure: %+v", got)
	}
}

// canonicalize collapses a concrete error path ($.spec.components[2].env.DB)
// into the canonical form the tables use ($.spec.components[].env.*).
func canonicalize(path string) string {
	var out strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '[' {
			out.WriteString("[]")
			for i < len(path) && path[i] != ']' {
				i++
			}
			continue
		}
		out.WriteByte(path[i])
	}
	// Map keys are the segments the schema does not name. The only maps in the
	// spec are env maps, and a path holds at most one, so the segment after
	// ".env." is the key and collapses to "*".
	s := out.String()
	idx := strings.Index(s, ".env.")
	if idx < 0 {
		return s
	}
	rest := s[idx+len(".env."):]
	end := strings.IndexAny(rest, ".[")
	if end < 0 {
		return s[:idx] + ".env.*"
	}
	return s[:idx] + ".env.*" + rest[end:]
}

// specFieldPaths derives every leaf path of a document type from its yaml
// tags — the same tag walk decode.go uses for unknown-field detection, so the
// two agree on what a field is. Sequences collapse to `[]`, maps to `*`, and a
// sequence of scalars is a leaf at the sequence itself (`domains`, not
// `domains[]`).
func specFieldPaths(t reflect.Type) []string {
	var out []string
	walkFieldPaths(t, "$", &out, nil)
	sort.Strings(out)
	return slices.Compact(out)
}

func walkFieldPaths(t reflect.Type, path string, out *[]string, stack []reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if slices.Contains(stack, t) { // no recursive types today; cheap insurance
		return
	}
	stack = append(stack, t)

	// EnvValue is a union with a custom unmarshaller and no yaml tags: a plain
	// scalar, {from: {service, key}}, or {secret: <name>, key: <key>}. All
	// three arms are spec surface. The secret reference is walked at `path`
	// itself because it is written flat — `secret:` carries the name, so there
	// is no wrapper key to descend through.
	if t == reflect.TypeOf(EnvValue{}) {
		*out = append(*out, path)
		walkFieldPaths(reflect.TypeOf(ServiceBinding{}), path+".from", out, stack)
		walkFieldPaths(reflect.TypeOf(SecretRef{}), path, out, stack)
		return
	}

	// ComponentSource is the other union with a custom unmarshaller and no yaml
	// tags: a scalar is a source name (ADR-0035) and a mapping is a chart source
	// (ADR-0016). Both arms are spec surface, and the scalar one is a leaf at
	// the key itself, exactly as an env value's plain-string arm is.
	if t == reflect.TypeOf(ComponentSource{}) {
		*out = append(*out, path)
		walkFieldPaths(reflect.TypeOf(ChartSource{}), path, out, stack)
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("yaml")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if f.Anonymous && strings.Contains(tag, "inline") {
				walkFieldPaths(f.Type, path, out, stack)
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			walkFieldPaths(f.Type, path+"."+name, out, stack)
		}
	case reflect.Slice, reflect.Array:
		if isLeafKind(deref(t.Elem())) {
			*out = append(*out, path) // []string and friends are one field
			return
		}
		walkFieldPaths(t.Elem(), path+"[]", out, stack)
	case reflect.Map:
		walkFieldPaths(t.Elem(), path+".*", out, stack)
	default:
		*out = append(*out, path)
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isLeafKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return false
	}
	return true
}

// TestSpecFieldPathsWalksTheModel guards the walker itself: a broken
// enumerator would make TestSpecFieldCoverage pass vacuously.
func TestSpecFieldPathsWalksTheModel(t *testing.T) {
	got := specFieldPaths(reflect.TypeOf(Project{}))
	for _, want := range []string{
		"$.apiVersion",
		"$.metadata.name",
		"$.spec.components[].name",
		"$.spec.components[].kind",
		"$.spec.components[].domains",
		"$.spec.components[].replicas.min",
		"$.spec.components[].resources.limits.memory",
		"$.spec.components[].env.*",
		"$.spec.components[].env.*.from.service",
		"$.spec.components[].env.*.secret",
		"$.spec.components[].env.*.key",
		"$.spec.components[].preset",
		"$.spec.components[].tools",
		"$.spec.defaults.policy.deployers",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("specFieldPaths missing %q; got:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if slices.Contains(got, "$.spec.components[].domains[]") {
		t.Errorf("a sequence of scalars must be one leaf, not an indexed one")
	}
	if len(got) < 30 {
		t.Errorf("walker found only %d paths, which cannot be the whole model: %v", len(got), got)
	}
}
