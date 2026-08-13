package api

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/renderer"
	"github.com/dafrie/kelson/internal/serverstate"
)

// One wire error shape, six plane vocabularies (ADR-0013 §2). model.Error,
// renderer.Error, delivery.Error, serverstate.Error, build.Error and
// promote.Error each fill the subset of kelson.v1alpha1.Error they know. Codes
// pass through verbatim — an agent branching on "schema/not-implemented" or
// "store/version-conflict" sees the same string here that the owning Go package
// defines, and the wire must not invent a second taxonomy.

// wireErrors projects a plane error onto the API's structured error list. It
// returns nil for an error that carries no plane taxonomy, which is how the
// callers distinguish "the request was answered with findings" from "the server
// failed".
//
// Every structured error this package emits — inline findings, Settled events,
// and the details ConnectRPC errors carry — is built here, which is why the
// known-value scrub (issue #117) is applied at this one point rather than at
// each of the several dozen call sites of fail/failRequest/specFindings. A
// credential kelson has resolved is registered with internal/redact when it is
// learned; from then on it cannot reach an error message, whichever plane
// produced it. Per ADR-0009 kelson resolves almost nothing, so the scrub is
// normally a no-op — it is the guarantee that matters, not the frequency.
func wireErrors(err error) []*kelsonv1alpha1.Error {
	return scrubErrors(planeErrors(err))
}

// scrubErrors runs the free-text fields of a structured error through the
// process-wide known-value scrubber. Code, resource, field and docs URL are
// left alone: they are vocabulary, and an agent branches on them.
func scrubErrors(errs []*kelsonv1alpha1.Error) []*kelsonv1alpha1.Error {
	for _, e := range errs {
		if e == nil {
			continue
		}
		e.Message = redact.Scrub(e.Message)
		e.Remediation = redact.Scrub(e.Remediation)
		e.Cause = redact.Scrub(e.Cause)
	}
	return errs
}

// planeErrors is the taxonomy projection itself: which plane owns this error,
// and which subset of the wire Error it fills.
func planeErrors(err error) []*kelsonv1alpha1.Error {
	if err == nil {
		return nil
	}

	var modelErrs model.Errors
	if errors.As(err, &modelErrs) {
		out := make([]*kelsonv1alpha1.Error, 0, len(modelErrs))
		for _, e := range modelErrs {
			out = append(out, fromModel(e))
		}
		return out
	}
	var modelErr model.Error
	if errors.As(err, &modelErr) {
		return []*kelsonv1alpha1.Error{fromModel(modelErr)}
	}

	var renderErrs renderer.Errors
	if errors.As(err, &renderErrs) {
		out := make([]*kelsonv1alpha1.Error, 0, len(renderErrs))
		for _, e := range renderErrs {
			out = append(out, fromRenderer(e))
		}
		return out
	}
	var renderErr renderer.Error
	if errors.As(err, &renderErr) {
		return []*kelsonv1alpha1.Error{fromRenderer(renderErr)}
	}

	var deliveryErr delivery.Error
	if errors.As(err, &deliveryErr) {
		return []*kelsonv1alpha1.Error{fromDelivery(deliveryErr)}
	}

	var storeErr serverstate.Error
	if errors.As(err, &storeErr) {
		return []*kelsonv1alpha1.Error{fromStore(storeErr)}
	}

	var buildErr build.Error
	if errors.As(err, &buildErr) {
		return []*kelsonv1alpha1.Error{fromBuild(buildErr)}
	}

	var promoteErr promote.Error
	if errors.As(err, &promoteErr) {
		return []*kelsonv1alpha1.Error{fromPromote(promoteErr)}
	}
	return nil
}

// specFindings is the subset of [wireErrors] a call may report inline: the
// spec's own model and renderer errors. A store failure or a delivery failure
// is never a finding about the spec — inlining one would tell a client its spec
// was rejected when the truth is that the server could not read it — so those
// keep travelling as ConnectRPC errors.
func specFindings(err error) []*kelsonv1alpha1.Error {
	var modelErrs model.Errors
	var modelErr model.Error
	var renderErrs renderer.Errors
	var renderErr renderer.Error
	if errors.As(err, &modelErrs) || errors.As(err, &modelErr) ||
		errors.As(err, &renderErrs) || errors.As(err, &renderErr) {
		return wireErrors(err)
	}
	return nil
}

func fromModel(e model.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        string(e.Code),
		Resource:    e.Resource,
		Field:       e.Field,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     e.DocsURL,
		Line:        int32(e.Line),   //nolint:gosec // YAML positions are small by construction
		Column:      int32(e.Column), //nolint:gosec // YAML positions are small by construction
	}
}

func fromRenderer(e renderer.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Code,
		Application: e.Application,
		Overlay:     e.Overlay,
		Target:      e.Target,
		Message:     e.Message,
		Remediation: e.Remediation,
	}
}

func fromDelivery(e delivery.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        string(e.Code),
		Resource:    e.Resource,
		Field:       e.Field,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     e.DocsURL,
		Cause:       e.Cause,
	}
}

// fromBuild carries a build-plane refusal onto the wire. It has no resource and
// no field: build/no-source and its siblings are statements about the whole
// request — this Project cannot be built, and here is what to change — rather
// than findings against one line of one document.
func fromBuild(e build.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Reason,
		Message:     e.Message,
		Remediation: e.Remediation,
	}
}

func fromStore(e serverstate.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        string(e.Code),
		Resource:    e.Resource,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     e.DocsURL,
		Cause:       e.Cause,
	}
}

func fromPromote(e promote.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        string(e.Code),
		Resource:    e.Resource,
		Field:       e.Field,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     e.DocsURL,
	}
}

// fail wraps err as a ConnectRPC error, attaching every structured error it
// carries as an error detail. A client that speaks the taxonomy reads the
// details; one that does not still gets the message.
func fail(code connect.Code, err error) *connect.Error {
	cerr := connect.NewError(code, scrubbed{err: err})
	for _, wire := range wireErrors(err) {
		detail, derr := connect.NewErrorDetail(wire)
		if derr != nil {
			// A detail that cannot be marshalled must not lose the error it
			// was describing; the message already carries it.
			continue
		}
		cerr.AddDetail(detail)
	}
	return cerr
}

// failStore maps a state-plane error onto its ConnectRPC code (ADR-0013 §2).
// The three store codes are the whole vocabulary: a version conflict is a
// failed precondition (the caller's expectation about the stored state was
// wrong), missing state is not-found, and an over-budget payload is exhausted
// resources.
func failStore(err error) error {
	switch {
	case serverstate.AsVersionConflict(err):
		return fail(connect.CodeFailedPrecondition, err)
	case serverstate.AsNotFound(err):
		return fail(connect.CodeNotFound, err)
	case serverstate.AsTooLarge(err):
		return fail(connect.CodeResourceExhausted, err)
	default:
		return nil
	}
}

// failPromote maps a promotion refusal onto its ConnectRPC code. Two of the
// codes are statements about the world rather than about the request — nothing
// has been deployed to the source, this document cannot be spliced — and a
// caller must be able to tell them from a request it could fix by editing.
func failPromote(err error) error {
	var pe promote.Error
	if !errors.As(err, &pe) {
		return nil
	}
	switch pe.Code {
	case promote.ErrNothingDeployed, promote.ErrDocumentUnwritable:
		return fail(connect.CodeFailedPrecondition, err)
	default:
		return fail(connect.CodeInvalidArgument, err)
	}
}

// scrubbed is the free-text half of a ConnectRPC error passing through the
// known-value scrubber on its way to the client (issue #117). It wraps rather
// than replaces so errors.As still reaches the plane error underneath — the
// taxonomy travels in the details, and losing the chain to protect the prose
// would trade one contract for another.
type scrubbed struct{ err error }

func (e scrubbed) Error() string { return redact.Scrub(e.err.Error()) }
func (e scrubbed) Unwrap() error { return e.err }

// unavailableError marks a failure of the server's own dependencies — an
// unreachable cluster, an adapter that could not be built — as opposed to a
// request the caller got wrong. The distinction is what stops a broken cluster
// connection being reported to an agent as an invalid argument it could fix by
// editing its request.
type unavailableError struct{ err error }

func (e unavailableError) Error() string { return e.err.Error() }
func (e unavailableError) Unwrap() error { return e.err }

func unavailable(format string, a ...any) error {
	return unavailableError{err: fmt.Errorf(format, a...)}
}

// unimplemented reports a seam this build was not wired with.
func unimplemented(what string) error {
	return connect.NewError(connect.CodeUnimplemented,
		fmt.Errorf("%s is not available in this server: it was started without the backing seam", what))
}

// failRequest is the single boundary translation: state-plane errors keep their
// own codes, a dependency failure is Unavailable, and everything else is the
// caller's request being wrong.
func failRequest(err error) error {
	if cerr := failStore(err); cerr != nil {
		return cerr
	}
	if cerr := failPromote(err); cerr != nil {
		return cerr
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr
	}
	var unavail unavailableError
	if errors.As(err, &unavail) {
		return fail(connect.CodeUnavailable, err)
	}
	return fail(connect.CodeInvalidArgument, err)
}
