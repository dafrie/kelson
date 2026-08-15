package model

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Sources: declared per project or globally, bound per component (ADR-0035).
//
// The split these tests hold the line on is the ADR's: what a document can be
// judged on alone is validation, and what needs the instance's list is
// resolution. A duplicate name is a validation error; a name that resolves
// nowhere is not, because it may be a GitSource this document cannot see.

const twoSourceProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://github.com/acme/build-tools
      ref: v2
      connection: acme-github
  components:
    - {name: web, port: 8080, source: app}
    - {name: worker, source: tools}
`

const anyEnvironment = `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
`

// codes reports the validation codes a document produces, so a case can assert
// on the taxonomy rather than on message text.
func codes(t *testing.T, src string) []Code {
	t.Helper()
	_, errs := DecodeDocuments([]byte(src))
	return errs.Codes()
}

// errorAt returns the first error carrying a code, and fails when there is none.
func errorAt(t *testing.T, errs Errors, code Code) Error {
	t.Helper()
	for _, e := range errs {
		if e.Code == code {
			return e
		}
	}
	t.Fatalf("no %s error in:\n%v", code, errs)
	return Error{}
}

func TestSourcesListValidates(t *testing.T) {
	if got := codes(t, twoSourceProject); len(got) != 0 {
		t.Fatalf("a two-source project must validate, got %v", got)
	}
}

// TestSourceSpellingsAreExclusive: the singular block and the list are one list
// written two ways, so a document writing both has not said which (ADR-0035
// decision 1).
func TestSourceSpellingsAreExclusive(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout}
  sources:
    - {name: app, git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
`))
	e := errorAt(t, errs, ErrMutuallyExclusive)
	if e.Field != "$.spec.sources" {
		t.Errorf("field = %q, want $.spec.sources", e.Field)
	}
	if !strings.Contains(e.Remediation, DefaultSourceName) {
		t.Errorf("the remediation must name the fix — moving the shorthand in as %q: %q",
			DefaultSourceName, e.Remediation)
	}
}

// TestShorthandRefusesItsOwnName: the singular spelling declares exactly one
// source and it is already named, so `name:` there would be a second way to say
// what the list says.
func TestShorthandRefusesItsOwnName(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {name: app, git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
`))
	e := errorAt(t, errs, ErrMutuallyExclusive)
	if e.Field != "$.spec.source.name" {
		t.Errorf("field = %q, want $.spec.source.name", e.Field)
	}
}

func TestSourceListRules(t *testing.T) {
	cases := []struct {
		name  string
		spec  string
		want  Code
		field string
	}{
		{
			name: "duplicate names",
			spec: `
  sources:
    - {name: app, git: https://github.com/acme/one}
    - {name: app, git: https://github.com/acme/two}
  components:
    - {name: web, port: 8080, source: app}`,
			want:  ErrDuplicateName,
			field: "$.spec.sources[1].name",
		},
		{
			name: "an entry with no name",
			spec: `
  sources:
    - {git: https://github.com/acme/one}
  components:
    - {name: web, port: 8080}`,
			want:  ErrMissingRequired,
			field: "$.spec.sources[0].name",
		},
		{
			name: "an entry with no git",
			spec: `
  sources:
    - {name: app}
  components:
    - {name: web, port: 8080, source: app}`,
			want:  ErrMissingRequired,
			field: "$.spec.sources[0].git",
		},
		{
			name: "a name that is not a label",
			spec: `
  sources:
    - {name: App_1, git: https://github.com/acme/one}
  components:
    - {name: web, port: 8080}`,
			want:  ErrInvalidFormat,
			field: "$.spec.sources[0].name",
		},
		{
			name: "a connection that is not a name",
			spec: `
  sources:
    - {name: app, git: https://github.com/acme/one, connection: "Not A Name"}
  components:
    - {name: web, port: 8080}`,
			want:  ErrInvalidFormat,
			field: "$.spec.sources[0].connection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(
				"apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata: {name: checkout}\nspec:" + tc.spec + "\n"))
			e := errorAt(t, errs, tc.want)
			if e.Field != tc.field {
				t.Errorf("field = %q, want %q (%v)", e.Field, tc.field, errs)
			}
		})
	}
}

// TestMissingDefaultSourceNamesTheCandidates is the refusal ADR-0035 decision 3
// argues for by name: a list's order is not a decision, so kelson says which
// sources it could have meant instead of picking one.
func TestMissingDefaultSourceNamesTheCandidates(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: app, git: https://github.com/acme/checkout}
    - {name: tools, git: https://github.com/acme/build-tools}
  components:
    - {name: web, port: 8080, source: app}
    - {name: worker}
`))
	e := errorAt(t, errs, ErrNoDefaultSource)
	if e.Field != "$.spec.components[1].source" {
		t.Errorf("field = %q, want the unbound component's source", e.Field)
	}
	for _, want := range []string{"app", "tools", DefaultSourceName} {
		if !strings.Contains(e.Remediation, want) {
			t.Errorf("the remediation must name %q: %q", want, e.Remediation)
		}
	}
}

// TestDefaultSourceCoversTheOneEntryList: a list of one is not a decision, so
// the entry is the default whatever it is called.
func TestDefaultSourceCoversTheOneEntryList(t *testing.T) {
	if got := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: app, git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
`); len(got) != 0 {
		t.Fatalf("one source needs no default entry, got %v", got)
	}
}

// TestNamedDefaultSatisfiesUnboundComponents: with a `default` in the list,
// naming a source is optional again.
func TestNamedDefaultSatisfiesUnboundComponents(t *testing.T) {
	if got := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: default, git: https://github.com/acme/checkout}
    - {name: tools, git: https://github.com/acme/build-tools}
  components:
    - {name: web, port: 8080}
    - {name: worker, source: tools}
`); len(got) != 0 {
		t.Fatalf("a default entry binds every unbound component, got %v", got)
	}
}

// TestImagePinnedComponentNeedsNoSource: a component that is not built needs no
// binding, so the missing default is not its problem.
func TestImagePinnedComponentNeedsNoSource(t *testing.T) {
	if got := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: app, git: https://github.com/acme/checkout}
    - {name: tools, git: https://github.com/acme/build-tools}
  components:
    - {name: web, port: 8080, source: app}
    - {name: sidecar, image: ghcr.io/acme/sidecar:1}
`); len(got) != 0 {
		t.Fatalf("an image-pinned component needs no source, got %v", got)
	}
}

// TestSourceNameIsAnImageSource: naming a source is naming where the image
// comes from, even when the Project declares no sources of its own — the name
// may be a GitSource the instance offers, which this document cannot see.
func TestSourceNameIsAnImageSource(t *testing.T) {
	if got := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  components:
    - {name: web, port: 8080, source: shared-tools}
`); len(got) != 0 {
		t.Fatalf("a component naming a source has an image source, got %v", got)
	}
}

// TestSourceArmsAreHeldToTheKind: `source:` carries two things and the kind
// decides which one is meaningful. The other is refused, never ignored
// (issue #141).
func TestSourceArmsAreHeldToTheKind(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{
			name: "a source name on a data component",
			spec: `
  sources:
    - {name: app, git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
    - {name: db, kind: postgres, source: app}`,
		},
		{
			name: "a source name on a helm component",
			spec: `
  image: ghcr.io/acme/checkout:1
  components:
    - {name: web, port: 8080}
    - {name: ingress, kind: helm, chart: ingress-nginx, chartVersion: 4.11.3, source: app}`,
		},
		{
			name: "a chart source on a workload",
			spec: `
  image: ghcr.io/acme/checkout:1
  components:
    - name: web
      port: 8080
      source: {repository: https://kubernetes.github.io/ingress-nginx}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codes(t, "apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata: {name: checkout}\nspec:"+
				tc.spec+"\n")
			if !slices.Contains(got, ErrMutuallyExclusive) {
				t.Fatalf("want %s, got %v", ErrMutuallyExclusive, got)
			}
		})
	}
}

// TestChartSourceStillValidates: the union did not cost the helm component the
// checks ADR-0016 gave it.
func TestChartSourceStillValidates(t *testing.T) {
	both := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: platform}
spec:
  image: ghcr.io/acme/platform:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source:
        repository: https://kubernetes.github.io/ingress-nginx
        oci: oci://ghcr.io/acme/charts
`)
	if !slices.Contains(both, ErrMutuallyExclusive) {
		t.Errorf("both chart source forms must still be refused, got %v", both)
	}
	unknown := codes(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: platform}
spec:
  image: ghcr.io/acme/platform:1
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx
      chartVersion: 4.11.3
      source: {registry: oci://ghcr.io/acme/charts}
`)
	if !slices.Contains(unknown, ErrUnknownField) {
		t.Errorf("an unknown key in the chart arm must still be reported, got %v", unknown)
	}
}

// TestComponentSourceRoundTrips: both arms survive a decode/encode cycle in
// both encodings, because a Project is carried over the wire and into a custom
// resource and must come back the document that was written.
func TestComponentSourceRoundTrips(t *testing.T) {
	for _, body := range []string{"app", "{repository: https://charts.example.com}"} {
		t.Run(body, func(t *testing.T) {
			src := "name: web\nsource: " + body + "\n"
			var c Component
			if err := yaml.Unmarshal([]byte(src), &c); err != nil {
				t.Fatalf("decoding %q: %v", src, err)
			}
			out, err := yaml.Marshal(c)
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			var back Component
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatalf("re-decoding %q: %v", out, err)
			}
			if back.SourceName() != c.SourceName() {
				t.Errorf("source name %q became %q", c.SourceName(), back.SourceName())
			}
			if (back.ChartSourceOf() == nil) != (c.ChartSourceOf() == nil) {
				t.Errorf("the chart arm did not survive: %+v", back.Source)
			}

			data, err := json.Marshal(c)
			if err != nil {
				t.Fatalf("marshalling JSON: %v", err)
			}
			var fromJSON Component
			if err := json.Unmarshal(data, &fromJSON); err != nil {
				t.Fatalf("unmarshalling %s: %v", data, err)
			}
			if fromJSON.SourceName() != c.SourceName() {
				t.Errorf("JSON source name %q became %q", c.SourceName(), fromJSON.SourceName())
			}
			if chart := fromJSON.ChartSourceOf(); (chart == nil) != (c.ChartSourceOf() == nil) {
				t.Errorf("the chart arm did not survive JSON: %s", data)
			}
		})
	}
}

// TestComponentSourceRefusesASequence: the union is two shapes, and a list is
// neither. The remediation has to say both, because an author who wrote the
// wrong shape is choosing between them.
func TestComponentSourceRefusesASequence(t *testing.T) {
	_, errs := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: i:1
  components:
    - name: web
      port: 8080
      source: [app, tools]
`))
	e := errorAt(t, errs, ErrInvalidFormat)
	if e.Remediation != ComponentSourceRemediation {
		t.Errorf("remediation = %q, want the component-source remediation", e.Remediation)
	}
}

// TestResolveBindsEveryBuildableComponent is the output slice 2 consumes: one
// binding per component that builds, carrying that component's repository, ref
// and connection (ADR-0035 decisions 3 and 4).
func TestResolveBindsEveryBuildableComponent(t *testing.T) {
	p, e := loadPair(t, twoSourceProject, anyEnvironment)
	r, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}

	web := r.SourceFor("web")
	if web == nil || web.Name != "app" || web.Git != "https://github.com/acme/checkout" || web.Ref != "main" {
		t.Fatalf("web bound to %+v, want the app source at main", web)
	}
	worker := r.SourceFor("worker")
	if worker == nil || worker.Name != "tools" || worker.Ref != "v2" || worker.Connection != "acme-github" {
		t.Fatalf("worker bound to %+v, want the tools source at v2 through acme-github", worker)
	}
	if r.Source != nil {
		t.Errorf("a project declaring two sources and no default has no project default: %+v", r.Source)
	}
	for _, c := range r.Components {
		if c.Image != ImageUnresolved {
			t.Errorf("component %q image = %q, want the unresolved sentinel: it is built from its source",
				c.Name, c.Image)
		}
	}
}

// TestResolveShorthandBindsEveryComponent: the singular spelling means what it
// always meant, and every component gets it under the name `default`.
func TestResolveShorthandBindsEveryComponent(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout, ref: main, connection: acme-github}
  components:
    - {name: web, port: 8080}
    - {name: worker}
`, anyEnvironment)
	r, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Source == nil || r.Source.Name != DefaultSourceName || r.Source.Connection != "acme-github" {
		t.Fatalf("project default = %+v, want the shorthand named %q", r.Source, DefaultSourceName)
	}
	for _, name := range []string{"web", "worker"} {
		bound := r.SourceFor(name)
		if bound == nil || bound.Name != DefaultSourceName || bound.Git != "https://github.com/acme/checkout" {
			t.Errorf("%s bound to %+v, want the project default", name, bound)
		}
	}
}

// TestResolveTakesGlobalSourcesAsInput: the instance's tier is a parameter, the
// way a ClusterProfile is (ADR-0029). Nothing here reads a cluster.
func TestResolveTakesGlobalSourcesAsInput(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
    - {name: docs, source: handbook}
`, anyEnvironment)

	if _, errs := Resolve(p, e); len(errs) == 0 {
		t.Fatal("a name in neither scope must be refused when no global sources are supplied")
	} else {
		refusal := errorAt(t, errs, ErrUnknownSource)
		if refusal.Field != "$.spec.components[1].source" {
			t.Errorf("field = %q, want the binding that failed", refusal.Field)
		}
		for _, want := range []string{DefaultSourceName, "nothing"} {
			if !strings.Contains(refusal.Remediation, want) {
				t.Errorf("the refusal must list what was in scope (%q): %q", want, refusal.Remediation)
			}
		}
	}

	global := Source{Name: "handbook", Git: "https://github.com/acme/handbook", Ref: "trunk"}
	r, errs := Resolve(p, e, global)
	if len(errs) > 0 {
		t.Fatalf("resolve with the global tier: %v", errs)
	}
	docs := r.SourceFor("docs")
	if docs == nil || docs.Git != global.Git || docs.Ref != "trunk" {
		t.Fatalf("docs bound to %+v, want the instance's handbook source", docs)
	}
}

// TestProjectSourceShadowsGlobal: innermost scope wins, which is the same
// instinct as P1–P3 (ADR-0035 decision 3).
func TestProjectSourceShadowsGlobal(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: default, git: https://github.com/acme/checkout}
    - {name: tools, git: https://github.com/acme/our-tools}
  components:
    - {name: web, port: 8080}
    - {name: builder, source: tools}
`, anyEnvironment)
	r, errs := Resolve(p, e, Source{Name: "tools", Git: "https://github.com/platform/tools"})
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if bound := r.SourceFor("builder"); bound == nil || bound.Git != "https://github.com/acme/our-tools" {
		t.Fatalf("builder bound to %+v, want the project's own tools source", bound)
	}
}

// TestResolveBindsNothingWithoutASource: an image-only project keeps today's
// meaning exactly — no bindings, and a resolved spec whose JSON is what it was
// before sources existed.
func TestResolveBindsNothingWithoutASource(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  components:
    - {name: web, port: 8080}
`, anyEnvironment)
	r, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Source != nil || len(r.Sources) != 0 {
		t.Fatalf("an image-only project binds nothing: %+v / %+v", r.Source, r.Sources)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshalling the resolved spec: %v", err)
	}
	if strings.Contains(string(data), `"sources"`) {
		t.Errorf("an empty binding list must marshal to nothing, or every environment retags: %s", data)
	}
}

// gitSourceDoc wraps a spec body in the document envelope, so the cases below
// read as the YAML an author writes rather than as a Go literal.
func gitSourceDoc(name, spec string) string {
	return "apiVersion: " + APIVersion + "\nkind: " + KindGitSource +
		"\nmetadata: {name: " + name + "}\nspec:\n" + spec
}

// TestValidGitSource: the global tier of ADR-0035 decision 2, decoded as its own
// kind and offered to the resolver as a plain Source.
func TestValidGitSource(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(gitSourceDoc("build-tools",
		"  git: https://github.com/acme/build-tools\n  ref: v2\n  connection: acme-github\n  owner: {kind: instance}\n")))
	if len(errs) != 0 {
		t.Fatalf("expected a valid source, got:\n%v", errs)
	}
	g, ok := docs[0].(*GitSource)
	if !ok {
		t.Fatalf("expected *GitSource, got %T", docs[0])
	}
	if !g.Spec.IsInstanceOwned() {
		t.Error("a source with owner.kind instance is instance-owned")
	}
	if errs := ValidateGitSource(g); len(errs) != 0 {
		t.Errorf("ValidateGitSource disagrees with the decode-time pass:\n%v", errs)
	}
	want := Source{Name: "build-tools", Git: "https://github.com/acme/build-tools", Ref: "v2", Connection: "acme-github"}
	if got := g.AsSource(); got != want {
		t.Errorf("AsSource() = %+v, want %+v — the name comes from metadata", got, want)
	}
}

func TestGitSourceValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		docName string
		spec    string
		code    Code
		field   string
	}{
		{
			name:    "no repository",
			docName: "build-tools",
			spec:    "  ref: main\n",
			code:    ErrMissingRequired,
			field:   "$.spec.git",
		},
		{
			name:    "a name that is not a label",
			docName: "Build_Tools",
			spec:    "  git: https://github.com/acme/build-tools\n",
			code:    ErrInvalidFormat,
			field:   "$.metadata.name",
		},
		{
			name:    "a connection that is not a name",
			docName: "build-tools",
			spec:    "  git: https://github.com/acme/build-tools\n  connection: \"Not A Name\"\n",
			code:    ErrInvalidFormat,
			field:   "$.spec.connection",
		},
		{
			name:    "an instance owner naming a principal",
			docName: "build-tools",
			spec:    "  git: https://github.com/acme/build-tools\n  owner: {kind: instance, name: alice}\n",
			code:    ErrMutuallyExclusive,
			field:   "$.spec.owner.name",
		},
		{
			name:    "a user owner naming nobody",
			docName: "build-tools",
			spec:    "  git: https://github.com/acme/build-tools\n  owner: {kind: user}\n",
			code:    ErrMissingRequired,
			field:   "$.spec.owner.name",
		},
		{
			name:    "a field the kind does not have",
			docName: "build-tools",
			spec:    "  git: https://github.com/acme/build-tools\n  provider: github\n",
			code:    ErrUnknownField,
			field:   "$.spec.provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := DecodeDocuments([]byte(gitSourceDoc(tc.docName, tc.spec)))
			e := errorAt(t, errs, tc.code)
			if e.Field != tc.field {
				t.Errorf("field = %q, want %q (%v)", e.Field, tc.field, errs)
			}
		})
	}
}

// TestGitSourceCarriesNoCredential is ADR-0035 decision 2's "dumber than a
// connection" as a test: a source is data, and the credential it is read with is
// a name pointing at a GitConnection.
func TestGitSourceCarriesNoCredential(t *testing.T) {
	for _, path := range specFieldPaths(reflect.TypeOf(GitSource{})) {
		for _, forbidden := range []string{"auth", "token", "secret", "key", "password"} {
			if strings.Contains(strings.ToLower(path), forbidden) {
				t.Errorf("%s looks like credential material; a GitSource carries none (ADR-0035 decision 2)", path)
			}
		}
	}
}

// TestGitSourceResolvesAsAGlobal is the seam slice 2 uses: list the GitSources,
// convert, hand them to Resolve.
func TestGitSourceResolvesAsAGlobal(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(gitSourceDoc("build-tools",
		"  git: https://github.com/acme/build-tools\n  ref: v2\n")))
	if len(errs) != 0 {
		t.Fatalf("decoding the source: %v", errs)
	}
	global := docs[0].(*GitSource)

	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout}
  components:
    - {name: web, port: 8080}
    - {name: tools, source: build-tools}
`, anyEnvironment)
	r, errs := Resolve(p, e, global.AsSource())
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if bound := r.SourceFor("tools"); bound == nil || bound.Git != global.Spec.Git || bound.Ref != "v2" {
		t.Fatalf("tools bound to %+v, want the instance's build-tools source", bound)
	}
	if bound := r.SourceFor("web"); bound == nil || bound.Name != DefaultSourceName {
		t.Fatalf("web bound to %+v, want the project's own default", bound)
	}
}

// TestBuildStrategyNoneBindsNothingToBuild: with nothing built, a binding would
// promise a clone that never happens.
func TestBuildStrategyNoneBindsNothingToBuild(t *testing.T) {
	p, e := loadPair(t, `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  source: {git: https://github.com/acme/checkout}
  build: {strategy: none}
  image: ghcr.io/acme/checkout:1
  components:
    - {name: web, port: 8080}
`, anyEnvironment)
	r, errs := Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if r.Components[0].Image != "ghcr.io/acme/checkout:1" {
		t.Errorf("image = %q, want the pre-built reference", r.Components[0].Image)
	}
}
