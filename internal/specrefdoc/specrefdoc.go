// Package main contains the specrefdoc generator: it renders the committed
// JSON Schemas (schema/*.json) into the user-facing Markdown spec reference
// (docs/reference/*.md) for every kelson document kind.
//
// The reference is generated, not hand-maintained (issue #22): it is sourced
// from the same schema/ files the rest of the tooling validates against, so
// the page cannot drift from the model. The generator is invoked both by this
// command (`go generate ./internal/specrefdoc`) and by a golden test that
// fails if the committed Markdown ever differs from what it produces.
//
// Output is deterministic: field order is derived by sorting property names,
// never by ranging over a map as the source of ordering. The only map
// iteration here is stable because we range over a sorted slice of keys.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ref describes one generated page: the schema it is read from and the
// docs-relative path and title it is written to. The slice order is the
// stable authoring order of the pages.
var refs = []struct {
	src   string
	file  string
	title string
}{
	{"project.schema.json", "reference/project.md", "Project spec"},
	{"environment.schema.json", "reference/environment.md", "Environment spec"},
	{"gitconnection.schema.json", "reference/gitconnection.md", "GitConnection spec"},
	{"gitsource.schema.json", "reference/gitsource.md", "GitSource spec"},
}

// Generate reads every committed schema in schemaDir and returns the rendered
// Markdown for each page, keyed by docs-relative file path.
func Generate(schemaDir string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(refs))
	for _, r := range refs {
		data, err := os.ReadFile(filepath.Join(schemaDir, r.src))
		if err != nil {
			return nil, fmt.Errorf("specrefdoc: reading %s: %w", r.src, err)
		}
		var s schema
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("specrefdoc: parsing %s: %w", r.src, err)
		}
		out[r.file] = renderDocument(&s, r.title, r.src)
	}
	return out, nil
}

// WriteAll writes the generated pages under docsDir, creating directories as
// needed. It returns the list of written paths.
func WriteAll(docsDir string, pages map[string][]byte) ([]string, error) {
	var written []string
	for _, r := range refs {
		path := filepath.Join(docsDir, filepath.FromSlash(r.file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("specrefdoc: mkdir: %w", err)
		}
		if err := os.WriteFile(path, pages[r.file], 0o644); err != nil {
			return nil, fmt.Errorf("specrefdoc: writing %s: %w", path, err)
		}
		written = append(written, r.file)
	}
	return written, nil
}

// schema is the minimal JSON Schema subset the reference renderer consumes.
// It is a projection of schema/*.json, parsed with encoding/json and plain
// structs — no external schema dependency (depguard).
type schema struct {
	Type                 string             `json:"type"`
	Description          string             `json:"description"`
	Format               string             `json:"format"`
	Pattern              string             `json:"pattern"`
	Default              any                `json:"default"`
	Enum                 []any              `json:"enum"`
	MinLength            *int               `json:"minLength"`
	MaxLength            *int               `json:"maxLength"`
	Minimum              *float64           `json:"minimum"`
	Maximum              *float64           `json:"maximum"`
	MinItems             *int               `json:"minItems"`
	Items                *schema            `json:"items"`
	Required             []string           `json:"required"`
	Properties           map[string]*schema `json:"properties"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	OneOf                []*schema          `json:"oneOf"`
}

// renderDocument renders one top-level document (Project or Environment).
func renderDocument(root *schema, title, source string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/%s` and rewrites this page. -->\n\n", source)
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "This reference is **generated** from the committed JSON Schema "+
		"[`schema/%s`](https://github.com/dafrie/kelson/blob/main/schema/%s), so it cannot drift from the model. "+
		"Edit the Go types in `internal/model` and regenerate; never edit this page by hand. "+
		"See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.\n\n",
		source, source)
	writeObject(&b, root, "", 2)
	return b.Bytes()
}

// writeObject renders the heading and field table for one object schema, then
// recurses into nested objects / arrays of objects (fields are authorable and
// recurring). path is the dotted field path used in the heading; empty for the
// document root. level is the Markdown heading depth (2 = "##").
func writeObject(b *bytes.Buffer, s *schema, path string, level int) {
	if path != "" {
		heading := strings.Repeat("#", level)
		fmt.Fprintf(b, "\n%s `%s`\n\n", heading, path)
	}
	fmt.Fprintf(b, "| Field | Type | Required | Default | Description |\n")
	fmt.Fprintf(b, "|-------|------|----------|---------|-------------|\n")
	for _, name := range sortedKeys(s.Properties) {
		ps := s.Properties[name]
		req := "no"
		if contains(s.Required, name) {
			req = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n",
			name, typeString(ps, name), req, defaultString(ps.Default), inline(ps.Description))
	}
	for _, name := range sortedKeys(s.Properties) {
		ps := s.Properties[name]
		if child, childPath, ok := nestedObject(ps, path, name); ok {
			writeObject(b, child, childPath, level+1)
		}
	}
}

// nestedObject returns the object schema a property expands to, the heading
// path for it, and whether there is one. Arrays of objects expand to their
// item schema with a trailing "[]" on the path; maps whose values are objects
// do not expand (their shape is described inline in the type column).
//
// A union expands when exactly one of its arms is an object — a component's
// `source:`, whose other arm is a plain source name (ADR-0035). Its fields are
// as authorable as any other object's, and dropping them would leave the only
// fields in this reference with no description of their own. A union with two
// object arms does not expand: both arms would want the same heading, and the
// `one of: object {…}` naming in the type column is what distinguishes them.
func nestedObject(s *schema, parent, name string) (*schema, string, bool) {
	path := name
	if parent != "" {
		path = parent + "." + name
	}
	switch {
	case len(s.OneOf) > 0:
		if arm, ok := soleObjectArm(s.OneOf); ok {
			return arm, path, true
		}
		return nil, "", false
	case s.Type == "array" && s.Items != nil && len(s.Items.Properties) > 0:
		return s.Items, path + "[]", true
	case s.Type == "object" && len(s.Properties) > 0 && !isMap(s):
		return s, path, true
	default:
		return nil, "", false
	}
}

// soleObjectArm returns the union's one object arm, if it has exactly one.
func soleObjectArm(arms []*schema) (*schema, bool) {
	var found *schema
	for _, arm := range arms {
		if len(arm.Properties) == 0 {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = arm
	}
	return found, found != nil
}

// typeString renders the type column for a field: JSON type, enum values,
// element type for arrays and maps, and the schema constraints that matter to
// an author (length/range bounds, pattern, format).
func typeString(s *schema, _ string) string {
	if len(s.OneOf) > 0 {
		one := make([]string, 0, len(s.OneOf))
		for _, o := range s.OneOf {
			one = append(one, oneOfArm(o))
		}
		return "one of: " + strings.Join(one, ", ")
	}
	switch {
	case s.Type == "array":
		base := "array of " + typeString(s.Items, "")
		if c := constraints(s); c != "" {
			base += " · " + c
		}
		return base
	case s.Type == "object" && isMap(s):
		return "map of " + typeString(mapValue(s), "")
	}
	parts := []string{}
	if s.Type != "" {
		parts = append(parts, s.Type)
	}
	if len(s.Enum) > 0 {
		parts = append(parts, "enum "+joinEnum(s.Enum))
	}
	if c := constraints(s); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, " ")
}

// oneOfArm names one arm of a union. An object arm is named by its keys —
// `object {secret, key}` — because a union of two object arms otherwise reads
// as "object, object", which tells an author nothing about which one they are
// choosing between. Its required keys are what distinguishes an environment
// value's two mapping forms; an arm that requires nothing is named by every key
// it has, which is what a chart source is (exactly one of `repository`, `oci`,
// so neither is required and both are the point).
func oneOfArm(s *schema) string {
	if s.Type != "object" {
		return typeString(s, "")
	}
	keys := s.Required
	if len(keys) == 0 {
		keys = sortedKeys(s.Properties)
	}
	if len(keys) == 0 {
		return typeString(s, "")
	}
	return "object {" + strings.Join(keys, ", ") + "}"
}

// constraints renders length/range/pattern/format bounds, if any.
func constraints(s *schema) string {
	var c []string
	if s.Format != "" {
		c = append(c, "format "+s.Format)
	}
	if s.MinLength != nil {
		c = append(c, fmt.Sprintf("min length %d", *s.MinLength))
	}
	if s.MaxLength != nil {
		c = append(c, fmt.Sprintf("max length %d", *s.MaxLength))
	}
	if s.Minimum != nil {
		c = append(c, fmt.Sprintf("min %s", num(*s.Minimum)))
	}
	if s.Maximum != nil {
		c = append(c, fmt.Sprintf("max %s", num(*s.Maximum)))
	}
	if s.MinItems != nil {
		c = append(c, fmt.Sprintf("min %d item(s)", *s.MinItems))
	}
	if s.Pattern != "" {
		c = append(c, "pattern `"+s.Pattern+"`")
	}
	return strings.Join(c, ", ")
}

// isMap reports whether the object has an object-typed additionalProperties
// rather than a named property set — i.e. it is a string-keyed map.
func isMap(s *schema) bool {
	if len(s.AdditionalProperties) == 0 {
		return false
	}
	trimmed := bytes.TrimSpace(s.AdditionalProperties)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// mapValue returns the additionalProperties schema for a map-typed object.
func mapValue(s *schema) *schema {
	var v schema
	if err := json.Unmarshal(s.AdditionalProperties, &v); err != nil {
		return &schema{}
	}
	return &v
}

// joinEnum formats enum values as a comma-separated code list.
func joinEnum(enum []any) string {
	out := make([]string, 0, len(enum))
	for _, v := range enum {
		b, err := json.Marshal(v)
		if err == nil {
			out = append(out, "`"+string(b)+"`")
		}
	}
	return strings.Join(out, ", ")
}

// defaultString formats a schema default as inline code, or "" when absent.
func defaultString(d any) string {
	if d == nil {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil || string(b) == "null" {
		return ""
	}
	return "`" + string(b) + "`"
}

// num renders a float without trailing ".000000".
func num(f float64) string {
	return fmt.Sprintf("%g", f)
}

// inline collapses newlines in a description so table rows stay well-formed.
func inline(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

// sortedKeys returns the property names of m in lexicographic order. All
// rendered field order flows through here so output is byte-identical across
// runs and independent of Go's map iteration order.
func sortedKeys(m map[string]*schema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
