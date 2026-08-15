package controller

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
)

// The events a skipped teardown records. They are the only trace an orphaned
// pair leaves in the cluster besides the objects themselves: the custom
// resource is gone a moment later, so there is no status left to read and the
// event on the deleted Environment is what `kubectl get events` still shows.
const (
	// EventReasonOrphaned — the Environment was deleted, the annotation was in
	// force, and the Flux pair was deliberately left behind.
	EventReasonOrphaned = "Orphaned"

	// EventReasonOrphanAnnotationMalformed — the annotation was there and its
	// value is not a boolean. The deletion was orphaned anyway (see [orphanFor]),
	// which is the safe half of an ambiguous instruction and still worth a
	// warning, because the operator who typed it may have meant the opposite.
	EventReasonOrphanAnnotationMalformed = "OrphanAnnotationMalformed"
)

// orphan is what [v1alpha1.AnnotationOrphanOnDelete] means for one deletion.
type orphan struct {
	// Requested means the finalizer releases without tearing the pair down.
	Requested bool

	// Value is the annotation as written, trimmed. Empty means it was absent —
	// which is not the same as present and empty, see [orphanFor].
	Value string

	// Malformed means the value is present and is not a boolean.
	Malformed bool
}

// orphanFor reads the escape hatch off one Environment (issue #242).
//
// # Why an unreadable value opts in
//
// The two mistakes this function can make are not the same size. Reading
// `orphan-on-delete: ture` as "no" prunes a production namespace because of a
// typo in the annotation whose entire purpose was to prevent that, and nothing
// undoes it: the Kustomization is gone, its inventory is gone, and the workloads
// with it. Reading it as "yes" leaves an application running that somebody meant
// to remove, and `kubectl delete kustomization <project>-<environment>` finishes
// the job whenever they notice. So a value that parses decides — including
// `false`, which is the default said out loud — and a value that does not parse
// takes the reversible answer and says so.
//
// The presence of the key is what is read, not its emptiness: `kubectl annotate
// environment production kelson.dev/orphan-on-delete=""` is somebody reaching
// for this hatch, and an absent key and an empty value must not mean the same
// thing when one of the two meanings destroys an application.
func orphanFor(annotations map[string]string) orphan {
	raw, present := annotations[v1alpha1.AnnotationOrphanOnDelete]
	if !present {
		return orphan{}
	}
	value := strings.TrimSpace(raw)
	requested, err := strconv.ParseBool(value)
	if err != nil {
		return orphan{Requested: true, Value: value, Malformed: true}
	}
	return orphan{Requested: requested, Value: value}
}

// orphanedMessage is what the log line and the event say when a deletion skips
// the teardown.
//
// It names what survived, what is now true of it, and both ways out, because
// the operator reading it — possibly weeks later, off `kubectl get events`, with
// the Environment long gone — is asking exactly those questions. The Flux
// namespace is described rather than named: the reconciler does not hold it,
// [Deliverer] does, and a message that guessed `kelson-system` would be wrong on
// every instance started with --flux-namespace.
func orphanedMessage(project, environment string) string {
	name := ObjectName(project, environment)
	return fmt.Sprintf(
		"%s=true: the Environment was deleted and its workloads were deliberately left running. The "+
			"OCIRepository and Kustomization named %s stay in kelson's Flux namespace, keep their "+
			"kelson.dev provenance labels, and go on reconciling the last artifact kelson published — with "+
			"nothing left that declares them, which is what orphaned means. Two ways out: re-apply an "+
			"Environment named %s that binds project %s in this namespace, which adopts the pair back, or "+
			"`kubectl delete kustomization %s -n <kelson's namespace>`, which prunes the workloads after all.",
		v1alpha1.AnnotationOrphanOnDelete, name, environment, project, name)
}

// orphanMalformedMessage is the warning beside it. An annotation that quietly
// did something other than what its value says is worse than one that failed,
// so the value is quoted back and the rule is stated.
func orphanMalformedMessage(value string) string {
	return fmt.Sprintf(
		"%s=%q is not a boolean, and this deletion was orphaned rather than torn down: an unreadable "+
			"opt-out is answered the way that can still be undone by hand. Write `true` to mean it, or "+
			"`false` to get the default back, which deletes the Kustomization and prunes the workloads.",
		v1alpha1.AnnotationOrphanOnDelete, value)
}
