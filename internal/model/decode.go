package model

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Position is a 1-based line/column in the YAML source.
type Position struct {
	Line   int
	Column int
}

// positions maps JSONPath-like field paths to source positions. Keys point
// at the mapping key node (e.g. the `port:` key in `port: 70000`), falling
// back to the value node for sequence entries.
type positions map[string]Position

func (p positions) at(path string) Position {
	if pos, ok := p[path]; ok {
		return pos
	}
	// Fall back toward the parent path, so errors on computed subfields of a
	// scalar still land somewhere useful.
	for i := len(path); i > 0; i-- {
		if path[i-1] == '.' || path[i-1] == '[' {
			if pos, ok := p[path[:i-1]]; ok {
				return pos
			}
		}
	}
	return Position{}
}

func indexPositions(n *yaml.Node, path string, out positions) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			indexPositions(c, path, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			child := path + "." + key.Value
			if _, exists := out[child]; !exists {
				out[child] = Position{Line: key.Line, Column: key.Column}
			}
			indexPositions(val, child, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			indexPositions(c, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// DecodeDocuments parses a YAML stream (one or more documents) and validates
// each against its declared kind. JSON input works unchanged (JSON is a YAML
// subset). Cross-document rules (Environment → Project references) are checked
// separately with ValidateSet or ValidateEnvironment.
func DecodeDocuments(data []byte) ([]any, Errors) {
	var docs []any
	var errs Errors
	dec := yaml.NewDecoder(bytes.NewReader(data))
	docIdx := -1
	for {
		var raw yaml.Node
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		docIdx++
		if err != nil {
			errs = append(errs, Error{
				Code:        ErrInvalidFormat,
				Resource:    "document",
				Field:       "$",
				Message:     fmt.Sprintf("YAML syntax error: %v", err),
				Remediation: "fix the YAML syntax; line numbers in the message refer to the source",
				DocsURL:     docsURL(ErrInvalidFormat),
			})
			break
		}
		if raw.Kind == 0 || (len(raw.Content) > 0 && raw.Content[0].Kind == yaml.ScalarNode && raw.Content[0].Tag == "!!null") {
			continue // empty document in the stream
		}
		doc, derr := decodeDocument(&raw, docIdx)
		errs = append(errs, derr...)
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, errs
}

func decodeDocument(raw *yaml.Node, docIdx int) (any, Errors) {
	pos := make(positions)
	indexPositions(raw, "$", pos)

	var tm TypeMeta
	if err := raw.Decode(&tm); err != nil {
		return nil, typeErrors(err, "document", pos)
	}
	var errs Errors
	v := validator{resource: fmt.Sprintf("%s/%s", tm.Kind, "?"), pos: pos}

	if tm.APIVersion != APIVersion {
		v.err(ErrInvalidFormat, "$.apiVersion",
			fmt.Sprintf("unsupported apiVersion %q", tm.APIVersion),
			fmt.Sprintf("set apiVersion to %q", APIVersion))
	}

	switch tm.Kind {
	case KindProject, KindEnvironment, KindGitConnection, KindGitSource:
	case "":
		v.err(ErrMissingRequired, "$.kind", "kind is required",
			"set kind to one of: "+strings.Join(Kinds, ", "))
	default:
		v.err(ErrInvalidEnum, "$.kind",
			fmt.Sprintf("unknown kind %q", tm.Kind),
			"valid kinds: "+strings.Join(Kinds, ", "))
	}
	if len(v.errs) > 0 {
		return nil, v.errs
	}

	var doc any
	switch tm.Kind {
	case KindProject:
		p := new(Project)
		doc = p
	case KindEnvironment:
		e := new(Environment)
		doc = e
	case KindGitConnection:
		g := new(GitConnection)
		doc = g
	case KindGitSource:
		g := new(GitSource)
		doc = g
	}

	resource := docResource(doc)
	if err := raw.Decode(doc); err != nil {
		errs = append(errs, typeErrors(err, resource, pos)...)
	}
	unknownFieldErrorsWithResource(raw, doc, resource, pos, &errs)
	switch d := doc.(type) {
	case *Project:
		vp := validator{resource: resource, kind: KindProject, pos: pos}
		validateProject(d, &vp)
		errs = append(errs, vp.errs...)
	case *Environment:
		ve := validator{resource: resource, kind: KindEnvironment, pos: pos}
		validateEnvironmentShape(d, &ve)
		errs = append(errs, ve.errs...)
	case *GitConnection:
		vg := validator{resource: resource, kind: KindGitConnection, pos: pos}
		validateGitConnection(d, &vg)
		errs = append(errs, vg.errs...)
	case *GitSource:
		vs := validator{resource: resource, kind: KindGitSource, pos: pos}
		validateGitSource(d, &vs)
		errs = append(errs, vs.errs...)
	}
	return doc, errs
}

func docResource(doc any) string {
	switch d := doc.(type) {
	case *Project:
		return fmt.Sprintf("%s/%s", KindProject, d.Metadata.Name)
	case *Environment:
		return fmt.Sprintf("%s/%s", KindEnvironment, d.Metadata.Name)
	case *GitConnection:
		return fmt.Sprintf("%s/%s", KindGitConnection, d.Metadata.Name)
	case *GitSource:
		return fmt.Sprintf("%s/%s", KindGitSource, d.Metadata.Name)
	}
	return "document"
}

var yamlLineRE = regexp.MustCompile(`^line (\d+): (.*)$`)

// typeErrors converts yaml.TypeError entries (e.g. "line 12: cannot unmarshal
// !!str `abc` into int") into structured Errors, recovering the field path
// from the position index.
func typeErrors(err error, resource string, pos positions) Errors {
	te, ok := err.(*yaml.TypeError)
	if !ok {
		return Errors{{
			Code:        ErrInvalidFormat,
			Resource:    resource,
			Field:       "$",
			Message:     err.Error(),
			Remediation: "fix the value so it matches the schema type",
			DocsURL:     docsURL(ErrInvalidFormat),
		}}
	}
	var errs Errors
	for _, msg := range te.Errors {
		e := Error{
			Code:        ErrInvalidFormat,
			Resource:    resource,
			Field:       "$",
			Message:     msg,
			Remediation: "fix the value so it matches the schema type",
			DocsURL:     docsURL(ErrInvalidFormat),
		}
		if m := yamlLineRE.FindStringSubmatch(msg); m != nil {
			line, _ := strconv.Atoi(m[1])
			e.Line = line
			e.Message = m[2]
			if p := pathAtLine(pos, line); p != "" {
				e.Field = p
				e.Column = pos[e.Field].Column
			}
		}
		// A union value knows its own remediation and cannot return it: a
		// custom unmarshaller reports through a yaml.TypeError string or it
		// aborts the document. The prefix is the handshake (envvalue.go,
		// componentsource.go).
		switch {
		case strings.HasPrefix(e.Message, envValueShapePrefix):
			e.Message = strings.TrimPrefix(e.Message, envValueShapePrefix)
			e.Remediation = EnvValueRemediation
		case strings.HasPrefix(e.Message, componentSourceShapePrefix):
			e.Message = strings.TrimPrefix(e.Message, componentSourceShapePrefix)
			e.Remediation = ComponentSourceRemediation
		}
		errs = append(errs, e)
	}
	return errs
}

// pathAtLine finds the most specific indexed path whose key sits on the given
// source line, used to localize yaml type errors.
func pathAtLine(pos positions, line int) string {
	best := ""
	for path, p := range pos {
		if p.Line != line {
			continue
		}
		// Exact line matches only when the offending scalar is on the key's
		// own line; deeper-value subtrees live below the key's line.
		if best == "" || len(path) < len(best) {
			best = path
		}
	}
	return best
}

// unknownFieldErrorsWithResource walks the YAML node tree against the target
// struct's yaml tags and reports any mapping key the schema does not define.
// This complements type checking: keys a human mistyped (e.g. `replica:`) are
// errors, not silently ignored.
func unknownFieldErrorsWithResource(n *yaml.Node, target any, resource string, pos positions, errs *Errors) {
	walkUnknown(n, reflect.TypeOf(target).Elem(), "$", resource, pos, errs)
}

func walkUnknown(n *yaml.Node, t reflect.Type, path, resource string, pos positions, errs *Errors) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		walkUnknown(n.Content[0], t, path, resource, pos, errs)
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t {
	case reflect.TypeOf(EnvValue{}):
		walkEnvValue(n, path, resource, pos, errs)
		return
	case reflect.TypeOf(ComponentSource{}):
		// A scalar is a source name and has no keys to check; a mapping is a
		// chart source, whose keys are ChartSource's. Walking the union's own
		// struct instead would report `repository` as an unknown field, because
		// the union carries the arms and not their spelling
		// (componentsource.go).
		walkUnknown(n, reflect.TypeOf(ChartSource{}), path, resource, pos, errs)
		return
	case reflect.TypeOf(Resources{}):
		walkStructNode(n, t, path, resource, pos, errs)
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		walkStructNode(n, t, path, resource, pos, errs)
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return
		}
		et := t.Elem()
		for i := 0; i+1 < len(n.Content); i += 2 {
			walkUnknown(n.Content[i+1], et, path+"."+n.Content[i].Value, resource, pos, errs)
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			return
		}
		for i, c := range n.Content {
			walkUnknown(c, t.Elem(), fmt.Sprintf("%s[%d]", path, i), resource, pos, errs)
		}
	}
}

func walkStructNode(n *yaml.Node, t reflect.Type, path, resource string, pos positions, errs *Errors) {
	if n.Kind != yaml.MappingNode {
		return
	}
	fields := yamlFields(t)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		child := path + "." + key.Value
		sf, ok := fields[key.Value]
		if !ok {
			p := pos.at(child)
			*errs = append(*errs, Error{
				Code:        ErrUnknownField,
				Resource:    resource,
				Field:       child,
				Message:     fmt.Sprintf("unknown field %q on %s", key.Value, t.Name()),
				Remediation: fmt.Sprintf("check the field name against the %s schema; valid fields: %s", t.Name(), strings.Join(fieldNames(fields), ", ")),
				DocsURL:     docsURL(ErrUnknownField),
				Line:        p.Line,
				Column:      p.Column,
			})
			continue
		}
		walkUnknown(val, sf.Type, child, resource, pos, errs)
	}
}

// walkEnvValue rejects every key an environment mapping does not define. The
// three top-level keys are `from` (a service binding), and `secret`/`key` (a
// secret reference, written flat because `secret:` carries the name itself).
// Which combinations are legal is the unmarshaller's judgement, not this
// walk's: here a key is either part of the vocabulary or it is not.
func walkEnvValue(n *yaml.Node, path, resource string, pos positions, errs *Errors) {
	if n.Kind != yaml.MappingNode {
		return
	}
	topLevel := map[string]bool{"from": true, "secret": true, "key": true}
	binding := map[string]bool{"service": true, "key": true}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if !topLevel[key.Value] {
			p := pos.at(path + "." + key.Value)
			*errs = append(*errs, Error{
				Code:        ErrUnknownField,
				Resource:    resource,
				Field:       path + "." + key.Value,
				Message:     fmt.Sprintf("unknown field %q in environment value", key.Value),
				Remediation: EnvValueRemediation,
				DocsURL:     docsURL(ErrUnknownField),
				Line:        p.Line,
				Column:      p.Column,
			})
			continue
		}
		if key.Value != "from" {
			continue
		}
		if fm := n.Content[i+1]; fm.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(fm.Content); j += 2 {
				k := fm.Content[j]
				if !binding[k.Value] {
					p := pos.at(path + ".from." + k.Value)
					*errs = append(*errs, Error{
						Code:        ErrUnknownField,
						Resource:    resource,
						Field:       path + ".from." + k.Value,
						Message:     fmt.Sprintf("unknown field %q in service binding", k.Value),
						Remediation: "a service binding has exactly two fields: service, key",
						DocsURL:     docsURL(ErrUnknownField),
						Line:        p.Line,
						Column:      p.Column,
					})
				}
			}
		}
	}
}

type structField struct {
	Name string
	Type reflect.Type
}

func yamlFields(t reflect.Type) map[string]structField {
	out := map[string]structField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		inline := strings.Contains(tag, "inline")
		if inline && f.Anonymous {
			for n, sf := range yamlFields(f.Type) {
				out[n] = sf
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = structField{Name: f.Name, Type: f.Type}
	}
	return out
}

func fieldNames(fields map[string]structField) []string {
	out := make([]string, 0, len(fields))
	for n := range fields {
		out = append(out, n)
	}
	return out
}
