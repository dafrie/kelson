package model

import (
	"fmt"
	"strings"
)

// Code is a stable, machine-actionable validation error code (issue #28).
// The taxonomy is documented in docs/model.md; codes are a compatibility
// promise — existing codes never change meaning.
type Code string

const (
	// Schema-level: shape, types, ranges, formats.
	ErrUnknownField      Code = "schema/unknown-field"
	ErrMissingRequired   Code = "schema/missing-required"
	ErrInvalidFormat     Code = "schema/invalid-format"
	ErrOutOfRange        Code = "schema/out-of-range"
	ErrInvalidEnum       Code = "schema/invalid-enum"
	ErrDuplicateName     Code = "schema/duplicate-name"
	ErrMutuallyExclusive Code = "schema/mutually-exclusive"
	// ErrNotImplemented marks a field the schema defines but no plane consumes
	// yet. Accepting it would report success for work that never happened
	// (issue #141); see notimplemented.go for the gate table.
	ErrNotImplemented Code = "schema/not-implemented"

	// Semantic: cross-references and model rules.
	ErrUnknownService    Code = "ref/unknown-service"
	ErrUnknownServiceKey Code = "ref/unknown-service-key"
	// ErrUnknownComponent replaces ref/unknown-application, which named the
	// leaf ADR-0014 renamed. The code changed with the vocabulary rather than
	// outliving it: kelson is pre-alpha and a code whose noun no longer exists
	// in the spec is worse than a breaking rename.
	ErrUnknownComponent Code = "ref/unknown-component"
	ErrSecretLiteral    Code = "secret/literal"
	ErrNoImageSource    Code = "semantic/no-image-source"
	ErrGitTargetMissing Code = "semantic/git-target-missing"
)

// DocsBaseURL is the stable basis for error documentation links. The docs
// site anchors pages at <DocsBaseURL>/<code with / replaced by ->.
const DocsBaseURL = "https://kelson.dev/model/errors"

// Error is one structured validation problem.
type Error struct {
	Code        Code   `json:"code"`
	Resource    string `json:"resource"` // e.g. "Project/checkout"
	Field       string `json:"field"`    // JSONPath-like: $.spec.components[2].port
	Message     string `json:"message"`
	Remediation string `json:"remediation"` // the fix, stated as an action
	DocsURL     string `json:"docsUrl"`
	Line        int    `json:"line,omitempty"` // 1-based, from YAML source
	Column      int    `json:"column,omitempty"`
}

func (e Error) Error() string {
	loc := e.Field
	if e.Line > 0 {
		loc = fmt.Sprintf("%s (line %d)", e.Field, e.Line)
	}
	return fmt.Sprintf("%s [%s] %s: %s", e.Resource, e.Code, loc, e.Message)
}

// docsURL derives the documentation link for a code.
func docsURL(code Code) string {
	return DocsBaseURL + "/" + strings.ReplaceAll(string(code), "/", "-")

}

// Errors is the collected result of validating a document set. Validation
// never fails fast: every detectable problem is reported.
type Errors []Error

func (e Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation error(s):", len(e))
	for _, err := range e {
		fmt.Fprintf(&b, "\n  - %s", err.Error())
	}
	return b.String()
}

// Codes returns the codes present, for callers that branch on them.
func (e Errors) Codes() []Code {
	out := make([]Code, len(e))
	for i, err := range e {
		out[i] = err.Code
	}
	return out
}
