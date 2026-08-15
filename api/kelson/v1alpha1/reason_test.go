package v1alpha1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestReasonVocabularyIsClassifiedAndSpelled is the drift gate on the closed
// reason set, and it reads this package's own source because that is the only
// way it can be one: a list a test keeps by hand goes stale the first time
// somebody adds a constant and does not think about the test.
//
// It catches the two ways a closed vocabulary stops being closed:
//
//   - a reason whose value is not its name — a copy-paste that makes two causes
//     report the same string, which is exactly the value nobody can branch on
//     that the set exists to prevent;
//   - a reason declared and not classified — declaring one is easy, and
//     deciding *which* question it answers (validation, a delivery refusal with
//     a requeue behaviour, a phase that settled badly, or Progressing) is the
//     decision that keeps the set a vocabulary rather than a pile of strings.
//
// The second is the shape of issue #256: for as long as the degraded phase
// reused ReasonRenderFailed, one reason stood in for a cause nobody had named,
// and the status said the renderer had refused a document it rendered perfectly.
func TestReasonVocabularyIsClassifiedAndSpelled(t *testing.T) {
	// Every reason this package declares, in the groups status.go declares them
	// in. A new constant belongs in exactly one of these.
	groups := map[string][]string{
		"validate, resolve, render": {
			ReasonReady, ReasonSpecInvalid, ReasonProjectNotFound, ReasonRenderFailed,
			ReasonClusterProfileUnavailable, ReasonSourcesUnavailable, ReasonRolledBack,
		},
		"delivery refusals, each with a requeue behaviour": {
			ReasonFluxNotInstalled, ReasonRegistryNotConfigured, ReasonArtifactRefInvalid,
			ReasonRegistryUnreachable, ReasonPushDenied, ReasonFluxApplyForbidden,
			ReasonFieldManagerConflict, ReasonNameConflict, ReasonRollbackTargetUnknown,
			ReasonRegistryReadDenied,
		},
		"a delivery that settled badly": {
			ReasonApplyFailed, ReasonWorkloadDegraded, ReasonUnhealthy,
		},
		"Progressing": {
			ReasonRollbackPinned, ReasonReconciling, ReasonSettled,
		},
	}
	classified := map[string]string{}
	for group, reasons := range groups {
		for _, reason := range reasons {
			if other, duplicate := classified[reason]; duplicate {
				t.Errorf("reason %q is classified as both %q and %q: one string cannot be two causes",
					reason, other, group)
			}
			classified[reason] = group
		}
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "status.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing status.go: %v", err)
	}
	declared := 0
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			name := value.Names[0].Name
			if !strings.HasPrefix(name, "Reason") {
				continue
			}
			declared++
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Errorf("%s is not a string literal: a reason is a wire value, not an expression", name)
				continue
			}
			got, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if want := strings.TrimPrefix(name, "Reason"); got != want {
				t.Errorf("%s = %q, want %q: the constant and the string a reader sees must be the "+
					"same word, or grepping for one finds nothing about the other", name, got, want)
			}
			if group, ok := classified[got]; !ok {
				t.Errorf("reason %q (%s) is declared and not classified in this test: say which "+
					"question it answers, and whether internal/controller owes it a requeue row", got, name)
			} else if testing.Verbose() {
				t.Logf("%s: %s", got, group)
			}
		}
		return true
	})
	if declared != len(classified) {
		t.Errorf("status.go declares %d reasons and this test classifies %d: the set is not closed",
			declared, len(classified))
	}
}
