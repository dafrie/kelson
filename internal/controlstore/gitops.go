package controlstore

import "sort"

// Ownership detection: which stored documents somebody else's Flux is
// reconciling from a repository (#248).
//
// # Why the spec store is where this is noticed
//
// [ADR-0027](docs/adr/0027-crd-native-control-plane.md) decision 6 recommends
// keeping Project and Environment documents in a repository and letting Flux
// apply them — and the moment a user takes that advice, kelson's own spec write
// becomes a write that git will silently undo on the next reconcile. The store
// is the one place that sees the evidence: it reads the custom resources
// themselves, and a resource applied by kustomize-controller carries the
// Kustomization that applied it in its labels. Everything above this — the
// wire, the UI banner, the export and the pull-request proposal — is that one
// fact, carried forward.
//
// The alternative was to ask Flux. A Kustomization's `spec.sourceRef` and
// `spec.path` would say *where* in *which* repository the document lives, which
// is the question the proposal flow actually wants answered — but reading it
// needs RBAC on `kustomize.toolkit.fluxcd.io` in whatever namespace the user's
// Flux runs in, and kelson-server's RBAC is deliberately one namespace
// (ADR-0013 §3). So the labels are what this store reports, the repository path
// is asked of the user, and the gap is named rather than papered over.
//
// # Detection is evidence, not a claim about intent
//
// A document carrying these labels *is* being reconciled from a repository:
// kustomize-controller stamps them on everything it applies and removes them
// from nothing. What it is not is a statement that the user meant to hand the
// document over — somebody may have applied it once from a repository and moved
// on. That is why every surface built on this warns and offers an alternative
// rather than refusing: the honest answer is "git will overwrite this", and
// what to do about it is the user's call.

// Flux's kustomize-controller labels every object it applies with the
// Kustomization that owns it. Both keys are needed to identify it: a
// Kustomization is namespaced, and two clusters' worth of `flux-system/apps`
// is a real shape in a monorepo.
const (
	labelFluxKustomization = "kustomize.toolkit.fluxcd.io/name"
	labelFluxNamespace     = "kustomize.toolkit.fluxcd.io/namespace"
)

// DocumentProject is the [GitOpsOwner.Document] value for a project document.
// Environments use their own name, which is why this one is a word an
// environment cannot be called — `validSegment` accepts it, so it is spelled
// here as the constant every caller compares against rather than as a literal
// three clients would each have to get right.
const DocumentProject = "project"

// GitOpsOwner is one stored document and the Flux Kustomization reconciling it.
//
// It is per document rather than per project because the two halves genuinely
// come apart: a common shape is a Project checked into a repository with its
// production Environment beside it, while `development` is created and edited
// through kelson. Reporting "this project is GitOps-managed" for that would
// disable an edit that is perfectly safe, and reporting nothing would let the
// unsafe one through.
type GitOpsOwner struct {
	// Document is [DocumentProject] or an environment's name.
	Document string
	// Kustomization is the Flux Kustomization's name, and Namespace is the
	// namespace it lives in — together, what `flux get kustomization` answers to.
	Kustomization, Namespace string
}

// gitOpsOwner reads the ownership labels off one object.
//
// The name alone is enough to report ownership; the namespace is reported when
// it is there. Both are written by the same controller in the same operation,
// so a name without a namespace is not a state Flux produces — but a label a
// person copied by hand is, and half an answer is still true.
func gitOpsOwner(document string, labels map[string]string) (GitOpsOwner, bool) {
	name := labels[labelFluxKustomization]
	if name == "" {
		return GitOpsOwner{}, false
	}
	return GitOpsOwner{
		Document:      document,
		Kustomization: name,
		Namespace:     labels[labelFluxNamespace],
	}, true
}

// gitOpsOwners collects the owners of a project's documents, project first and
// then environments by name. Deterministic order, because this list is rendered
// into a sentence a person reads and a list that reshuffled between reads would
// look like the cluster had changed.
func gitOpsOwners(set resourceSet) []GitOpsOwner {
	var out []GitOpsOwner
	if owner, ok := gitOpsOwner(DocumentProject, set.project.Labels); ok {
		out = append(out, owner)
	}
	names := make([]string, 0, len(set.environments))
	for name := range set.environments {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if owner, ok := gitOpsOwner(name, set.environments[name].Labels); ok {
			out = append(out, owner)
		}
	}
	return out
}
