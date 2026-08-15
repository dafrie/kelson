package api

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/renderer"
	"github.com/dafrie/kelson/internal/secret"
)

// One wire error shape, six plane vocabularies (ADR-0013 §2). model.Error,
// renderer.Error, delivery.Error, controlstore.Error, build.Error and
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

	var storeErr controlstore.Error
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

	var secretErr secret.Error
	if errors.As(err, &secretErr) {
		return []*kelsonv1alpha1.Error{fromSecret(secretErr)}
	}

	// The authorization plane (issue #74). It is the api plane's own vocabulary
	// rather than another package's, and it rides the same wire shape so an
	// agent branching on `auth/out-of-scope` reads it exactly where it reads
	// `store/not-found`.
	var authErr authzError
	if errors.As(err, &authErr) {
		return []*kelsonv1alpha1.Error{authErr.wire()}
	}

	// The agent-policy plane (issue #75, ADR-0025). Same wire shape again, and
	// its own prefix: `agent-policy/propose-only` is a statement about this
	// environment's spec, where `policy/webhook-denied` (internal/diff) is one
	// about the cluster's admission control.
	var policyErr policyError
	if errors.As(err, &policyErr) {
		return []*kelsonv1alpha1.Error{policyErr.wire()}
	}

	// The build-report plane (ADR-0034 decision 3). Also the api plane's own
	// vocabulary, with its own prefix: `report/image-not-pinned` is a statement
	// about what CI sent, where `build/no-source` is one about what the Project
	// says — and an agent that conflated them would edit the wrong file.
	var reportErr reportError
	if errors.As(err, &reportErr) {
		return []*kelsonv1alpha1.Error{reportErr.wire()}
	}

	// The forge-connection plane (ADR-0033, issue #248). The api plane's
	// vocabulary again, and its prefix answers a question the others cannot:
	// `connection/capability-unsupported` says this *forge* has no repository
	// browser, where `store/not-found` would say the connection is missing and
	// `auth/out-of-scope` would say the caller may not ask. A client that
	// conflated them would tell somebody to fix a connection that is working.
	var connErr connectionError
	if errors.As(err, &connErr) {
		return []*kelsonv1alpha1.Error{connErr.wire()}
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
		Code: e.Code,
		// The wire field keeps its v1alpha1 name; the renderer's does not
		// (ADR-0032).
		Application: e.Component,
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

func fromStore(e controlstore.Error) *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        string(e.Code),
		Resource:    e.Resource,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     e.DocsURL,
		Cause:       e.Cause,
	}
}

// fromSecret carries a secret-backend refusal onto the wire (issue #116).
//
// Like a build error it has no field: `secret/not-managed` and its siblings are
// statements about an object in a namespace, not findings against a line of a
// document, so Resource carries `Secret/<namespace>/<name>` and Field stays
// empty. The message, remediation and cause pass through scrubErrors like every
// other free-text field — which for this service is the one that has to hold:
// it is the only RPC in the schema that receives secret values.
func fromSecret(e secret.Error) *kelsonv1alpha1.Error {
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
	case controlstore.AsVersionConflict(err):
		return fail(connect.CodeFailedPrecondition, err)
	case controlstore.AsNotFound(err):
		return fail(connect.CodeNotFound, err)
	case controlstore.AsTooLarge(err):
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

// failSecret maps a secret-backend refusal onto its ConnectRPC code.
//
// The split that matters is between "your request is wrong" and "the world
// refuses it". A malformed key or a missing project is the caller's to fix by
// editing the request; a Secret kelson does not manage, or a namespace that
// does not exist, is a well-formed request the cluster's state refuses — a
// failed precondition, which is what tells an agent to look at the cluster
// rather than at its own arguments. A read or write the API server would not
// answer is the server's dependency failing, so it is Unavailable for the same
// reason unavailableError exists.
func failSecret(err error) error {
	var se secret.Error
	if !errors.As(err, &se) {
		return failRequest(err)
	}
	switch se.Code {
	case secret.ErrNotManaged, secret.ErrNamespaceMissing:
		return fail(connect.CodeFailedPrecondition, err)
	case secret.ErrNotFound:
		return fail(connect.CodeNotFound, err)
	case secret.ErrReadFailed, secret.ErrWriteFailed:
		return fail(connect.CodeUnavailable, err)
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
// own codes, a capability kelson has not rebuilt yet is Unimplemented, a
// dependency failure is Unavailable, and everything else is the caller's
// request being wrong.
func failRequest(err error) error {
	if cerr := failStore(err); cerr != nil {
		return cerr
	}
	// A gated capability is not the caller's mistake and must never be reported
	// as one: an agent that reads InvalidArgument rewrites its request and
	// tries again forever, where Unimplemented tells it to stop (ADR-0028,
	// issue #224). The structured detail rides along either way.
	if delivery.AsNotImplemented(err) {
		return fail(connect.CodeUnimplemented, err)
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
