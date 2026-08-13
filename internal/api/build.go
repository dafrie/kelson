package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
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
	if s.build == nil {
		return unimplemented("in-cluster builds")
	}
	msg := req.Msg

	// resolve, not renderSpec: a build reads spec.source, spec.build and the
	// Environment's identity, and nothing a ClusterProfile decides. Rendering
	// would additionally demand an image for the very spec that has none yet,
	// which is the spec a build exists to serve (#136).
	project, environment, resolved, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return failRequest(err)
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
		// Application stays empty: one build serves the whole Project, so
		// naming one of its applications would put a false label on the Job
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
	return stream.Send(&kelsonv1alpha1.BuildResponse{
		Event: &kelsonv1alpha1.BuildResponse_Finished_{
			Finished: &kelsonv1alpha1.BuildResponse_Finished{
				Reference: res.Reference,
				Digest:    res.Digest,
			},
		},
	})
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
