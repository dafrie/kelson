package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesAreValid pins every example under examples/ to zero validation
// errors: the published examples are the schema's first users and must never
// drift out of validity. Overlay payloads (raw k8s manifests) live under
// k8s/ and are deliberately not kelson documents.
func TestExamplesAreValid(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read examples/: %v", err)
	}
	exampleCount := 0
	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		exampleCount++
		dirPath := filepath.Join(root, dir.Name())
		files, err := os.ReadDir(dirPath)
		if err != nil {
			t.Fatalf("read %s: %v", dirPath, err)
		}
		var project *Project
		var envs []*Environment
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dirPath, f.Name()))
			if err != nil {
				t.Fatalf("read %s/%s: %v", dirPath, f.Name(), err)
			}
			docs, docErrs := DecodeDocuments(data)
			for _, e := range docErrs {
				t.Errorf("%s/%s: %s", dirPath, f.Name(), e)
			}
			for _, d := range docs {
				switch d := d.(type) {
				case *Project:
					if project != nil {
						t.Errorf("%s: two Project documents", dirPath)
					}
					project = d
				case *Environment:
					envs = append(envs, d)
				}
			}
		}

		if project == nil {
			t.Errorf("%s: no project.yaml", dirPath)
			continue
		}
		for _, env := range envs {
			for _, e := range ValidateEnvironment(env, project) {
				t.Errorf("%s/%s: %s", dirPath, env.Metadata.Name, e)
			}
			if _, errs := Resolve(project, env); len(errs) > 0 {
				t.Errorf("%s: resolve(%s) failed: %v", dirPath, env.Metadata.Name, errs)
			}
		}
	}
	if exampleCount < 5 {
		t.Fatalf("want at least 5 examples, found %d", exampleCount)
	}
}

// TestThreeEnvironmentsResolve is the #25 acceptance: one Project resolves
// into three environments that differ where an Environment is meant to differ.
//
// It used to assert three *delivery modes*, which was the shape of the
// acceptance under ADR-0001's hybrid delivery. There is one delivery path now
// (ADR-0028) and the example says so; what is left to check is that the same
// Project meets three documents and takes each one's namespace and routing.
//
// It used to assert service presets and agent policy from the same example.
// Both are gated until M9 and M7 (issue #141), so the example no longer
// declares them and this test no longer reads them; the precedence rules they
// covered (P4 for policy, P5 for presets) are exercised against the resolver
// directly in resolve_test.go, which is where they belong now that a spec
// carrying them does not validate.
func TestThreeEnvironmentsResolve(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "three-environments")
	var project *Project
	envs := map[string]*Environment{}
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		docs, docErrs := DecodeDocuments(data)
		if len(docErrs) > 0 {
			t.Fatalf("%s: %v", f.Name(), docErrs)
		}
		for _, d := range docs {
			switch d := d.(type) {
			case *Project:
				project = d
			case *Environment:
				envs[d.Metadata.Name] = d
			}
		}
	}
	wantNamespaces := map[string]string{
		// development names no namespace, so it takes the model's
		// <project>-<environment> default; the other two name their own.
		"development": "billing-development",
		"staging":     "billing-staging",
		"production":  "billing-prod",
	}
	for name, ns := range wantNamespaces {
		env, ok := envs[name]
		if !ok {
			t.Fatalf("environment %q missing from example", name)
		}
		r, errs := Resolve(project, env)
		if len(errs) > 0 {
			t.Fatalf("%s: %v", name, errs)
		}
		if r.Environment.Namespace != ns {
			t.Errorf("%s namespace = %q, want %q", name, r.Environment.Namespace, ns)
		}
		if r.Environment.Routing.DomainSuffix == "" {
			t.Errorf("%s: the environment's own domain suffix did not survive resolution", name)
		}
	}
}
