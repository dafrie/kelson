package model

import (
	"os"
	"path/filepath"
	"slices"
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

// TestThreeEnvironmentsDeliveryModes is the #25 acceptance: one Project
// renders correctly into three environments with three delivery modes.
func TestThreeEnvironmentsDeliveryModes(t *testing.T) {
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
	wantModes := map[string]DeliveryMode{
		"development": DeliveryDirect,
		"staging":     DeliveryFlux,
		"production":  DeliveryArgoCD,
	}
	wantPresets := map[string]ServicePreset{
		"development": PresetShared,
		"staging":     PresetSmall,
		"production":  PresetHAMedium,
	}
	for name, mode := range wantModes {
		env, ok := envs[name]
		if !ok {
			t.Fatalf("environment %q missing from example", name)
		}
		r, errs := Resolve(project, env)
		if len(errs) > 0 {
			t.Fatalf("%s: %v", name, errs)
		}
		if r.Environment.Mode != mode {
			t.Errorf("%s mode = %q, want %q", name, r.Environment.Mode, mode)
		}
		if r.Services[0].Preset != wantPresets[name] {
			t.Errorf("%s db preset = %q, want %q", name, r.Services[0].Preset, wantPresets[name])
		}
		if mode == DeliveryDirect && r.Environment.Delivery.Git != nil {
			t.Errorf("%s: direct mode must not carry a git target", name)
		}
	}
	// Agents are free in development, dry-run-bound in staging, PR-only in prod.
	devAgent, _ := Resolve(project, envs["development"])
	prodAgent, _ := Resolve(project, envs["production"])
	if devAgent.Environment.Policy.Agents != AgentsAllow {
		t.Errorf("development agents = %q, want allow", devAgent.Environment.Policy.Agents)
	}
	if prodAgent.Environment.Policy.Agents != AgentsProposeOnly {
		t.Errorf("production agents = %q, want propose-only", prodAgent.Environment.Policy.Agents)
	}
	stagingAgent, _ := Resolve(project, envs["staging"])
	if !slices.Contains(stagingAgent.Environment.Policy.Require, "dry-run") {
		t.Errorf("staging must require dry-run, got %v", stagingAgent.Environment.Policy.Require)
	}
}
