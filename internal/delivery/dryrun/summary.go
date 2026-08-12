package dryrun

import (
	"github.com/dafrie/kelson/internal/diff"
)

// summarize computes the diff.Summary (issue #43/#44): counts by operation and
// the max-risk gate that a CI pipeline branches on. MaxRisk is derived from
// resource risks only — ordering and audit-mode findings never raise it, so a
// warning cannot block deploy (issue #45).
func summarize(d *diff.Diff) diff.Summary {
	var s diff.Summary
	for _, r := range d.Resources {
		switch r.Op {
		case diff.OpAdded:
			s.Added++
		case diff.OpModified:
			s.Modified++
		case diff.OpRemoved:
			s.Removed++
		}
		switch r.Risk {
		case diff.RiskRestart:
			s.Restarting = append(s.Restarting, r.Kind+"/"+r.Name)
		case diff.RiskDisruptive:
			s.Disruptive = append(s.Disruptive, r.Kind+"/"+r.Name)
		}
		s.MaxRisk = maxRisk(s.MaxRisk, r.Risk)
	}
	if s.MaxRisk == "" {
		s.MaxRisk = diff.RiskCosmetic
	}
	return s
}
