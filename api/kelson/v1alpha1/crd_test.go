package v1alpha1

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The status half of deploy/crds/*.yaml is hand-written in
// internal/schemagen/status.go, because the status types live here and this
// package imports apimachinery — which the generator's depguard fence does not
// allow it to import back (see that file's comment for the asymmetry).
//
// This is the other side of that trade. Every property the CRD declares under
// `status` must be a json tag of the Go struct, and every json tag must be a
// declared property, recursively. A field added to EnvironmentStatus without a
// line in the emitter fails here, in the package that owns the type, which is
// where someone adding the field is already looking.
//
// The `spec` half needs no such test: it is reflected from internal/model, and
// internal/schemagen's own drift test compares it against schema/*.json.

const crdDir = "../../../deploy/crds"

func TestCRDStatusMatchesGoTypes(t *testing.T) {
	cases := []struct {
		file string
		kind string
		typ  reflect.Type
	}{
		{"kelson.dev_projects.yaml", "Project", reflect.TypeOf(ProjectStatus{})},
		{"kelson.dev_environments.yaml", "Environment", reflect.TypeOf(EnvironmentStatus{})},
		{"kelson.dev_gitconnections.yaml", "GitConnection", reflect.TypeOf(GitConnectionStatus{})},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			crd := readCRD(t, tc.file)
			if got := crd.Spec.Names.Kind; got != tc.kind {
				t.Fatalf("CRD declares kind %q, want %q", got, tc.kind)
			}
			if got := crd.Spec.Group; got != Group {
				t.Errorf("CRD declares group %q, want %q", got, Group)
			}
			if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != Version {
				t.Fatalf("CRD must serve exactly version %q", Version)
			}
			status, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
			if !ok {
				t.Fatalf("the CRD schema has no status property")
			}
			compare(t, tc.typ, status, tc.typ.Name())
		})
	}
}

// TestCRDHistoryBoundMatchesConstant pins the one number that appears in both
// places: the schema's maxItems and MaxHistoryEntries. A schema that allowed
// more than the controller writes would be a bound nothing enforces; a schema
// that allowed fewer would make a legal status unwritable.
func TestCRDHistoryBoundMatchesConstant(t *testing.T) {
	crd := readCRD(t, "kelson.dev_environments.yaml")
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	history, ok := status.Properties["history"]
	if !ok {
		t.Fatalf("status has no history property")
	}
	if history.MaxItems == nil {
		t.Fatalf("status.history has no maxItems; it must be bounded (ADR-0028 decision 4)")
	}
	if *history.MaxItems != MaxHistoryEntries {
		t.Errorf("status.history maxItems is %d, want MaxHistoryEntries (%d)",
			*history.MaxItems, MaxHistoryEntries)
	}
}

// compare walks a Go type and a schema node together.
func compare(t *testing.T, rt reflect.Type, node schemaNode, path string) {
	t.Helper()
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	switch {
	case rt.Kind() == reflect.Slice:
		if node.Items == nil {
			t.Errorf("%s is a slice in Go but the schema declares no items", path)
			return
		}
		compare(t, rt.Elem(), *node.Items, path+"[]")
		return
	case rt.Kind() != reflect.Struct:
		return
	}
	// metav1.Time and friends marshal as a scalar; there is nothing to compare.
	if node.Type != "object" {
		return
	}

	want := jsonTags(rt)
	got := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("%s: the CRD schema and the Go struct disagree about the fields.\n"+
			"  Go (%s): %s\n  CRD:        %s\n"+
			"Fix internal/schemagen/status.go, then `go generate ./internal/model`.",
			path, rt.String(), strings.Join(want, ","), strings.Join(got, ","))
		return
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		name := tagName(f)
		if name == "" {
			continue
		}
		compare(t, f.Type, node.Properties[name], path+"."+name)
	}
}

func jsonTags(rt reflect.Type) []string {
	var names []string
	for i := range rt.NumField() {
		if name := tagName(rt.Field(i)); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func tagName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

// schemaNode is the subset of an openAPIV3Schema this test reads. It is
// declared here rather than imported from k8s.io/apiextensions-apiserver
// because the whole of what is needed is four fields, and the fence for this
// plane deliberately does not include the apiextensions types.
type schemaNode struct {
	Type       string                `json:"type"`
	Properties map[string]schemaNode `json:"properties"`
	Items      *schemaNode           `json:"items"`
	MaxItems   *int                  `json:"maxItems"`
}

type crdFile struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		Versions []struct {
			Name   string `json:"name"`
			Schema struct {
				OpenAPIV3Schema schemaNode `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

func readCRD(t *testing.T, name string) crdFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(crdDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v (generate it with `go generate ./internal/model`)", name, err)
	}
	var crd crdFile
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return crd
}
