package promote

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pin writes image as component's per-environment image pin in the Environment
// document doc, and returns the edited bytes.
//
// environment names the document to edit when doc is a stream carrying several
// (a -f file may hold the Project and its Environments together); an empty name
// requires the stream to hold exactly one Environment.
//
// # Byte fidelity is by construction
//
// Everything the edit does not write is returned unchanged, byte for byte,
// because the edit is a splice: the YAML node tree is used only to locate a
// position, and the bytes at that position are replaced or inserted. A
// re-serialization would be simpler to write and would silently lose blank
// lines, flow-mapping spacing and trailing-comment columns — a yaml.v3 Node
// round-trip of examples/checkout-multi/production.yaml changes eleven lines
// without changing a single value. The spec is the user's document
// (ADR-0013 §1); a promotion may write one line of it and nothing else.
//
// Four shapes are handled, in the order they are met:
//
//  1. the component override already carries `image:` — the scalar is replaced
//     in place, keeping any trailing comment;
//  2. the override exists without one — a new `image:` line is inserted under
//     it, at the override's own indentation;
//  3. `spec.components` exists without this component — a new override is
//     appended to the sequence;
//  4. `spec.components` is absent — a minimal one is appended to `spec`.
//
// A document whose shape none of those fit — a flow-style components list, a
// `components:` key that is not a sequence — is refused with
// promote/document-unwritable rather than rewritten.
func Pin(doc []byte, environment, component, image string) ([]byte, error) {
	if component == "" {
		return nil, fmt.Errorf("promote: a component name is required to write a pin")
	}
	scalar, err := scalarText(image)
	if err != nil {
		return nil, err
	}

	root, err := environmentDocument(doc, environment)
	if err != nil {
		return nil, err
	}
	src := newSource(doc)

	spec := childValue(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return nil, unwritable(environment, "$.spec",
			"the Environment document has no spec mapping to write a pin into")
	}

	edited, err := pinInto(src, spec, environment, component, scalar)
	if err != nil {
		return nil, err
	}
	if err := verifyPin(edited, environment, component, image); err != nil {
		return nil, err
	}
	return edited, nil
}

// pinInto dispatches to the four shapes, innermost first.
func pinInto(src *source, spec *yaml.Node, environment, component, scalar string) ([]byte, error) {
	components := childValue(spec, "components")
	switch {
	case components == nil, isNull(components):
		return appendComponentList(src, spec, components, environment, component, scalar)
	case components.Kind != yaml.SequenceNode || components.Style&yaml.FlowStyle != 0:
		return nil, unwritable(environment, "$.spec.components",
			"spec.components is not a block sequence, so a pin cannot be spliced into it")
	}

	for _, item := range components.Content {
		if item.Kind != yaml.MappingNode || scalarAt(item, "name") != component {
			continue
		}
		if item.Style&yaml.FlowStyle != 0 {
			return nil, unwritable(environment, "$.spec.components["+component+"]",
				"this component override is written as a flow mapping, so a pin cannot be spliced into it")
		}
		if value := childValue(item, "image"); value != nil {
			return src.replaceScalar(value, scalar, environment, component)
		}
		return insertImageKey(src, item, environment, component, scalar)
	}
	return appendComponentOverride(src, components, environment, component, scalar)
}

// insertImageKey adds `image:` to an override that has none, on the line after
// the override's first key. Placing it there rather than at the end keeps the
// pin next to the name it belongs to, which is where a reader looks for it.
func insertImageKey(src *source, item *yaml.Node, environment, component, scalar string) ([]byte, error) {
	first := item.Content[0]
	// The key's own column, not the line's leading whitespace: an override's
	// first key shares its line with the sequence marker ("    - name: web"),
	// so the keys sit two columns further in than the line begins.
	indent := first.Column - 1
	if indent < 0 || src.indentOf(first.Line) < 0 {
		return nil, unwritable(environment, "$.spec.components["+component+"]",
			"the component override does not start on a line of its own")
	}
	end := src.blockEnd(first.Line, indent)
	return src.insertAfter(end, fmt.Sprintf("%simage: %s", strings.Repeat(" ", indent), scalar)), nil
}

// appendComponentOverride adds a whole override for a component the
// Environment does not mention, after the last entry of the existing list.
func appendComponentOverride(src *source, components *yaml.Node, environment, component, scalar string) ([]byte, error) {
	last := components.Content[len(components.Content)-1]
	marker := src.markerIndent(last.Line)
	if marker < 0 {
		return nil, unwritable(environment, "$.spec.components",
			"the last component override does not begin with a block sequence marker, so a new one cannot be appended")
	}
	end := src.blockEnd(last.Line, marker)
	pad := strings.Repeat(" ", marker)
	return src.insertAfter(end,
		fmt.Sprintf("%s- name: %s", pad, component),
		fmt.Sprintf("%s  image: %s", pad, scalar),
	), nil
}

// appendComponentList adds the whole `components:` block to a spec that has
// none, or fills in a `components:` key whose value is empty. The block is the
// minimum that carries the pin: one override, one name, one image.
func appendComponentList(src *source, spec, components *yaml.Node, environment, component, scalar string) ([]byte, error) {
	if components != nil {
		// `components:` with nothing under it. The key stays; the sequence is
		// written beneath it.
		key := keyNode(spec, "components")
		indent := src.indentOf(key.Line)
		if indent < 0 || !isNull(components) {
			return nil, unwritable(environment, "$.spec.components",
				"spec.components carries a value a pin cannot be spliced into")
		}
		pad := strings.Repeat(" ", indent+2)
		return src.insertAfter(src.blockEnd(key.Line, indent),
			fmt.Sprintf("%s- name: %s", pad, component),
			fmt.Sprintf("%s  image: %s", pad, scalar),
		), nil
	}

	first := spec.Content[0]
	indent := src.indentOf(first.Line)
	if indent < 0 {
		return nil, unwritable(environment, "$.spec",
			"the spec mapping does not start on a line of its own")
	}
	pad := strings.Repeat(" ", indent)
	end := src.blockEnd(first.Line, indent-1)
	return src.insertAfter(end,
		fmt.Sprintf("%scomponents:", pad),
		fmt.Sprintf("%s  - name: %s", pad, component),
		fmt.Sprintf("%s    image: %s", pad, scalar),
	), nil
}

/* ----------------------------------------------------------- the byte layer */

// source is the document's bytes indexed by line, the substrate every splice
// works on. Line numbers are yaml.Node's own: 1-based, over the whole stream.
type source struct {
	data  []byte
	start []int // byte offset of each line's first character
	eol   string
}

func newSource(data []byte) *source {
	s := &source{data: data, start: []int{0}, eol: "\n"}
	for i, b := range data {
		if b == '\n' && i+1 < len(data) {
			s.start = append(s.start, i+1)
		}
	}
	if bytes.Contains(data, []byte("\r\n")) {
		s.eol = "\r\n"
	}
	return s
}

func (s *source) lines() int { return len(s.start) }

// line returns one line's text without its terminator.
func (s *source) line(n int) string {
	if n < 1 || n > len(s.start) {
		return ""
	}
	begin := s.start[n-1]
	end := len(s.data)
	if n < len(s.start) {
		end = s.start[n]
	}
	return strings.TrimRight(string(s.data[begin:end]), "\r\n")
}

// indentOf is the number of leading spaces on a line, or -1 when the line is
// blank or a comment (neither can anchor an indentation).
func (s *source) indentOf(n int) int {
	text := s.line(n)
	trimmed := strings.TrimLeft(text, " ")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return -1
	}
	if strings.ContainsRune(text[:len(text)-len(trimmed)], '\t') {
		return -1
	}
	return len(text) - len(trimmed)
}

// markerIndent is the indentation of a block-sequence entry's "-", or -1 when
// the line does not start one.
func (s *source) markerIndent(n int) int {
	indent := s.indentOf(n)
	if indent < 0 {
		return -1
	}
	rest := s.line(n)[indent:]
	if rest != "-" && !strings.HasPrefix(rest, "- ") {
		return -1
	}
	return indent
}

// blockEnd returns the last line belonging to the block that starts at line
// start and whose own token sits at indentation indent.
//
// Deeper-indented lines belong to the block, which is what makes this work for
// block scalars, nested mappings and sequences alike without knowing which one
// it is looking at. Blank lines and comments are carried across but never
// extend the block on their own: a comment after the last value belongs to
// whatever comes next, and inserting above it keeps it there.
func (s *source) blockEnd(start, indent int) int {
	end := start
	for n := start + 1; n <= s.lines(); n++ {
		text := strings.TrimSpace(s.line(n))
		if text == "---" || text == "..." {
			break
		}
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if at := s.indentOf(n); at > indent {
			end = n
			continue
		}
		break
	}
	return end
}

// insertAfter splices whole lines in after line n.
func (s *source) insertAfter(n int, added ...string) []byte {
	at := len(s.data)
	if n < s.lines() {
		at = s.start[n]
	}
	var block bytes.Buffer
	for _, line := range added {
		block.WriteString(line)
		block.WriteString(s.eol)
	}

	out := make([]byte, 0, len(s.data)+block.Len()+len(s.eol))
	out = append(out, s.data[:at]...)
	// A final line with no terminator has to gain one before anything can
	// follow it; that newline is the only byte outside the pin this edit adds.
	if at == len(s.data) && at > 0 && !bytes.HasSuffix(s.data[:at], []byte("\n")) {
		out = append(out, s.eol...)
	}
	out = append(out, block.Bytes()...)
	out = append(out, s.data[at:]...)
	return out
}

// replaceScalar rewrites one scalar value in place, keeping whatever follows it
// on the line — the trailing comment and the whitespace that positions it.
func (s *source) replaceScalar(value *yaml.Node, scalar, environment, component string) ([]byte, error) {
	if value.Kind != yaml.ScalarNode || value.Style == yaml.LiteralStyle || value.Style == yaml.FoldedStyle {
		return nil, unwritable(environment, "$.spec.components["+component+"].image",
			"the existing image is not a single-line scalar, so it cannot be replaced in place")
	}
	if value.Line < 1 || value.Line > s.lines() {
		return nil, unwritable(environment, "$.spec.components["+component+"].image",
			"the existing image has no source position to replace")
	}
	text := s.line(value.Line)
	col := value.Column - 1
	if col < 0 || col > len(text) {
		return nil, unwritable(environment, "$.spec.components["+component+"].image",
			"the existing image's source position is outside its line")
	}

	rest := text[col:]
	cut := len(rest)
	if comment := value.LineComment; comment != "" {
		if i := strings.LastIndex(rest, comment); i >= 0 {
			cut = i
		}
	}
	tail := rest[len(strings.TrimRight(rest[:cut], " \t")):]

	begin := s.start[value.Line-1] + col
	end := s.start[value.Line-1] + len(text)
	out := make([]byte, 0, len(s.data)+len(scalar))
	out = append(out, s.data[:begin]...)
	out = append(out, scalar...)
	out = append(out, tail...)
	out = append(out, s.data[end:]...)
	return out, nil
}

/* ------------------------------------------------------------- YAML helpers */

// environmentDocument finds the Environment document to edit in a stream that
// may hold several, and returns its root mapping.
func environmentDocument(doc []byte, environment string) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	var found *yaml.Node
	var names []string
	for {
		var raw yaml.Node
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("promote: reading the Environment document: %w", err)
		}
		root := documentBody(&raw)
		if root == nil || scalarAt(root, "kind") != "Environment" {
			continue
		}
		name := scalarAt(root, "metadata", "name")
		names = append(names, name)
		if environment != "" && name != environment {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("promote: the document holds more than one Environment (%s); name the one to pin",
				strings.Join(names, ", "))
		}
		found = root
	}
	if found == nil {
		if environment == "" {
			return nil, fmt.Errorf("promote: no Environment document to pin")
		}
		return nil, fmt.Errorf("promote: no Environment named %q in the document (found: %s)",
			environment, strings.Join(names, ", "))
	}
	return found, nil
}

// keyNode returns the key node for one key of a mapping — the position an
// insertion under that key anchors to.
func keyNode(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i]
		}
	}
	return nil
}

func isNull(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")
}

// scalarText serializes an image reference the way YAML would write it,
// quoting only when the value needs it. yaml.Marshal is the authority on that
// rather than a hand-written rule about colons and slashes.
func scalarText(image string) (string, error) {
	if strings.TrimSpace(image) == "" {
		return "", fmt.Errorf("promote: refusing to pin a blank image reference")
	}
	out, err := yaml.Marshal(image)
	if err != nil {
		return "", fmt.Errorf("promote: encoding the image reference: %w", err)
	}
	text := strings.TrimRight(string(out), "\n")
	if strings.Contains(text, "\n") {
		return "", fmt.Errorf("promote: image reference %q does not fit on one line", image)
	}
	return text, nil
}

// verifyPin re-reads the spliced document and checks the pin is what was
// asked for. A splice that produced valid YAML saying something else would be
// the worst failure this package could have, so it is checked rather than
// trusted.
func verifyPin(doc []byte, environment, component, image string) error {
	root, err := environmentDocument(doc, environment)
	if err != nil {
		return fmt.Errorf("promote: the pinned document no longer parses: %w", err)
	}
	components := mapValue(root, "spec", "components")
	if components != nil && components.Kind == yaml.SequenceNode {
		for _, item := range components.Content {
			if item.Kind == yaml.MappingNode && scalarAt(item, "name") == component {
				if got := scalarAt(item, "image"); got == image {
					return nil
				}
				return fmt.Errorf("promote: pinning %s wrote %q, not %q", component, scalarAt(item, "image"), image)
			}
		}
	}
	return fmt.Errorf("promote: pinning %s left no override in the document", component)
}

func unwritable(environment, field, msg string) error {
	return newError(ErrDocumentUnwritable,
		"Environment/"+environment, field, msg,
		"edit the Environment document by hand to add spec.components[].image, or reformat it as a block mapping so kelson can splice the pin in")
}
