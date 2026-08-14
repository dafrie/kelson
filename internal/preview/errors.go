package preview

import "fmt"

// Reason codes for the publisher's own refusals, as opposed to a failure
// reported by the renderer, the model or the registry. They mirror
// build.Error's shape (internal/build/plan.go) rather than renderer.Error's:
// a publish refusal is about the request and the environment it names, not
// about a resource in a rendered set.
const (
	// ReasonNoPreviews: the named environment has no previews block, so there
	// is no artifact repository to publish to and no ResourceSet that would
	// ever fetch what was published.
	ReasonNoPreviews = "preview/no-previews"
	// ReasonNoArtifactRepository: previews.artifacts.repository is empty. The
	// model requires it, so this is reachable only for a spec assembled in
	// code, but publishing to nowhere silently would be worse than a refusal
	// nobody hits.
	ReasonNoArtifactRepository = "preview/no-artifact-repository"
	// ReasonRequiresFlux: the environment's delivery mode is not flux, which is
	// the same gate the renderer applies to the previews block itself
	// (render/previews-require-flux). Publishing an artifact for a lifecycle
	// that will never be reconciled is a push into an empty room.
	ReasonRequiresFlux = "preview/requires-flux"
	// ReasonInvalidChangeRequest: --pr is not a change request number the
	// naming scheme can carry.
	ReasonInvalidChangeRequest = "preview/invalid-change-request"
	// ReasonInvalidSHA: --sha is not a full git commit id, so the tag would not
	// be the one the rendered OCIRepository asks for.
	ReasonInvalidSHA = "preview/invalid-sha"
	// ReasonNameTooLong: <project>-<environment> exceeds the cap, so the
	// preview's namespace would not be a valid DNS-1123 label. It is the
	// publisher's copy of render/preview-name-too-long, refused here so a
	// publish fails at the flag rather than at reconcile time.
	ReasonNameTooLong = "preview/name-too-long"
	// ReasonRepositoryInvalid: previews.artifacts.repository is not a
	// repository reference this publisher can push to — a tag or digest on it,
	// or a name the registry grammar rejects.
	ReasonRepositoryInvalid = "preview/repository-invalid"
)

// Error is a publisher refusal: a named reason, what happened, and what to do.
// The three-field shape is the house style for a plane that refuses before it
// acts (build.Error, renderer.Error), and it is what lets the CLI print a
// remediation without knowing which refusal it is holding.
type Error struct {
	Reason      string
	Message     string
	Remediation string
}

func (e Error) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Reason, e.Message, e.Remediation)
}
