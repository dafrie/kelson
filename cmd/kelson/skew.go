package main

import (
	"fmt"
	"io"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// writeSkew prints the version-skew statements for a resolved ClusterProfile
// (issue #57).
//
// Adopting a component means inheriting its version skew, and the failure this
// prevents is specific: a cluster whose operator is too old to serve the API
// kelson renders against accepts the command, accepts the apply, and fails
// confusingly well after deploy. So the statements are printed at the moment a
// profile enters a command — profile capture, and every render, diff, preview,
// promote and deploy that resolves one — rather than being discovered later.
//
// It is a report and not a refusal. The refusals that exist are specific and
// live where the decision is: internal/clusterprofile/postgres, valkey and helm
// already refuse the presets and kinds their operator cannot serve, and the
// renderer surfaces that. Turning a too-old cert-manager into a global failure
// would refuse specs that ask nothing of cert-manager, which is a worse answer
// than the skew.
//
// Stderr, never stdout: `kelson profile > cluster.yaml` and `kelson render |
// kubectl apply -f -` both put a pipe on stdout, and a warning that lands in
// the pipe is either invisible or corrupting.
func writeSkew(w io.Writer, p clusterprofile.ClusterProfile) {
	if w == nil {
		return
	}
	report := support.Check(p)
	statements := report.Statements()
	if len(statements) == 0 {
		return
	}
	lead := "note"
	if report.Degraded() {
		lead = "warning"
	}
	_, _ = fmt.Fprintf(w, "%s: version skew — %d statement(s) about what this cluster changes "+
		"(docs/reference/support-matrix.md):\n", lead, len(statements))
	for _, s := range statements {
		_, _ = fmt.Fprintf(w, "  - %s\n", s)
	}
}
