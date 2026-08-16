package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview"
	"github.com/dafrie/kelson/internal/preview/naming"
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

// GitSourceLister reads the repositories the instance offers to every project —
// the GitSources of ADR-0035 decision 2, the global tier a component may bind
// to by name.
//
// It is a seam for the reason every cluster-facing capability in this package
// is one: a GitSource is a custom resource and this plane may not hold a
// Kubernetes client (.golangci.yml). It is narrower than the connection store's
// on purpose — there is no Get, no Create and no Delete, because no RPC in this
// schema authors one; an operator applies a GitSource with kubectl, exactly as
// ADR-0035 decision 2 describes, and kelson reads it.
//
// A nil one is a server with no global tier, which resolves every project
// against its own declared sources alone. That is the pre-ADR-0035 posture and
// stays correct: a project that declares what it builds from is unaffected by
// whether the instance offers anything.
type GitSourceLister interface {
	// ListSources returns every GitSource the instance offers, already in the
	// shape [model.Resolve] takes (model.GitSource.AsSource).
	ListSources(ctx context.Context) ([]model.Source, error)
}

// globalSources reads the instance's tier for one request.
//
// A failure is reported rather than swallowed: a build that silently resolved
// against an empty global tier would refuse a component bound to a GitSource
// with `ref/unknown-source` — "this instance offers: nothing" — which is a
// truthful sentence about a lookup that failed and a false one about the
// instance. The two are told apart here or not at all.
func (s *Server) globalSources(ctx context.Context) ([]model.Source, error) {
	if s.gitSources == nil {
		return nil, nil
	}
	sources, err := s.gitSources.ListSources(ctx)
	if err != nil {
		return nil, unavailable("api: listing the instance's GitSources: %w", err)
	}
	return sources, nil
}

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

	// The instance's GitSources, read before the spec is resolved because a
	// component may bind to one by name and resolution is what turns that name
	// into a repository (ADR-0035 decisions 2 and 3). A server with no source
	// store resolves against an empty global tier, which is what every caller
	// did before this existed and is still correct for a project that declares
	// its own sources.
	globals, err := s.globalSources(ctx)
	if err != nil {
		return failRequest(err)
	}

	// resolve, not renderSpec: a build reads the component's source binding,
	// spec.build and the Environment's identity, and nothing a ClusterProfile
	// decides. Rendering would additionally demand an image for the very spec
	// that has none yet, which is the spec a build exists to serve (#136).
	project, environment, resolved, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "", globals...)
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

	// Which repository this build clones is the components' answer now, not the
	// Project's (ADR-0035 decision 4). The refusals it can return — nothing
	// bound, or bound to several — are the shared plan's, so this handler and
	// `kelson build` name them identically (internal/build/binding.go).
	binding, err := build.SourceToBuild(project.Metadata.Name, resolved)
	if err != nil {
		return failRequest(err)
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
		// The connection travels with the target because the ref resolver is
		// built there and must read this repository as the same identity the
		// clone will (ADR-0033 decisions 4 and 5). It is the *source's*
		// connection now rather than the project's, which is the whole of what
		// ADR-0035 changes about credential resolution.
		SourceConnection: binding.Source.Connection,
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

	// One ls-remote, against the bound source's repository and its ref: the ref
	// is per source (ADR-0035 decision 1), and the request's `ref` overrides it
	// for this build alone.
	revision, err := resolveRevision(ctx, plane.Revisions, binding.Source.Git, binding.SourceRef(msg.GetRef()))
	if err != nil {
		return failRequest(err)
	}

	request := build.Request{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		// Component stays empty: one build serves every component bound to this
		// source — one clone, shared — so naming one of them would put a false
		// label on the Job and on the image (build.DestinationTag says the same
		// thing).
		SourceGit:        binding.Source.Git,
		SourceRef:        revision,
		SourceName:       binding.Source.Name,
		SourceConnection: binding.Source.Connection,
		Dockerfile:       build.DockerfilePath(project.Spec.Build),
		Image:            image,
		Tag:              build.DestinationTag(project.Metadata.Name, revision),
		Revision:         revision,
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

// ReportBuild is the CI hand-off of [ADR-0034](docs/adr/0034-forge-driven-delivery.md)
// decision 3: "I built the image, you take it from here".
//
// # Why every check runs before anything happens
//
// The schema is the contract CI is written against, and a report with a
// truncated SHA, a tag instead of a digest or a component the Project never
// declared is a pipeline bug whose only cheap moment to learn about is the
// first run. So the report is judged in full — InvalidArgument with a
// `report/*` code naming the field — before a single artifact is packaged, and
// nothing below that line runs for a request kelson would not have acted on.
//
// # One half of the pipeline is live: change requests
//
// A report with `pr > 0` publishes. Every environment of the project that
// declares `previews:` for the repository the report is about gets the parent
// environment's render, into the change request's own namespace, with the
// reported digests pinned, packaged as the Flux OCI artifact ADR-0017 decision
// 10 specifies and pushed under the head commit — the tag the rendered
// `OCIRepository` already asks for, which is why the preview moves the moment
// the artifact lands. Then flux-operator is asked to re-poll now
// (ADR-0034 decision 2) so the change request appears at once rather than at
// `previews.interval`.
//
// A report with `pr == 0` is the tracking-environment half (`autoDeploy`,
// ADR-0034 decision 4). That field does not exist in the model yet, so a branch
// report is refused rather than accepted and dropped — for the reason this
// method has always refused: `accepted: true` with nothing triggered is a lie a
// pipeline would believe, and ADR-0034 names "why didn't my preview update" as
// the question this whole path must stay answerable for.
//
// # `accepted` is not "it worked"
//
// The schema pins `accepted: false` to exactly one meaning — a report kelson
// understood and declined to act on, which is a project whose images come from
// kelson's own build plane (see [declineReport]). Everything else is accepted,
// including a report that matched no environment: "a report for a ref no
// environment follows is recorded and triggers nothing, which is exactly what a
// report for a feature branch should do".
//
// A publish that *fails* is a third thing, and it is neither. Where at least
// one preview was published, the failures ride in `message` beside what
// succeeded — partial publication is a real outcome and hiding half of it would
// be the silence the field exists to prevent. Where kelson had environments to
// publish into and published none, the RPC fails, because that is precisely the
// "green pipeline that published nothing" this method has always refused to be.
//
// # Idempotency: the artifact is the record
//
// A replayed report re-derives byte-identical blobs (ADR-0017 decision 10's
// determinism, applied to the same spec, profile, change request, commit and
// images), so the registry is asked to store what it already holds and the
// second answer is the first answer. That is what an idempotency key promises,
// and it is why there is no replay cache here: a cache would be a second
// authority on "has this been published" that could disagree with the registry.
// The key is still recorded on the audit entry, so a retry and the report it
// repeats are visibly the same one (#71).
//
// A second report for one commit with *different* images is not a replay. It
// republishes, and the SHA tag then resolves to the newer artifact — deliberate,
// because the tag names the commit and the artifact says what runs at it.
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
	spec, err := s.resolveSpec(ctx, storedSpec(msg.GetProject()))
	if err != nil {
		return nil, failRequest(err)
	}
	if err := checkReportedComponents(spec.project, msg); err != nil {
		return nil, failRequest(err)
	}
	if declined := declineReport(spec.project); declined != "" {
		return connect.NewResponse(&kelsonv1alpha1.ReportBuildResponse{Message: declined}), nil
	}

	if msg.GetPr() <= 0 {
		return s.deployReportedRef(ctx, spec, msg)
	}
	return s.publishReportedPreviews(ctx, spec, msg)
}

// storedSpec is the SpecRef for a project the server holds. ReportBuild has no
// inline-documents form — a report triggers a render of what the *server* holds,
// so a spec supplied in the request would render something no environment is
// deployed from — but the pipeline behind resolveSpec is the one every other
// handler runs, and reaching around it would be a second decode.
func storedSpec(project string) *kelsonv1alpha1.SpecRef {
	return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: project}}
}

// declineReport answers "would kelson's own build plane have produced these
// images?", and returns the sentence to decline with when it would.
//
// This is the `build.by` decision (ADR-0034 decision 3), and the schema fixes
// its shape rather than leaving it open: `accepted: false` is documented as
// "a report kelson understood and declined to act on — a project whose
// `build.by` is `kelson`, so its images come from kelson's own build plane and a
// CI report would be publishing over it". So it is not a `report/*` refusal and
// not an error code; it is an ordinary response that says no.
//
// The interesting case is the unset field. ADR-0034 spells the default
// carefully — "`by: kelson` (default for projects with `source:`)" — and the
// qualifier is load-bearing. A project with no source has no kelson build plane
// at all: `BuildService.Build` refuses it with `build/no-source`, and so does a
// `strategy: none` project. Declining *its* report would state a reason that is
// false — nothing of kelson's produces those images — and a report is the only
// way such a project's previews can ever run the change. So the decline fires
// where kelson would actually build, and the default is read as the ADR wrote
// it rather than as "empty means kelson, always".
//
// An explicit `by: kelson` declines regardless of whether kelson *could* build
// it. The author stated who owns image production; a spec that names an owner
// with no source is a spec to fix, and answering it with a silent publish would
// hide that.
func declineReport(p *model.Project) string {
	explicit := p.Spec.Build != nil && p.Spec.Build.By != ""
	if explicit && p.Spec.Build.By != model.BuildByKelson {
		return ""
	}
	if !explicit && !kelsonBuildsImages(p) {
		return ""
	}

	how := "declares no spec.build.by, and a project that builds from source defaults to `" + model.BuildByKelson + "`"
	if explicit {
		how = "declares spec.build.by: " + model.BuildByKelson
	}
	return fmt.Sprintf("Project %s %s, so kelson's own build plane produces its images and publishing this "+
		"report would publish over it. Set spec.build.by: %s to hand image production to your pipeline — that is "+
		"the whole difference between the two postures, and kelson acts on a report only for the second.",
		p.Metadata.Name, how, model.BuildByCI)
}

// kelsonBuildsImages reports whether kelson's build plane would produce this
// project's images if asked. It is the same condition validate.go reads to
// decide that a component's image is "(built from source)", spelled here
// because that one is not exported and this one is a different question about
// the same two fields.
func kelsonBuildsImages(p *model.Project) bool {
	if len(projectRepositories(p)) == 0 {
		return false
	}
	return p.Spec.Build == nil || p.Spec.Build.Strategy != model.BuildNone
}

// projectRepositories are the repositories a Project declares, in spec order
// and without duplicates: the singular `source:`, the plural `sources:`, or
// neither.
//
// It reads the declarations rather than the bindings because both of its
// callers ask about the *project* — would kelson build this at all, and is this
// report about a repository the project reads — and a resolution needs an
// Environment neither of them has. ADR-0035 decision 4 says the candidate
// repositories for a project are the union of its sources, and this is that
// union; a declared source no component binds to is still a repository this
// project is about.
func projectRepositories(p *model.Project) []string {
	if p == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, src := range p.Spec.EffectiveSources() {
		git := strings.TrimSpace(src.Git)
		if git == "" || seen[git] {
			continue
		}
		seen[git] = true
		out = append(out, git)
	}
	return out
}

// publishReportedPreviews is the live half: render and publish this change
// request's preview into every environment that previews the repository the
// report is about, then ask flux-operator to look now.
//
// Environments are walked in the order [decodeSpec] produced them, which is
// sorted by name, so a report into two environments answers with the same list
// and the same message every time. A refusal that varied with Go's map order
// would be one nobody could reproduce.
func (s *Server) publishReportedPreviews(ctx context.Context, spec decoded, msg *kelsonv1alpha1.ReportBuildRequest) (*connect.Response[kelsonv1alpha1.ReportBuildResponse], error) {
	if s.publish == nil {
		return nil, unimplemented("server-side preview publishing")
	}
	// No ProfileRef on this message and none it could carry: a report says what
	// CI knows, and which cluster the preview is judged against is the server's
	// (resolveProfile's "an absent ProfileRef means the cluster you are
	// attached to"). It is captured once for every environment in the report,
	// so two previews of one commit are never judged against two clusters.
	profile, err := s.resolveProfile(ctx, nil)
	if err != nil {
		return nil, failRequest(err)
	}

	pr := strconv.FormatInt(int64(msg.GetPr()), 10)
	sha := build.NormalizeCommit(msg.GetSha())

	var (
		triggered  []string
		references []string
		notes      []string
		causes     []error
		previews   []publishedPreview
		candidates int
	)
	for _, env := range spec.environments {
		if env.Spec.Previews == nil {
			continue
		}
		if note := previewsElsewhere(spec.project, env); note != "" {
			notes = append(notes, note)
			continue
		}
		candidates++
		if note := reportOverlays(spec.project, env); note != "" {
			notes = append(notes, note)
			causes = append(causes, errors.New(note))
			continue
		}

		published, err := s.publish.Publish(ctx, preview.Options{
			Project:     spec.project,
			Environment: env,
			PR:          pr,
			SHA:         sha,
			Images:      msg.GetImages(),
			Profile:     profile,
		})
		if err != nil {
			notes = append(notes, "environment "+env.Metadata.Name+" published nothing: "+err.Error())
			causes = append(causes, err)
			continue
		}
		// The identifier is the preview's own name, `<project>-<environment>-pr<id>`
		// (ADR-0017 decision 3), and not the bare `pr<number>` the field's
		// comment sketches. One repository may be previewed by several
		// environments — nothing in the spec forbids it and `previews.repo` is
		// authored per environment — and two entries reading `pr412` would name
		// two different namespaces with one string. This is also the identifier
		// PreviewService reports for the same object, so a caller that reads
		// `triggered` and then asks what became of it is asking about the same
		// thing.
		triggered = append(triggered, published.Set.Namespace)
		references = append(references, published.Reference)
		previews = append(previews, publishedPreview{
			environment: env.Metadata.Name,
			repo:        strings.TrimSpace(env.Spec.Previews.Repo),
			published:   published,
		})
		if note := s.pokeProvider(ctx, spec.project.Metadata.Name, env.Metadata.Name, published.Set.ParentNamespace); note != "" {
			notes = append(notes, note)
		}
	}

	if candidates > 0 && len(triggered) == 0 {
		// Nothing was published where something was meant to be. See the method
		// comment: this is the one outcome an empty-but-cheerful response would
		// misreport, so it is the one that fails.
		//
		// FailedPrecondition, not InvalidArgument: the report is well-formed and
		// no edit to it would help — a registry that refused the push, a spec
		// that cannot be rendered server-side and a cluster that would not
		// answer are all the world refusing a correct request, which is the
		// same distinction failSecret draws. Whatever taxonomy the causes carry
		// rides along in the details.
		return nil, fail(connect.CodeFailedPrecondition, fmt.Errorf(
			"api: the report for %s at %s published no preview: %w",
			spec.project.Metadata.Name, sha, errors.Join(causes...)))
	}

	if note := s.reportPreviewStatus(ctx, spec, sha, previews, causes); note != "" {
		notes = append(notes, note)
	}
	notes = append(notes, s.commentPreviews(ctx, int(msg.GetPr()), previews)...)
	if len(references) > 0 {
		// What this call produced, in the audit trail's own vocabulary: the
		// artifacts, against the commit they were rendered for (#78).
		auditChange(ctx, controlstore.AuditChange{Revision: strings.Join(references, " "), From: sha})
	}
	return connect.NewResponse(&kelsonv1alpha1.ReportBuildResponse{
		Accepted:  true,
		Triggered: triggered,
		Message:   reportMessage(spec, pr, triggered, notes),
	}), nil
}

// deployReportedRef is the tracking half of the report (ADR-0036 decision 3):
// the environments that follow the ref these images were built from get them,
// through the one trigger the spine has — a spec write (autodeploy.go's method
// comment argues why it is not a publish).
//
// # What `accepted` means here, unchanged
//
// A report that matched no environment is accepted. The schema pins that
// meaning — "a report for a ref no environment follows is recorded and triggers
// nothing, which is exactly what a report for a feature branch should do" — and
// the tracking half is where the sentence is most load-bearing: most branches of
// most repositories are followed by nothing, and a red pipeline for each of them
// would train a team to ignore the field.
//
// A *refusal* is different and is not this. `build/several-sources` is a
// kelson-built project's refusal (#252) and cannot be reached from here at all:
// [declineReport] has already turned every `by: kelson` project away, and a
// `by: ci` project reporting per-component images is precisely what that refusal
// points at.
//
// # A write that failed fails the RPC
//
// Where kelson had environments to move and moved none because the store refused
// the write, the RPC fails — the same rule the preview half keeps, and for the
// same reason: `accepted: true` with nothing triggered is a lie a pipeline would
// believe.
func (s *Server) deployReportedRef(ctx context.Context, spec decoded, msg *kelsonv1alpha1.ReportBuildRequest) (*connect.Response[kelsonv1alpha1.ReportBuildResponse], error) {
	ref := ShortRef(msg.GetRef())
	if ref == "" {
		// The proto says so on the field: "Empty reports the images and triggers
		// no environment." There is nothing to compare a source's `ref:` against,
		// so this is a well-formed report with no target rather than a mistake.
		return connect.NewResponse(&kelsonv1alpha1.ReportBuildResponse{
			Accepted: true,
			Message: redact.Scrub(fmt.Sprintf("recorded the images for %s and triggered nothing: the report names no "+
				"ref and no change request, and an environment follows a ref. Send --ref (the branch or tag CI built) "+
				"or --pr (the change request to preview)", build.NormalizeCommit(msg.GetSha()))),
		}), nil
	}

	sha := build.NormalizeCommit(msg.GetSha())
	trigger := PushTrigger{
		Project: spec.project.Metadata.Name,
		// No repository: CI reports what it built, not where from. See
		// [PushTrigger.Repo].
		Ref:    ref,
		SHA:    sha,
		Images: msg.GetImages(),
	}
	// The documents are read again rather than carried down from
	// [Server.resolveSpec], which decodes and discards the bytes: the write is a
	// read-modify-write and the version it asserts has to be the one it read
	// (the rule Promote states at its own Get). It is one more read of a store
	// this handler has already reached.
	stored, err := s.specs.Get(ctx, msg.GetProject())
	if err != nil {
		return nil, failRequest(err)
	}
	out, err := s.applyTrigger(ctx, stored, spec, trigger)
	if err != nil {
		return nil, failRequest(err)
	}

	notes := out.Notes
	if note := s.reportDeployStatus(ctx, trigger.repositories(spec.project), trigger, out); note != "" {
		notes = append(notes, note)
	}
	if out.moved() {
		// The audit record's "what did this produce?" is the environments the
		// write set in motion, against the commit they were moved to (#78). The
		// delivery revision is the controller's to assign and is deliberately not
		// invented here.
		auditChange(ctx, controlstore.AuditChange{
			Revision: strings.Join(out.Triggered, " "),
			From:     sha,
		})
	}
	return connect.NewResponse(&kelsonv1alpha1.ReportBuildResponse{
		Accepted:  true,
		Triggered: out.Triggered,
		Message:   deployedMessage(spec, ref, out, notes),
	}), nil
}

// deployedMessage is the prose half of the tracking answer: why `triggered` is
// what it is.
//
// It is scrubbed on the way out for [reportMessage]'s reason: it is free text
// assembled from values kelson resolved — image references among them — on a
// *successful* response, which never reaches the error boundary where scrubbing
// otherwise happens (issue #117).
func deployedMessage(spec decoded, ref string, out PushOutcome, notes []string) string {
	var clauses []string
	switch {
	case len(out.Triggered) == 1:
		env := out.Triggered[0]
		clauses = append(clauses, fmt.Sprintf("moved %s in environment %s at %s",
			strings.Join(out.Components[env], ", "), env, ref))
	case len(out.Triggered) > 1:
		clauses = append(clauses, fmt.Sprintf("moved %d environments at %s: %s",
			len(out.Triggered), ref, strings.Join(out.Triggered, ", ")))
	case len(notes) > 0:
		clauses = append(clauses, "deployed nothing at "+ref)
	default:
		clauses = append(clauses, fmt.Sprintf("deployed nothing at %s: no component of project %s follows %s here — "+
			"a component follows its source when the environment (or the component) sets autoDeploy: true and the "+
			"source it binds is this repository at this ref",
			ref, spec.project.Metadata.Name, ref))
	}
	return redact.Scrub(strings.Join(append(clauses, notes...), "; "))
}

// previewsElsewhere reports an environment whose previews are about a different
// repository than the one this report came from, as the sentence to say so.
//
// `previews.repo` is the *source* repository — whose pull requests become
// previews — and ADR-0017 decision 1 keeps it separate from the delivery
// repository with no defaulting between them, precisely so an author can point
// it somewhere the project's own `source:` does not name. A report is about the
// commit CI built, which is a commit in the project's source; an environment
// previewing another repository is not a target of it, and saying which one it
// previews is what keeps "why didn't my preview update" answerable.
//
// A project that declares no source is the one case with nothing to compare:
// kelson never learns where its images come from, the author's `previews.repo`
// is the only statement about where these pull requests live, and CI reported
// for this project by name. That is the ordinary shape of a `by: ci` project, so
// it matches rather than being skipped for a mismatch nobody stated.
//
// A project declaring several sources is compared against all of them, which is
// ADR-0035 decision 4 in one line: "candidate repositories for a project are the
// union of its bound sources' repositories". A two-repository project whose
// previews follow the second one is previewing its own code, and refusing that
// because it is not the *first* source would make a list order a decision.
func previewsElsewhere(p *model.Project, env *model.Environment) string {
	repositories := projectRepositories(p)
	if len(repositories) == 0 {
		return ""
	}
	for _, source := range repositories {
		if model.SameRepository(env.Spec.Previews.Repo, source) {
			return ""
		}
	}
	return fmt.Sprintf("environment %s previews %s, which is none of this project's sources (%s), so this report "+
		"is not about its change requests", env.Metadata.Name, display(env.Spec.Previews.Repo),
		strings.Join(repositories, ", "))
}

// reportOverlays refuses to publish a preview of a spec that carries overlays,
// as the sentence to say so.
//
// It is [checkOverlays]' rule at a different moment: an overlay path is relative
// to the authoring documents and a stored spec has no authoring directory, so
// there is nothing to resolve it against. Rendering anyway would publish an
// artifact silently missing what the author asked for — which is worse than the
// refusal, because the preview would come up and be wrong.
func reportOverlays(p *model.Project, env *model.Environment) string {
	if len(p.Spec.Overlays)+len(env.Spec.Overlays) == 0 {
		return ""
	}
	return "environment " + env.Metadata.Name + " published nothing: its spec carries overlays, whose paths " +
		"resolve against the files they were authored beside, and a stored spec has no such directory. Publish " +
		"this preview with `kelson preview publish` from a checkout, or move the overlay into the spec"
}

// pokeProvider asks flux-operator to re-poll the environment's
// ResourceSetInputProvider (ADR-0034 decision 2), and returns the sentence to
// report a failure with.
//
// The object's address is derived rather than looked up, from the same function
// the renderer names it with: kelson created it by applying its own rendered
// manifests, so re-deriving the address is reading the contract rather than
// guessing at it. An environment whose previews have never been applied has no
// object, and the poke fails with NotFound — reported, never fatal, because
// polling stays as configured and losing a poke costs latency alone.
func (s *Server) pokeProvider(ctx context.Context, project, environment, namespace string) string {
	if s.poke == nil {
		return ""
	}
	name := naming.Lifecycle(project, environment)
	if err := s.poke.Poke(ctx, namespace, name); err != nil {
		return fmt.Sprintf("published, but flux-operator was not asked to look now (%s/%s: %s), so this preview "+
			"appears at the environment's previews.interval instead", namespace, name, err.Error())
	}
	return ""
}

// reportPreviewStatus writes the publish back onto the commit (ADR-0034
// decision 5), and returns the sentence to report a *failure* to do so with.
//
// Absence degrades to nothing and says nothing: no reporter, no connection, or
// a connection whose provider has no StatusReporter are all "statuses are a
// courtesy of the integration, not a delivery dependency". A reporter that was
// asked and failed is different — something is configured and broken — so it is
// named in the report's message.
//
// The state is terminal rather than `pending`, and that is a choice worth
// stating. `pending` would describe the truth more finely (the artifact is
// published; the cluster has not applied it yet), but nothing here would ever
// resolve it: what happens next is flux-operator's, and a required check stuck
// pending forever blocks merges. So the status answers the question kelson can
// answer — did the preview publish — and what became of it afterwards is the
// preview surface's to report.
func (s *Server) reportPreviewStatus(ctx context.Context, spec decoded, sha string, published []publishedPreview, causes []error) string {
	if s.outcomes == nil || len(published) == 0 {
		return ""
	}
	triggered := make([]string, len(published))
	for i, p := range published {
		triggered[i] = p.published.Set.Namespace
	}
	state, description := "success", fmt.Sprintf("published %s", strings.Join(triggered, ", "))
	if len(causes) > 0 {
		state = "failure"
		description = fmt.Sprintf("published %s; %d environment(s) failed", strings.Join(triggered, ", "), len(causes))
	}

	var failures []string
	for _, repo := range previewRepositories(spec) {
		_, fullName, ok := splitRepository(repo)
		if !ok {
			// Unreachable for a validated spec — `previews.repo` is a URL the
			// model checks — and silent rather than reported if it ever is: a
			// status is a courtesy, and nothing about what was published
			// changes.
			continue
		}
		err := s.outcomes.ReportCommitStatus(ctx, CommitStatus{
			Repo:     repo,
			FullName: fullName,
			SHA:      sha,
			// One context for every preview publish of one commit, because a
			// forge keys statuses by it: a per-environment context would leave
			// a check per environment on every commit, and a changing one
			// leaves a graveyard of stale checks instead of an answer.
			Context:     PreviewStatusContext,
			State:       state,
			Description: description,
			Path:        statusPath(spec.project.Metadata.Name, published, repo),
		})
		if err != nil {
			failures = append(failures, display(repo)+": "+err.Error())
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "the commit status could not be written back (" + strings.Join(failures, "; ") + "), which changes " +
		"nothing about what was published"
}

// PreviewStatusContext is the check name a preview publish appears under on a
// commit. It is exported because it is a contract with whoever configures
// branch protection: a required check is named by this string, and changing it
// silently makes an existing rule match nothing.
const PreviewStatusContext = "kelson/preview"

// publishedPreview is one environment's successful publish, kept because the
// two write-backs of ADR-0034 decision 5 speak about the preview rather than
// about the report: the status links a preview's own page and the comment names
// that preview's hosts, and neither is derivable from the namespace string the
// response carries.
type publishedPreview struct {
	environment string
	// repo is `previews.repo` as that environment spells it — the repository
	// the comment is written on and the status is written to.
	repo      string
	published *preview.Published
}

// previewPath is the UI route one preview's detail page lives at.
//
// The id is the change request number as the forge numbers it, unencoded: it is
// the same value `Preview.id` carries on the wire, and a preview's identity is
// validated to digits (naming.ValidateID) before anything is ever published
// under it, so there is nothing here for an escape to protect.
func previewPath(project, environment, id string) string {
	return "/projects/" + project + "/" + environment + "/previews/" + id
}

// statusPath is where one repository's check points: the detail page of the
// first preview published into it.
//
// The fallback to the project page is not decoration. [previewRepositories]
// lists what the *spec* previews, so a repository can appear here with nothing
// published into it — an environment that failed while another succeeded — and
// that repository has no preview page worth linking. Publishes are appended in
// the environment order [decodeSpec] produced, which is sorted by name, so two
// environments previewing one repository link the same page on every call
// rather than whichever the map happened to yield.
func statusPath(project string, published []publishedPreview, repo string) string {
	for _, p := range published {
		if model.SameRepository(p.repo, repo) {
			return previewPath(project, p.environment, p.published.Set.PR)
		}
	}
	return "/projects/" + project
}

// commentPreviews upserts one comment per published preview onto its change
// request (ADR-0034 decision 5's second half), and returns the sentences to
// report failures with.
//
// One comment *per environment*, keyed by [previewCommentMarker], because two
// environments may preview one pull request: a marker naming only the project
// would make the second publish overwrite the first's answer, and the pull
// request would report on whichever environment published last.
//
// It degrades exactly as the status does — no reporter, no connection, no
// capability, nothing written and nothing said — and names a failure for the
// same reason: something configured is broken, and the publish stands either
// way.
func (s *Server) commentPreviews(ctx context.Context, pr int, published []publishedPreview) []string {
	if s.outcomes == nil || len(published) == 0 || pr <= 0 {
		return nil
	}
	var failures []string
	for _, p := range published {
		_, fullName, ok := splitRepository(p.repo)
		if !ok {
			// Unreachable for a validated spec, and silent if it ever is: see
			// [Server.reportPreviewStatus] for why a write-back this plane
			// cannot address is not worth a sentence.
			continue
		}
		marker := previewCommentMarker(p.published.Set.Project, p.environment)
		err := s.outcomes.UpsertPreviewComment(ctx, PreviewComment{
			Repo:     p.repo,
			FullName: fullName,
			PR:       pr,
			Marker:   marker,
			Body: previewCommentBody(marker, p, s.outcomes.Link(
				previewPath(p.published.Set.Project, p.environment, p.published.Set.PR))),
		})
		if err != nil {
			failures = append(failures, "the preview comment for environment "+p.environment+" could not be written ("+
				display(p.repo)+": "+err.Error()+"), which changes nothing about what was published")
		}
	}
	return failures
}

// previewCommentMarker identifies the one comment an environment keeps on a
// change request. It is an HTML comment because it must survive in the body a
// reader sees without being part of what they read — the forge has no "my
// comment on this pull request" lookup, so the identity lives in the text
// (internal/forge's UpsertPRComment).
func previewCommentMarker(project, environment string) string {
	return "<!-- kelson:preview:" + project + "-" + environment + " -->"
}

// previewCommentBody is what the change request reads, and it is deliberately
// short: hosts, what was published, and where the live answer is.
//
// Two things it does not claim. It does not report a rollout — the artifact is
// in the registry and flux-operator applies it next, which is the same reason
// [Server.reportPreviewStatus] writes a terminal state rather than a `pending`
// nobody would ever resolve — and it does not pretend to be live: it is rewritten
// by the next publish, not by the cluster, so the phase it states is the phase
// at publish time and it says where the current one is.
//
// The body is scrubbed of known credentials for the reason [reportMessage] is:
// it is free text assembled from values kelson resolved (a registry reference
// among them), and it leaves the process rather than merely being logged.
func previewCommentBody(marker string, p publishedPreview, link string) string {
	set := p.published.Set
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString("\n### kelson preview · ")
	b.WriteString(set.Project)
	b.WriteString(" / ")
	b.WriteString(p.environment)
	b.WriteString("\n\n")

	if len(set.Hosts) == 0 {
		b.WriteString("This preview serves no hostnames: no component of this environment declares `domains`.\n\n")
	} else {
		for _, host := range set.Hosts {
			b.WriteString("- [")
			b.WriteString(host)
			b.WriteString("](https://")
			b.WriteString(host)
			b.WriteString(")\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Published `")
	b.WriteString(set.SHA)
	b.WriteString("` as `")
	b.WriteString(p.published.Reference)
	b.WriteString("` into namespace `")
	b.WriteString(set.Namespace)
	b.WriteString("`.\n\nPhase at publish: **published** — the artifact exists and flux-operator applies it next. ")

	if link != "" {
		b.WriteString("The live phase, the applied revision and the logs are on the preview's page: ")
		b.WriteString(link)
		b.WriteString("\n")
	} else {
		// No external URL is configured, so there is no absolute link to give
		// and this says where to look instead rather than guessing an origin —
		// the same rule CommitStatus.Path keeps for the check.
		b.WriteString("The live phase is in your kelson UI under project ")
		b.WriteString(set.Project)
		b.WriteString(", environment ")
		b.WriteString(p.environment)
		b.WriteString(", preview ")
		b.WriteString(set.PR)
		b.WriteString(" (this server has no external URL configured, so there is no link to give).\n")
	}
	return redact.Scrub(b.String())
}

// previewRepositories are the distinct repositories the report's previews live
// in, sorted. It is normally one; it is a list because `previews.repo` is
// authored per environment and nothing says two environments of one project
// must agree.
func previewRepositories(spec decoded) []string {
	seen := map[string]bool{}
	for _, env := range spec.environments {
		if env.Spec.Previews == nil || previewsElsewhere(spec.project, env) != "" {
			continue
		}
		if repo := strings.TrimSpace(env.Spec.Previews.Repo); repo != "" {
			seen[repo] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// reportMessage is the prose half of the answer: why `triggered` is what it is.
//
// The schema asks for exactly this — "which environments matched, or that none
// did and what would have" — because a pipeline whose report silently did
// nothing is the failure mode the field exists to prevent. The empty case
// therefore says what is missing rather than saying nothing.
//
// It is the one wire surface in this package that carries plane error text on a
// *successful* response, so it goes through the known-value scrubber on the way
// out (issue #117). Errors are scrubbed in errors.go at their one boundary;
// this string never reaches that boundary, and a push refused by a registry is
// exactly the text a resolved credential could turn up in.
func reportMessage(spec decoded, pr string, triggered, notes []string) string {
	var clauses []string
	switch {
	case len(triggered) == 1:
		clauses = append(clauses, "published the preview for change request "+pr+": "+triggered[0])
	case len(triggered) > 1:
		clauses = append(clauses, fmt.Sprintf("published %d previews for change request %s: %s",
			len(triggered), pr, strings.Join(triggered, ", ")))
	case len(notes) > 0:
		clauses = append(clauses, "published nothing for change request "+pr)
	default:
		clauses = append(clauses, fmt.Sprintf("published nothing for change request %s: no environment of project %s "+
			"declares spec.previews, so this project has no change-request previews to publish. Add a previews "+
			"block to the environment whose pull requests should become previews",
			pr, spec.project.Metadata.Name))
	}
	return redact.Scrub(strings.Join(append(clauses, notes...), "; "))
}

// There is no sameRepository here any more.
//
// This package held a private copy of the comparison while there was nowhere
// else for it: whether two spec-shaped repository references name one
// repository, host and path both, because the path alone would match
// `acme/checkout` on gitlab.com against the same path on github.com.
// [model.SameRepository] is that function now, in the package where a
// `source.git` and a `previews.repo` are *defined*, and its own doc comment
// asks the next change that touches this file to collapse the copy onto it —
// which is this change (ADR-0036 decision 2 asks the same question of the same
// fields). internal/forgehttp's comparison stays its own: it compares a
// *delivery* against a spec and brings the connection's host into the answer.
//
// splitRepository below is not the same function and stays: what it produces is
// the `owner/repo` a forge API is keyed by, which is a projection rather than a
// comparison.

// splitRepository reduces a repository reference to its lowercased host and
// path. It accepts what a spec actually carries: an https URL, a bare
// host/path, and the `git@host:path` form `previews.repo` and `source.git` are
// both written in.
func splitRepository(raw string) (host, path string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(raw, "git@"); found {
		// scp-like syntax: host and path are separated by a colon, not a slash,
		// so url.Parse would read the path as a port.
		h, p, split := strings.Cut(rest, ":")
		if !split {
			return "", "", false
		}
		raw = "https://" + h + "/" + p
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	path = strings.ToLower(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"))
	if path == "" {
		return "", "", false
	}
	return strings.ToLower(u.Host), path, true
}

func display(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return "(unset)"
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
					"entirely (#51)")
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
func checkReportedComponents(project *model.Project, msg *kelsonv1alpha1.ReportBuildRequest) error {
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
