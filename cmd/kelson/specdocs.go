package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The façade-backed verbs (deploy, rollback, history) address a spec with a
// SpecRef: either a project already stored on kelson-server, or the CLI's own
// `-f` documents carried inline (proto/kelson/v1alpha1/render.proto). This
// file is the inline half — splitting a command's `-f` files into the shape
// SpecDocuments wants — and the shared "which one of -f/--project did the
// caller mean" resolution every one of those verbs needs identically.

// docStub is the minimal shape read from an authored document to route it:
// which kind it is, and — for an Environment — the name that keys it in
// SpecDocuments.environments. internal/controlstore requires that key and the
// document's own metadata.name to agree (internal/controlstore/spec.go,
// resources()), so this is not a label of convenience.
type docStub struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

// loadInlineDocuments reads every `-f` file and splits it into the inline
// SpecDocuments shape the wire wants: one Project document and one document
// per Environment, keyed by its own name.
//
// It re-encodes each document from the yaml.Node it decoded rather than
// round-tripping through model.Project/model.Environment, which is the
// closest this side of the wire gets to byte-faithful: a file that mixes a
// Project and an Environment behind one "---" (a supported, common shape for
// every other command's `-f`) splits cleanly into the two documents the store
// actually wants — internal/controlstore's resources() decodes each blob
// expecting exactly one kind — and whatever the model type does not carry,
// comments included, survives, because nothing here decodes as far as the
// model. The server does not promise byte fidelity for what it stores either
// way (docs/server.md), so this is the best fidelity available between here
// and there, not a stronger claim than the store already makes.
func loadInlineDocuments(files []string) (*kelsonv1alpha1.SpecDocuments, error) {
	docs := &kelsonv1alpha1.SpecDocuments{Environments: map[string][]byte{}}
	var projectFile string
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if err := splitInlineDocument(docs, f, data, &projectFile); err != nil {
			return nil, err
		}
	}
	if docs.Project == nil {
		return nil, fmt.Errorf("no Project document found in %s", strings.Join(files, ", "))
	}
	if len(docs.Environments) == 0 {
		return nil, fmt.Errorf("no Environment document found in %s", strings.Join(files, ", "))
	}
	return docs, nil
}

func splitInlineDocument(docs *kelsonv1alpha1.SpecDocuments, file string, data []byte, projectFile *string) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing %s: %w", file, err)
		}
		if node.Kind == 0 {
			continue // an empty document in the stream
		}
		var stub docStub
		if err := node.Decode(&stub); err != nil {
			return fmt.Errorf("parsing %s: %w", file, err)
		}
		switch stub.Kind {
		case "Project":
			if docs.Project != nil {
				return fmt.Errorf("multiple Project documents supplied (%s and %s); this command takes exactly one project", *projectFile, file)
			}
			body, err := encodeDocument(&node)
			if err != nil {
				return fmt.Errorf("re-encoding the Project document from %s: %w", file, err)
			}
			docs.Project = body
			*projectFile = file
		case "Environment":
			if stub.Metadata.Name == "" {
				return fmt.Errorf("%s declares an Environment document with no metadata.name", file)
			}
			body, err := encodeDocument(&node)
			if err != nil {
				return fmt.Errorf("re-encoding the Environment document from %s: %w", file, err)
			}
			docs.Environments[stub.Metadata.Name] = body
		}
	}
}

// encodeDocument re-serializes one decoded document node back to YAML bytes.
func encodeDocument(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// selectDocEnvironment picks the environment name a command's inline
// documents and --env flag resolve to, under the rule every `-f` command
// applies: an unnamed request is unambiguous only when the documents hold
// exactly one Environment.
func selectDocEnvironment(docs *kelsonv1alpha1.SpecDocuments, name string) (string, error) {
	environments := docs.GetEnvironments()
	names := make([]string, 0, len(environments))
	for n := range environments {
		names = append(names, n)
	}
	sort.Strings(names)

	if name != "" {
		if _, ok := environments[name]; ok {
			return name, nil
		}
		return "", fmt.Errorf("environment %q not found in the supplied files (available: %s)", name, strings.Join(names, ", "))
	}
	if len(names) != 1 {
		return "", fmt.Errorf("the supplied files declare %d environment(s) (%s) and none was named; pass --env", len(names), strings.Join(names, ", "))
	}
	return names[0], nil
}

// resolveSpecRef builds the SpecRef and resolves the target environment for
// one of deploy/rollback/history, from the two ways a command may address a
// spec: `-f` files (inline) or `--project` (a spec kelson-server already
// holds). They are mutually exclusive because the request has exactly one
// `spec` oneof to put either shape into.
//
// The stored case cannot resolve an omitted --env the way the inline case
// does — reading a stored project's environment names is another RPC this
// helper deliberately does not make on the caller's behalf — so --env is
// required whenever --project is.
func resolveSpecRef(files []string, project, env string) (*kelsonv1alpha1.SpecRef, string, error) {
	switch {
	case len(files) > 0 && project != "":
		return nil, "", errors.New("--file and --project are mutually exclusive: address either inline documents or a spec kelson-server already holds")
	case len(files) > 0:
		docs, err := loadInlineDocuments(files)
		if err != nil {
			return nil, "", err
		}
		env, err = selectDocEnvironment(docs, env)
		if err != nil {
			return nil, "", err
		}
		return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{Documents: docs}}, env, nil
	case project != "":
		if env == "" {
			return nil, "", errors.New("--env is required with --project")
		}
		return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: project}}, env, nil
	default:
		return nil, "", errors.New("pass -f <files> to address inline documents, or --project <name> for a spec kelson-server already holds")
	}
}

// inlineProfileRef maps the CLI's familiar --profile flag onto ProfileRef:
// unset is the zero profile (matching every offline command), from-cluster
// asks kelson-server to capture ITS OWN cluster connection (these RPCs render
// server-side now, so there is no CLI-side kubeconfig in this path at all),
// and anything else is a file to read and send verbatim.
func inlineProfileRef(flag string) (*kelsonv1alpha1.ProfileRef, error) {
	switch flag {
	case "":
		return nil, nil
	case "from-cluster":
		return &kelsonv1alpha1.ProfileRef{Profile: &kelsonv1alpha1.ProfileRef_FromCluster{FromCluster: true}}, nil
	default:
		data, err := os.ReadFile(filepath.Clean(flag))
		if err != nil {
			return nil, fmt.Errorf("reading cluster profile %s: %w", flag, err)
		}
		return &kelsonv1alpha1.ProfileRef{Profile: &kelsonv1alpha1.ProfileRef_Yaml{Yaml: data}}, nil
	}
}
