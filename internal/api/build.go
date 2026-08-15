package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// DefaultBuildTimeout is the budget one build gets, matching the CLI's
// --timeout default. It is generous next to a deployment's five minutes for the
// same reason: a cold build with no cache pulls a base image and compiles, and
// killing that at five minutes would fail more builds than it saves.
const DefaultBuildTimeout = 30 * time.Minute

// logChunkSize bounds one Log event. It is a transport unit, not a semantic
// one: the executor writes whatever the pod's log stream hands it, and this
// splits those writes so a single enormous write does not become a single
// enormous frame.
const logChunkSize = 32 << 10

// logChunkBuffer is how many chunks may sit between the build and the stream.
// Small on purpose. The buffer exists so a momentary stall in Send does not
// stall the log copy on every write; it is not a place to accumulate a build's
// output. Once it is full the writer blocks, which pushes back on the pod log
// copy — the same backpressure the CLI gets from a slow stdout, and the honest
// alternative to dropping output nobody would know was missing.
const logChunkBuffer = 8

// Build runs one in-cluster build and streams it (issues #48, #54).
//
// The pipeline is `kelson build`'s, in the same order and through the same
// shared functions (internal/build/plan.go): resolve the spec, resolve the
// strategy, derive the destination, resolve the ref to a commit, then run the
// executor. What differs is only what a server cannot have — there is no local
// checkout, so `auto` cannot be detected and says so rather than guessing.
//
// Three events, in one order: Started once everything it reports is settled
// fact, Log chunks while the Job runs, Finished with the pinned reference. A
// failure is a ConnectRPC error rather than a fourth event, because a failed
// build produced no image and there is nothing for a result message to say.
func (s *Server) Build(ctx context.Context, req *connect.Request[kelsonv1alpha1.BuildRequest], stream *connect.ServerStream[kelsonv1alpha1.BuildResponse]) error {
	msg := req.Msg

	// resolve, not renderSpec: a build reads spec.source, spec.build and the
	// Environment's identity, and nothing a ClusterProfile decides. Rendering
	// would additionally demand an image for the very spec that has none yet,
	// which is the spec a build exists to serve (#136).
	project, environment, resolved, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return failRequest(err)
	}

	// Agent policy (ADR-0025). A build changes no environment — it puts an
	// artifact in a registry — so `propose-only` does not refuse it and
	// `forbid: [build]` is the rule that does, which is why the guard runs
	// unconditionally here and there is no dry-run rung to exempt.
	if _, err := s.guard(ctx, model.AgentOpBuild, project.Metadata.Name, environment.Metadata.Name); err != nil {
		return err
	}
	// After the guard, for the reason SetSecret states: a refusal must not
	// depend on whether this server happens to have the seam.
	if s.build == nil {
		return unimplemented("in-cluster builds")
	}

	source := project.Spec.Source
	if source == nil || strings.TrimSpace(source.Git) == "" {
		return failRequest(build.Error{
			Reason:      build.ReasonNoSource,
			Message:     fmt.Sprintf("Project %s has no spec.source.git, so there is nothing to build from", project.Metadata.Name),
			Remediation: "add spec.source.git to the Project, or deploy a pre-built image with an explicit image reference",
		})
	}

	// A nil tree is the whole difference between this and the CLI: the server
	// has no checkout to detect from, so a spec that leaves the strategy to
	// `auto` is refused with build/detection-needs-source rather than guessed
	// at. See the RPC's proto comment and issue #50.
	detection, err := build.ResolveStrategy(project.Spec.Build, nil)
	if err != nil {
		return failRequest(err)
	}

	prefix := msg.GetRegistry()
	if prefix == "" {
		prefix = s.buildDefaults.Registry
	}
	if prefix == "" {
		return fail(connect.CodeFailedPrecondition, fmt.Errorf(
			"api: no destination registry: the request named none and this server was started without --registry (KELSON_REGISTRY)"))
	}
	image, err := registry.Repository(prefix, project.Metadata.Name)
	if err != nil {
		return failRequest(err)
	}

	namespace := s.buildDefaults.Namespace
	if namespace == "" {
		namespace = resolved.Environment.Namespace
	}
	pushSecret := msg.GetPushSecret()
	if pushSecret == "" {
		pushSecret = s.buildDefaults.PushSecret
	}

	plane, err := s.build(ctx, BuildTarget{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		Strategy:    string(detection.Strategy),
		Namespace:   namespace,
		PushSecret:  pushSecret,
	})
	if err != nil {
		return failRequest(unavailable("api: building the build plane: %w", err))
	}
	if plane == nil || plane.Builder == nil {
		return failRequest(unavailable("api: the build plane produced no builder for %s/%s",
			project.Metadata.Name, environment.Metadata.Name))
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultBuildTimeout)
	defer cancel()

	ref := msg.GetRef()
	if ref == "" {
		ref = source.Ref
	}
	revision, err := resolveRevision(ctx, plane.Revisions, source.Git, ref)
	if err != nil {
		return failRequest(err)
	}

	request := build.Request{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		// Component stays empty: one build serves the whole Project, so
		// naming one of its components would put a false label on the Job
		// and on the image (build.DestinationTag says the same thing).
		SourceGit:  source.Git,
		SourceRef:  revision,
		Dockerfile: build.DockerfilePath(project.Spec.Build),
		Image:      image,
		Tag:        build.DestinationTag(project.Metadata.Name, revision),
		Revision:   revision,
	}

	if err := stream.Send(&kelsonv1alpha1.BuildResponse{
		Event: &kelsonv1alpha1.BuildResponse_Started_{
			Started: &kelsonv1alpha1.BuildResponse_Started{
				Strategy:        string(detection.Strategy),
				ImageRepository: request.Image,
				Tag:             request.Tag,
				Revision:        request.Revision,
			},
		},
	}); err != nil {
		return err
	}

	res, err := runBuild(ctx, cancel, plane.Builder, request, stream)
	if err != nil {
		return failRequest(err)
	}
	if res.Reference == "" {
		return fail(connect.CodeInternal, fmt.Errorf("api: the build reported success but returned no image reference"))
	}
	if registry.Mutable(res.Reference) {
		// The executor pins by the digest it parsed out of the push. An
		// unpinned reference would make the deploy that follows
		// non-reproducible, which is the one guarantee the build exists to
		// provide (#51).
		return fail(connect.CodeInternal, fmt.Errorf("api: the build returned %s, which is not pinned by digest", res.Reference))
	}
	// A build produces an image, not a delivery revision, so the record's
	// "what did this produce?" field is the pinned reference — which is exactly
	// what a later deploy would name (issue #78).
	auditChange(ctx, controlstore.AuditChange{Revision: res.Reference, From: request.Revision})
	return stream.Send(&kelsonv1alpha1.BuildResponse{
		Event: &kelsonv1alpha1.BuildResponse_Finished_{
			Finished: &kelsonv1alpha1.BuildResponse_Finished{
				Reference: res.Reference,
				Digest:    res.Digest,
			},
		},
	})
}

// ReportBuild validates the CI hand-off in full, and then refuses it
// (ADR-0034 decision 3).
//
// # Why validate something that cannot succeed
//
// The schema is the contract CI is written against, and a pipeline is written
// long before the trigger pipeline behind this method exists. A method that
// answered Unimplemented to *everything* would teach a pipeline author nothing:
// they would wire up a report with a truncated SHA, a tag instead of a digest
// and a component name the Project never declared, see the same refusal a
// correct request gets, and discover all three on the day the slot is filled.
// So every check the report's own contract states runs here and answers
// InvalidArgument, and only a report kelson would have *acted* on reaches the
// gate. The two answers are different codes on purpose: an agent retries
// neither, and a human reads which one they got.
//
// # What is behind the gate, and is not built
//
// Recording images for a commit, resolving which environments and previews that
// commit feeds, and driving the server-side render→publish are the trigger
// pipeline of [ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 1,
// which stands on the [ADR-0028](docs/adr/0028-delivery-spine.md) spine.
//
// It refuses rather than accepting and dropping the report. `accepted: true`
// with nothing triggered is a lie a pipeline would believe — CI would go green
// having published nothing — and ADR-0034 names "why didn't my preview update"
// as the question this whole path must stay answerable for. Unimplemented tells
// an agent to stop; a cheerful empty response would tell it to carry on.
//
// The preview publish for `pr > 0` was considered here and deliberately not
// wired: this plane's only publish-shaped seam is the L2 dry-run
// [PreviewEngine], which computes a diff and applies nothing, and PreviewService
// reads previews rather than creating them. Reaching past those to the delivery
// plane is exactly what the api plane's seam discipline forbids, and a
// half-wired path that published a preview while recording no image-for-commit
// mapping would answer "why didn't my preview update" with a state nothing owns.
//
// The tracking slot names the ADR's pipeline rather than an issue, which is the
// one place this departs from [delivery.NotImplemented]'s contract. That
// contract asks for an issue reference so a caller can find out when the answer
// changes, and ADR-0034 is proposed with its implementation sequencing
// explicitly left to the tracker ("Revisit when: R2 lands and the first slice
// ships") — so no issue exists to name yet. Naming the ADR is the findable
// reference that does exist; the issue replaces it when the slice is filed.
func (s *Server) ReportBuild(ctx context.Context, req *connect.Request[kelsonv1alpha1.ReportBuildRequest]) (*connect.Response[kelsonv1alpha1.ReportBuildResponse], error) {
	msg := req.Msg
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())
	auditTarget(ctx, msg.GetProject(), "")

	if err := validateReport(msg); err != nil {
		return nil, failRequest(err)
	}
	// Agent policy (ADR-0025), filed under `deploy` because that is what a
	// report's effect is (policy.go's agentOperations says why at length). The
	// report names no environment, so every stored environment of the project
	// gets a say — the same reach DeleteSpec has, for the same reason: what the
	// call sets in motion is not scoped to one of them.
	if err := s.guardStored(ctx, model.AgentOpDeploy, msg.GetProject()); err != nil {
		return nil, err
	}
	if err := s.checkReportedComponents(ctx, msg); err != nil {
		return nil, failRequest(err)
	}

	// Everything above this line judged the request. Nothing below it exists.
	//
	// The idempotency key needs no handling of its own here, and that is a
	// consequence rather than an omission: a replay is only ambiguous when the
	// first attempt changed something, and this call changes nothing. It is
	// recorded on the audit entry above so a retry and the report it repeats
	// are visibly the same one (#71), and the moment the trigger pipeline
	// records an image-for-commit mapping, the key becomes that record's
	// replay token — which is what CreateConnectionRequest's comment describes
	// for a call that does write.
	return nil, fail(connect.CodeUnimplemented, delivery.NotImplemented("report-build",
		"kelson cannot accept a build report: the report is well-formed, and the trigger pipeline that turns "+
			"one into a server-side render and publish (ADR-0034) is not built, nor is the image-for-commit "+
			"record it reads",
		"the ADR-0034 trigger pipeline"))
}

// The report's own refusals. They are the api plane's vocabulary rather than
// internal/build's, for the reason authzError and policyError are: a report is
// not a build — nothing is compiled and no registry is written — and filing its
// mistakes under `build/` would make `build/*` mean two different things to an
// agent branching on it.
const (
	// ErrReportShaInvalid: `sha` is not a 40-character commit.
	ErrReportShaInvalid = "report/sha-invalid"
	// ErrReportNoImages: the report carries no images.
	ErrReportNoImages = "report/no-images"
	// ErrReportImageNotPinned: an image is named by tag rather than by digest.
	ErrReportImageNotPinned = "report/image-not-pinned"
	// ErrReportUnknownComponent: an image names a component the Project does
	// not declare.
	ErrReportUnknownComponent = "report/unknown-component"
)

const reportDocsBase = "https://kelson.dev/server/errors"

// reportError is one refusal of a build report. It rides the same wire shape as
// every other plane's error (errors.go) and carries its own prefix, so an agent
// branching on `report/sha-invalid` reads it exactly where it reads
// `store/not-found`.
type reportError struct {
	Code        string
	Field       string
	Message     string
	Remediation string
}

func (e reportError) Error() string {
	return fmt.Sprintf("report-build [%s] %s: %s", e.Code, e.Message, e.Remediation)
}

func (e reportError) wire() *kelsonv1alpha1.Error {
	return &kelsonv1alpha1.Error{
		Code:        e.Code,
		Resource:    "report-build",
		Field:       e.Field,
		Message:     e.Message,
		Remediation: e.Remediation,
		DocsUrl:     reportDocsBase + "/" + e.Code,
	}
}

func refusedReport(code, field, message, remediation string) error {
	return reportError{Code: code, Field: field, Message: message, Remediation: remediation}
}

// validateReport checks what the request can be judged on without reading
// anything: the join key, and the images.
//
// The digest rule is the one worth stating twice. A mutable tag would make the
// artifact kelson publishes describe something that can change underneath it,
// which is the single guarantee the build plane exists to provide (#51,
// ADR-0010) — and a report is the one path where the image comes from outside,
// so it is the one place that guarantee can be lost by accident.
func validateReport(msg *kelsonv1alpha1.ReportBuildRequest) error {
	if strings.TrimSpace(msg.GetProject()) == "" {
		return fmt.Errorf("api: ReportBuild needs a project: a report triggers a render of the spec the server " +
			"holds, so it must say whose")
	}
	if !build.IsCommit(msg.GetSha()) {
		return refusedReport(ErrReportShaInvalid, "sha",
			fmt.Sprintf("sha is %q, and a report's commit is a full 40-character hexadecimal SHA", msg.GetSha()),
			"send the commit the images were built from in full. It is the join key of the whole hand-off — the "+
				"preview publishes at it, and a tracking environment redeploys only if it is the head of the "+
				"ref it follows — and an abbreviation cannot be compared against either")
	}
	if len(msg.GetImages()) == 0 {
		return refusedReport(ErrReportNoImages, "images",
			"the report names no images",
			"map each built component to the image it produced, e.g. {\"web\": \"ghcr.io/acme/checkout-web@sha256:…\"}. "+
				"A report with nothing in it would trigger a publish of exactly what is already deployed")
	}

	// Sorted, so a report with two bad images always names the same one first:
	// a refusal that varied with Go's map order is a refusal nobody can fix
	// twice the same way.
	for _, component := range sortedKeys(msg.GetImages()) {
		ref := strings.TrimSpace(msg.GetImages()[component])
		if ref == "" {
			return refusedReport(ErrReportImageNotPinned, "images."+component,
				fmt.Sprintf("component %q reports an empty image reference", component),
				"remove the entry, or give it the digest-pinned reference the build produced. A component the "+
					"Project declares and the report omits keeps whatever the spec resolves for it")
		}
		if registry.Mutable(ref) {
			return refusedReport(ErrReportImageNotPinned, "images."+component,
				fmt.Sprintf("component %q reports %s, which is not pinned by digest", component, ref),
				"report the reference your push resolved to — `repository@sha256:…`. A tag can be moved after "+
					"the report, which would make the artifact kelson publishes describe something else "+
					"entirely (#51, ADR-0010)")
		}
	}
	return nil
}

// checkReportedComponents refuses a report naming a component the Project does
// not declare, which the proto asks for by name: "a key the Project does not
// declare is an error naming it, not a silent drop".
//
// The reverse is not an error and is deliberately not checked: a component the
// Project declares and the report omits keeps whatever the spec resolves for
// it, so a partial report is a partial pin rather than a broken render.
func (s *Server) checkReportedComponents(ctx context.Context, msg *kelsonv1alpha1.ReportBuildRequest) error {
	if s.specs == nil {
		return unimplemented("the spec store")
	}
	stored, err := s.specs.Get(ctx, msg.GetProject())
	if err != nil {
		return err
	}
	project, ok := decodeProjectDocument(stored.Documents.Project)
	if !ok {
		return fmt.Errorf("api: the stored Project document for %q could not be decoded, so the report cannot be "+
			"checked against the components it declares", msg.GetProject())
	}
	declared := make(map[string]bool, len(project.Spec.Components))
	for _, c := range project.Spec.Components {
		declared[c.Name] = true
	}
	for _, component := range sortedKeys(msg.GetImages()) {
		if declared[component] {
			continue
		}
		return refusedReport(ErrReportUnknownComponent, "images."+component,
			fmt.Sprintf("Project %s declares no component named %q", msg.GetProject(), component),
			fmt.Sprintf("report images for the components the Project declares (%s), or add this one to "+
				"spec.components. An image for a component that does not exist has nothing to pin",
				componentList(project)))
	}
	return nil
}

func componentList(project *model.Project) string {
	names := make([]string, 0, len(project.Spec.Components))
	for _, c := range project.Spec.Components {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runBuild runs the builder on its own goroutine and pumps its output onto the
// stream from this one.
//
// The builder takes an io.Writer and this handler has a stream, so something
// has to bridge them. It is a goroutine and a small channel rather than a
// direct call for one reason: a ServerStream's Send is not safe to call from
// two goroutines, and a driver is free to write logs from whichever goroutine
// it likes. Sending only from here makes that a non-question. The channel is
// bounded (logChunkBuffer chunks of at most logChunkSize), so a client that
// reads slowly slows the build down instead of growing the server's heap.
//
// Cancellation: a Send failure cancels the context, which unblocks the writer,
// which ends the copy, which ends the build call. The Job itself keeps running
// in the cluster — the executor returns on a done context before reaching the
// branch that deletes it — and BuildService.Build's proto comment says so,
// because "I cancelled the build" and "the build stopped" are different facts
// and a caller must not confuse them.
func runBuild(ctx context.Context, cancel context.CancelFunc, builder build.Builder, req build.Request, stream *connect.ServerStream[kelsonv1alpha1.BuildResponse]) (build.Result, error) {
	chunks := make(chan []byte, logChunkBuffer)
	type outcome struct {
		res build.Result
		err error
	}
	done := make(chan outcome, 1)

	go func() {
		defer close(chunks)
		// The scrub is here, on the server's side of the seam, so the guarantee
		// holds for every build.Builder rather than for the one that remembers
		// (issue #117). It wraps the chunker, not the stream, so a credential
		// split across two of the executor's writes is still caught; a scrubber
		// with nothing registered — the normal case under ADR-0009 — returns the
		// chunker unchanged and costs nothing.
		sink := &chunkWriter{ctx: ctx, out: chunks}
		logs := redact.Registered().Writer(sink)
		res, err := builder.Build(ctx, req, logs)
		if flusher, ok := logs.(*redact.ScrubWriter); ok {
			// The withheld tail is still owed to the client; a build's last line
			// is often the one that says what went wrong.
			if ferr := flusher.Flush(); ferr != nil && err == nil {
				err = ferr
			}
		}
		done <- outcome{res: res, err: err}
	}()

	var sendErr error
	for chunk := range chunks {
		if sendErr != nil {
			// Drain rather than break: the writer is blocked on this channel
			// and closing it is the builder goroutine's job, so leaving early
			// would leak both. The cancel below is what actually stops it.
			continue
		}
		if err := stream.Send(&kelsonv1alpha1.BuildResponse{
			Event: &kelsonv1alpha1.BuildResponse_Log_{
				Log: &kelsonv1alpha1.BuildResponse_Log{Chunk: chunk},
			},
		}); err != nil {
			sendErr = err
			cancel()
		}
	}

	out := <-done
	if sendErr != nil {
		// The client is gone or the transport broke. That is the failure worth
		// reporting, not whatever the aborted build said on its way out.
		return build.Result{}, sendErr
	}
	return out.res, out.err
}

// chunkWriter turns the builder's writes into bounded chunks on a channel.
//
// It copies every chunk because an io.Writer may reuse its buffer the moment
// Write returns (io.Copy does exactly that), and the bytes are still in flight
// on the channel. It watches ctx so a cancelled build does not leave the
// builder blocked forever on a reader that stopped reading.
type chunkWriter struct {
	ctx context.Context
	out chan<- []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), logChunkSize)
		chunk := make([]byte, n)
		copy(chunk, p[:n])
		select {
		case w.out <- chunk:
		case <-w.ctx.Done():
			return written, w.ctx.Err()
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// resolveRevision turns whatever the request (or spec.source.ref) says into the
// commit that is actually built, mirroring cmd/kelson's function of the same
// name.
//
// A ref that is already a commit costs no network call. Anything else is
// resolved once, here, so the image tag, the recorded revision, the Started
// event and the tree the build pod checks out all name the same commit — a
// branch resolved separately by each of them could disagree, and "built from
// main" is not a record of anything.
func resolveRevision(ctx context.Context, resolver RevisionResolver, repo, ref string) (string, error) {
	if build.IsCommit(ref) {
		return build.NormalizeCommit(ref), nil
	}
	if resolver == nil {
		return "", unavailable("api: this server cannot resolve git refs; ask for a full 40-character commit in `ref`")
	}
	return resolver.Resolve(ctx, repo, ref)
}
