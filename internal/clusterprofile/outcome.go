package clusterprofile

import "strings"

// Outcome is the answer to a yes/no question about a cluster that may turn out
// to be unanswerable: "is this component's version supported?", "can this
// storage class snapshot?". It is the single vocabulary for those judgements
// (issue #144) — before it, every package that had to keep "no" apart from
// "could not tell" invented its own three constants.
//
// The constants are deliberately domain-free. The question belongs to the
// function that answers it (support.Check asks about version skew,
// storage.CanSnapshot about snapshot drivers) and both report in the same
// three words; a caller reading `== OutcomeUnknown` does not have to learn a
// second dialect to know what happened.
//
// See the package doc for why the third answer exists at all.
type Outcome int

const (
	// OutcomeUnknown means the question could not be answered: the input was
	// absent, unreadable, or hidden behind a detection [Gap]. It is neither a
	// pass nor a failure, and it is the zero value on purpose — a judgement
	// nobody made must never read as a confident yes.
	OutcomeUnknown Outcome = iota
	// OutcomeYes means the question was answered in the affirmative: supported,
	// capable, present as required.
	OutcomeYes
	// OutcomeNo means the question was answered in the negative. Something was
	// actually checked and found wanting — which is what separates it from
	// OutcomeUnknown.
	OutcomeNo
)

// String renders an Outcome for messages and test output. The words are
// generic because the type is; the message a judgement carries alongside it is
// where the domain is named.
func (o Outcome) String() string {
	switch o {
	case OutcomeYes:
		return "yes"
	case OutcomeNo:
		return "no"
	default:
		return "unknown"
	}
}

// Covers reports whether this gap hides field, matching the field itself and
// anything beneath it: a gap recorded on "certManager.clusterIssuers" covers a
// judgement about "certManager", because the probe that failed was looking at
// part of it. Prefix matching on a dot boundary keeps that robust to the depth
// detection happens to report, without letting "storageClassesFoo" match
// "storageClasses".
func (g Gap) Covers(field string) bool {
	return g.Field == field || strings.HasPrefix(g.Field, field+".")
}

// GapFor returns the first detection gap covering field, and whether one
// exists. It is how a judgement turns into [OutcomeUnknown] with a reason
// attached: the Gap names the permission that would let detection look, so the
// caller can print something actionable instead of "unknown".
func (p ClusterProfile) GapFor(field string) (Gap, bool) {
	for _, g := range p.Incomplete {
		if g.Covers(field) {
			return g, true
		}
	}
	return Gap{}, false
}
