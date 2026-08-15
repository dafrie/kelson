package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
)

// autoDeploy: the trigger half (ADR-0036 decision 3, issue #248).
//
// internal/model answers "what does this push make out of date"
// ([model.Resolved.StaleComponents]); this file answers "and then what". Both
// trigger paths land here — [Server.ReportBuild] with a ref and no PR, and the
// `push` delivery internal/forgehttp enqueues through [Server.AutoDeployPush] —
// because the two differ only in where the images come from. What follows the
// images is one pipeline, which is what ADR-0036 decision 3 means by "two
// trigger paths in, one pipeline out".
//
// # The trigger is a spec write, and that is the whole design decision here
//
// ADR-0036 decision 3 sketches this half as "render with the reported
// component-keyed pins, publish to the environments' artifacts, let Flux
// reconcile" — which is the preview path's shape, and the preview path really
// does publish from this process ([Server.publishReportedPreviews]).
//
// An *environment's* artifact cannot be published from here, and not for want
// of a client. Under [ADR-0028](docs/adr/0028-delivery-spine.md) a revision is
// the pair (`<generation>-<spec-hash>` tag, an OCIRepository pinned to it), and
// both halves are derived and applied by kelson-controller inside one reconcile
// (internal/controller's ArtifactRef and FluxDeliverer.ensure). A server that
// pushed an artifact would therefore either invent a tag nothing points at — an
// upload no cluster would ever pull — or write over a tag the controller
// considers immutable history. Either one is a second authority over what an
// environment is serving, which is exactly the thing ADR-0028 decision 1 exists
// to prevent, and it would show up as an environment whose `status.revision`
// and running pods disagreed.
//
// So the trigger is the one input that makes the controller re-render: the
// images are written into the environment's document and the document is
// stored. The store applies it as an `Environment` custom resource, which bumps
// `.metadata.generation`, which is a watch event, which is a reconcile — the
// identical mechanism `kelson deploy --image` uses (deploy.go's pinDeployImage)
// and `kelson promote` uses (promote.go's pinnedDocuments), through the same
// [promote.Pin] splice. Nothing here renders, packages or pushes; the plane that
// owns delivery still owns all of it.
//
// # The pin says who wrote it, which is what makes this tracking
//
// `Environment.spec.components[].image` is the only component-keyed image the
// model has, and [model.Resolved.ImagePins] used to count every one of them as a
// pin — so a component this trigger moved was, from the next push onwards, a
// component the stale set excluded. Auto-deploy moved each component once and
// then reported it as pinned, which was honest but was not tracking.
//
// ADR-0036 decision 5 closes that in the model rather than here: the pin carries
// `imageTracked: true`, a marked pin renders as any pin does but is left out of
// [model.Resolved.ImagePins], and the stale set may move it again. So this path
// writes [promote.Tracked] with every splice, and it still never overwrites an
// unmarked pin — not by checking, but because an unmarked pin keeps a component
// out of the stale set, which is the one list this path pins from. That
// component reaches [notMoved] instead and is named with the pinned reason,
// exactly as a person's pin has always been.
//
// The refusal is structural on purpose. A trigger that decided for itself which
// pins it was allowed to overwrite would be a second authority over rule P3;
// what it is allowed to overwrite is what the resolved spec says it is.

// PushTrigger is one push, reduced to what the stale set is asked about
// (ADR-0036 decision 2) plus what moves the components it names.
//
// Ref is the *short* name — `main`, `v1.2.3` — because that is the contract
// [model.Resolved.StaleComponents] states: the model parses no payloads, so the
// surface that read the delivery strips `refs/heads/` and `refs/tags/`
// ([ShortRef]).
type PushTrigger struct {
	// Project is the stored project this trigger is about. Both paths know it:
	// a report names it, and the webhook resolves it from the repository.
	Project string

	// Repo is the repository that moved, in any spelling [model.SameRepository]
	// accepts.
	//
	// Empty means "every repository this Project declares", which is the report
	// path: `ReportBuildRequest` carries a project, a commit and a ref, because
	// CI reports what it *built* rather than where it built it from, and the
	// commit is by construction a commit in one of the project's own sources
	// (ADR-0035 decision 4's union — the same union [previewsElsewhere] compares
	// a change request against).
	Repo string

	// Ref is the ref that moved, in either spelling. [Server.PlanPush] and
	// [Server.AutoDeployPush] reduce it with [ShortRef] on the way in, so a
	// caller across a seam hands over what the delivery said and exactly one
	// piece of code decides what `refs/heads/main` is.
	Ref string

	// SHA is the commit at the head of Ref, in full. It is the join key: a
	// report carries it, a push carries `after`, and the commit status is
	// written against it.
	SHA string

	// Images maps component name to a digest-pinned reference. A report carries
	// what CI built; the webhook path fills it in from the build kelson ran
	// itself. A stale component with no entry is named in the answer rather than
	// deployed with whatever it was running.
	Images map[string]string

	// Connection is the git connection whose webhook secret verified the
	// delivery that caused this, and it is the audit trail's principal for the
	// webhook path ([PrincipalSystem], ADR-0036 decision 4). Empty on the report
	// path, where the principal is the reporting agent and the interceptor has
	// already recorded it.
	Connection string
}

// PushOutcome is what one trigger did, in the vocabulary both callers report in.
type PushOutcome struct {
	// Triggered names the environments whose document was written, in spec
	// order. Each one is a reconcile the controller is about to run.
	Triggered []string

	// Components maps each triggered environment to the components that moved
	// in it, so a caller can say what changed rather than only where.
	Components map[string][]string

	// Notes are the sentences that explain the rest: a component the trigger was
	// asked about and did not move, an environment whose spec does not resolve,
	// a refusal that belongs to the project as a whole.
	Notes []string

	// Refused is set when the whole project was declined for a stated reason —
	// today only `build/several-sources` (#252). It is separate from Notes
	// because a caller reports it differently: it is a refusal, not a remark.
	Refused string
}

// moved reports whether anything was written.
func (o PushOutcome) moved() bool { return len(o.Triggered) > 0 }

// ShortRef reduces a ref to the short name [model.Resolved.StaleComponents]
// compares against: `refs/heads/main` and `refs/tags/v1.2.3` become `main` and
// `v1.2.3`, and a name that carries no namespace is already short.
//
// It is exported because two surfaces read a ref off a delivery — this plane
// from `ReportBuildRequest.ref`, cmd/kelson-server from a webhook payload on the
// way into [Server.AutoDeployPush] — and the model's contract is that exactly
// one answer to "what is the short name of this ref" reaches it. A ref under any
// other namespace (`refs/pull/412/head`, `refs/notes/commits`) is left alone:
// stripping it would make a pull request's head look like a branch called `412`,
// and no source's `ref:` names one, so it simply matches nothing.
func ShortRef(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if short, found := strings.CutPrefix(ref, prefix); found {
			return short
		}
	}
	return ref
}

// PushPlan is what a push *would* do, decided from the stored spec alone: no
// build, no write, no forge call.
//
// It exists because the two halves of handling a delivery have opposite
// deadlines. Deciding what a push moves is a decode and a resolve, which is
// microseconds; doing it can be a container build, which is minutes — and a
// forge gives a webhook ten seconds. So internal/forgehttp asks this
// synchronously, answers the delivery with it, and enqueues
// [Server.AutoDeployPush] behind it (ADR-0034 decision 1's "an event enqueues
// reconciliation" read literally).
//
// It is also where a refusal becomes visible at all. `build/several-sources` is
// a property of the stored Project, so it is known before anything runs, and
// this is the only surface that can carry it back to whoever pushed: nothing in
// this plane can write an Environment's conditions — that status subresource is
// kelson-controller's alone (internal/controller's patchStatus, and
// [EnvironmentStore] has Get, Watch and Annotate and no third verb).
type PushPlan struct {
	// Environments are the ones this push would move, in spec order, with the
	// components it would move in each.
	Environments map[string][]string

	// Refused is the project-wide refusal, or "" for a project a push may move.
	Refused string

	// Notes are the same sentences [PushOutcome] carries, decided from the spec:
	// an environment whose spec does not resolve, and nothing more, because a
	// plan knows no images to say what it could not move.
	Notes []string
}

// Moves reports whether this push has anything to do, which is what decides
// whether the delivery is worth enqueuing behind its answer.
func (p PushPlan) Moves() bool { return len(p.Environments) > 0 }

// PlanPush answers [PushPlan] for one push. It reads the stored spec and
// nothing else, and it writes nothing — the property internal/forgehttp depends
// on to call it inside a delivery's own deadline.
func (s *Server) PlanPush(ctx context.Context, t PushTrigger) (PushPlan, error) {
	if s.specs == nil {
		return PushPlan{}, unimplemented("the spec store")
	}
	t.Ref = ShortRef(t.Ref)
	stored, err := s.specs.Get(ctx, t.Project)
	if err != nil {
		return PushPlan{}, err
	}
	spec, err := decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	if err != nil {
		return PushPlan{}, err
	}
	if refusal := severalSources(spec.project); refusal != "" {
		return PushPlan{Refused: refusal}, nil
	}
	globals, err := s.globalSources(ctx)
	if err != nil {
		return PushPlan{}, err
	}

	plan := PushPlan{Environments: map[string][]string{}}
	repositories := t.repositories(spec.project)
	for _, env := range spec.environments {
		resolved, errs := model.Resolve(spec.project, env, globals...)
		if len(errs) > 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf("environment %s was not considered: its spec does not "+
				"resolve (%s)", env.Metadata.Name, errs.Error()))
			continue
		}
		if stale := staleFor(resolved, repositories, t.Ref); len(stale) > 0 {
			plan.Environments[env.Metadata.Name] = stale
		}
	}
	return plan, nil
}

// AutoDeployPush runs one push through the trigger pipeline: resolve every
// environment of the project, move what the push makes stale, and store the
// result once.
//
// It is exported because internal/forgehttp reaches it through a function seam
// that cmd/kelson-server wires (forgehttp.AutoDeployer). That package holds two
// cluster clients and this plane holds none by design, so neither imports the
// other and the binary is where they meet — the same shape
// [forgehttp.Authenticator] already has.
//
// It carries no agent policy guard, and that is deliberate rather than an
// omission: the caller is a verified forge delivery, not a principal. What
// governs it is the environment's own `autoDeploy` — an environment that has not
// opted in is not in the stale set, which is a stronger gate than a policy check
// because it is a property of the document rather than of the credential.
func (s *Server) AutoDeployPush(ctx context.Context, t PushTrigger) (PushOutcome, error) {
	if s.specs == nil {
		return PushOutcome{}, unimplemented("the spec store")
	}
	t.Ref = ShortRef(t.Ref)
	stored, err := s.specs.Get(ctx, t.Project)
	if err != nil {
		return PushOutcome{}, err
	}
	spec, err := decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	if err != nil {
		return PushOutcome{}, err
	}
	if refusal := severalSources(spec.project); refusal != "" {
		// #252: one build produces one image, so a kelson-built project reading
		// two repositories cannot be moved by a push to one of them without
		// quietly reusing the other's stale image. The refusal is the project's
		// and not one environment's, so nothing is resolved past it — and it is
		// written onto the commit, which is the one surface this plane has for
		// telling whoever pushed (see [PushPlan] for the conditions it cannot
		// reach).
		out := PushOutcome{Refused: refusal}
		if note := s.reportDeployStatus(ctx, t.repositories(spec.project), t, out); note != "" {
			out.Notes = append(out.Notes, note)
		}
		s.recordPush(ctx, t, out, nil)
		return out, nil
	}
	if t.Images == nil {
		built, note, err := s.buildPushedHead(ctx, spec, t)
		if err != nil {
			return PushOutcome{}, err
		}
		if note != "" {
			return PushOutcome{Notes: []string{note}}, nil
		}
		t.Images = built
	}
	out, err := s.applyTrigger(ctx, stored, spec, t)
	if err != nil {
		s.recordPush(ctx, t, PushOutcome{}, err)
		return PushOutcome{}, err
	}
	if note := s.reportDeployStatus(ctx, t.repositories(spec.project), t, out); note != "" {
		out.Notes = append(out.Notes, note)
	}
	s.recordPush(ctx, t, out, nil)
	return out, nil
}

// recordPush writes the webhook trigger's own audit record (ADR-0036 decision 4,
// ADR-0026).
//
// The authorization interceptor cannot do it, and that is the whole reason this
// exists: the interceptor records one record per *RPC*, and this path is not
// one — a verified delivery arrives at internal/forgehttp, outside the
// ConnectRPC handlers and outside the credential gate (the HMAC over the body is
// its gate). So the one mutation it causes would leave no trail at all, which is
// precisely the silence ADR-0026 §3 refuses.
//
// The principal is the connection, as [PrincipalSystem]. There is no scope,
// because a connection has none, and no reason, because nobody stated one.
//
// A push that moved nothing writes nothing, which is the rule
// [auditEntry.recordable] already states for every other path: an allowed read
// changed nothing, and a webhook that resolved an empty stale set is a read.
func (s *Server) recordPush(ctx context.Context, t PushTrigger, out PushOutcome, failure error) {
	if s.audit == nil || !s.audit.enabled() {
		return
	}
	if failure == nil && !out.moved() && out.Refused == "" {
		return
	}
	rec := controlstore.AuditRecord{
		Principal: controlstore.AuditPrincipal{Type: string(PrincipalSystem), Name: t.Connection},
		// The procedure is not an RPC path, and it is spelled so that it cannot
		// be mistaken for one: nothing in the schema serves it, and a trail
		// reader filtering by procedure must be able to tell a delivery from a
		// call.
		Procedure: "forge/webhook/push",
		Operation: controlstore.OpMutate,
		Target:    controlstore.AuditTarget{Project: t.Project},
		Outcome:   controlstore.AuditAllowed,
	}
	switch {
	case failure != nil:
		rec.Outcome = controlstore.AuditFailed
		rec.Code, rec.Message = failureCode(failure)
	case out.Refused != "":
		rec.Outcome = controlstore.AuditRefused
		rec.Code, rec.Message = "build/several-sources", out.Refused
	default:
		rec.Change = &controlstore.AuditChange{
			Revision: strings.Join(out.Triggered, " "),
			From:     t.SHA,
		}
	}
	s.audit.write(ctx, rec)
}

// applyTrigger is the pipeline both paths share: resolve each environment,
// splice the pins the stale set asks for, and write the document set once.
//
// Environments are walked in the order [decodeSpec] produced them — sorted by
// name — so two environments moved by one push are answered in the same order
// every time, and a note that varied with Go's map order would be one nobody
// could reproduce.
//
// One write, not one per environment: the store's unit is the project's whole
// document set, and a write per environment would make a two-environment push
// two revisions of the same spec with a version conflict waiting between them.
func (s *Server) applyTrigger(ctx context.Context, stored controlstore.Stored, spec decoded, t PushTrigger) (PushOutcome, error) {
	globals, err := s.globalSources(ctx)
	if err != nil {
		return PushOutcome{}, err
	}

	out := PushOutcome{Components: map[string][]string{}}
	docs := copyDocuments(stored.Documents)
	repositories := t.repositories(spec.project)
	for _, env := range spec.environments {
		resolved, errs := model.Resolve(spec.project, env, globals...)
		if len(errs) > 0 {
			// A spec that does not resolve is not a spec this push can move, and
			// it is not this push's fault either. It is named rather than
			// skipped: "why did staging not follow main" must stay answerable.
			out.Notes = append(out.Notes, fmt.Sprintf("environment %s was not moved: its spec does not resolve (%s)",
				env.Metadata.Name, errs.Error()))
			continue
		}

		stale := staleFor(resolved, repositories, t.Ref)
		out.Notes = append(out.Notes, notMoved(resolved, env.Metadata.Name, repositories, t, stale)...)
		pins := pinsFor(t.Images, stale)
		if len(pins) == 0 {
			// The ordinary case, and it is silent by design (ADR-0036 decision
			// 2): a push to a repository an environment happens to build from is
			// not news. Only a component this trigger was *asked* about produces
			// a note, and notMoved above has already written those.
			continue
		}

		key, doc, err := environmentDocument(docs, env.Metadata.Name)
		if err != nil {
			return PushOutcome{}, err
		}
		for _, name := range pinsOrder(stale, pins) {
			// Marked, always (ADR-0036 decision 5). The pin this writes is a
			// record of where the component is, not a decision to hold it there,
			// and the marker is the only thing that tells the next push's stale
			// set which of the two it is looking at.
			if doc, err = promote.Pin(doc, env.Metadata.Name, name, pins[name], promote.Tracked()); err != nil {
				return PushOutcome{}, err
			}
		}
		docs.Environments[key] = doc
		out.Triggered = append(out.Triggered, env.Metadata.Name)
		out.Components[env.Metadata.Name] = pinsOrder(stale, pins)
	}

	if !out.moved() {
		return out, nil
	}
	// The version asserted is the one read at the top of this trigger, which is
	// the same read-modify-write Promote performs and for the same reason: the
	// alternative to asserting a version is a blind overwrite, and a spec that
	// changed while a build was running is exactly when one would happen.
	if _, err := s.specs.Put(ctx, stored.Project, docs, controlstore.PutOptions{
		ExpectedVersion: stored.Version,
	}); err != nil {
		return PushOutcome{}, err
	}
	return out, nil
}

// repositories is what the stale set is asked about: the one repository a
// webhook named, or every repository the Project declares when the trigger names
// none (see [PushTrigger.Repo]).
func (t PushTrigger) repositories(p *model.Project) []string {
	if repo := strings.TrimSpace(t.Repo); repo != "" {
		return []string{repo}
	}
	return projectRepositories(p)
}

// staleFor is [model.Resolved.StaleComponents] over however many repositories
// this trigger is about, unioned in spec order.
//
// The union is not a widening of the model's rule: each repository is asked
// separately, so a component is in the answer only because *its own* binding
// matched one of them. What the union adds is that a project reading two
// repositories can be reported against without CI having to say which of the two
// this commit came from — which is a fact its images already carry.
func staleFor(resolved *model.Resolved, repositories []string, ref string) []string {
	if len(repositories) == 1 {
		return resolved.StaleComponents(repositories[0], ref)
	}
	seen := map[string]bool{}
	var stale []string
	for _, c := range resolved.Components {
		for _, repo := range repositories {
			if seen[c.Name] {
				break
			}
			for _, name := range resolved.StaleComponents(repo, ref) {
				if name == c.Name {
					seen[c.Name] = true
					stale = append(stale, c.Name)
					break
				}
			}
		}
	}
	return stale
}

// pinsFor is the images the stale set actually has, keyed by component. A stale
// component the trigger holds no image for is left out here and named by
// [notMoved]: deploying it would mean re-pinning it to what it already runs,
// which is a write with nothing behind it.
func pinsFor(images map[string]string, stale []string) map[string]string {
	out := make(map[string]string, len(stale))
	for _, name := range stale {
		if ref := strings.TrimSpace(images[name]); ref != "" {
			out[name] = ref
		}
	}
	return out
}

// pinsOrder is the components to pin in spec order, which is the order the stale
// set arrives in. A map range would splice the same document in two different
// orders and produce two different byte sequences for one push.
func pinsOrder(stale []string, pins map[string]string) []string {
	out := make([]string, 0, len(pins))
	for _, name := range stale {
		if _, ok := pins[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// notMoved names every component this trigger was asked about and did not move
// in this environment, with the reason.
//
// ADR-0036 decision 3 asks for exactly this — "components reported but not stale
// (not bound, not tracking, pinned) are named in the response message, not
// silently deployed" — and it is why [model.Resolved] keeps `AutoDeploy` and
// `ImagePins` beside the merged answer: one resolved image string cannot say
// whether a component held still because nobody tracks it or because somebody
// pinned it, and those two have opposite fixes.
//
// Only the components the trigger *names* are considered. An environment full of
// components nobody built is not news, and listing them would bury the one
// sentence a pipeline author is looking for.
func notMoved(resolved *model.Resolved, environment string, repositories []string, t PushTrigger, stale []string) []string {
	staleSet := make(map[string]bool, len(stale))
	for _, name := range stale {
		staleSet[name] = true
	}
	var notes []string
	for _, component := range sortedKeys(t.Images) {
		if staleSet[component] {
			continue
		}
		if reason := heldStill(resolved, component, repositories, t.Ref); reason != "" {
			notes = append(notes, fmt.Sprintf("environment %s did not move %s: %s", environment, component, reason))
		}
	}
	// A stale component with no image is the other half of the same sentence:
	// kelson would have moved it and has nothing to move it to.
	for _, component := range stale {
		if strings.TrimSpace(t.Images[component]) == "" {
			notes = append(notes, fmt.Sprintf("environment %s did not move %s: it follows this push and the trigger "+
				"carries no image for it", environment, component))
		}
	}
	return notes
}

// heldStill is [model.Resolved.StaleComponents]' five conditions read backwards:
// the first one this component fails, as the sentence to say so. An empty answer
// means the component is not in this environment at all — a data component, a
// chart, or a workload this environment does not declare — which is not a reason
// to say anything.
func heldStill(resolved *model.Resolved, component string, repositories []string, ref string) string {
	source := resolved.SourceFor(component)
	switch {
	case source == nil:
		return "it binds no source here, so no push moves it (ADR-0035 decision 3)"
	case !boundTo(source.Git, repositories):
		return fmt.Sprintf("it builds from %s, and this push is about %s",
			display(source.Git), display(strings.Join(repositories, ", ")))
	case source.Ref != ref:
		return fmt.Sprintf("it follows %s, and this push is about %s", display(source.Ref), display(ref))
	case build.IsCommit(source.Ref):
		return fmt.Sprintf("it is bound to commit %s, which names one revision forever", source.Ref)
	case !resolved.AutoDeploys(component):
		return "it does not track its source here — set autoDeploy on the environment or on this component (ADR-0036 decision 1)"
	case resolved.ImagePinned(component):
		return "an image pin holds it, and a pinned component ignores everything (rule P3, ADR-0016). " +
			"Remove the pin to let it follow its source again, or mark it imageTracked: true to keep the image " +
			"as a starting point tracking may advance (ADR-0036 decision 5)"
	default:
		// Unreachable: the five conditions above are the whole of the stale set,
		// so a component that fails none of them is in it. Silent rather than
		// wrong if the two ever drift.
		return ""
	}
}

// boundTo reports whether a component's repository is one of the repositories
// this trigger is about, by [model.SameRepository] — host and path, never the
// path alone.
func boundTo(git string, repositories []string) bool {
	for _, repo := range repositories {
		if model.SameRepository(git, repo) {
			return true
		}
	}
	return false
}

// severalSources is the `build/several-sources` refusal of ADR-0036 decision 3,
// as the sentence to refuse with, or "" for a project a push may move.
//
// It applies to the kelson-built half alone. A `by: ci` project reporting images
// has already solved the problem this refuses — its pipeline built each
// component and named it — so a report carrying per-component digests is exactly
// the path this refusal points at (#252).
func severalSources(p *model.Project) string {
	if !kelsonBuildsImages(p) {
		return ""
	}
	repositories := projectRepositories(p)
	if len(repositories) < 2 {
		return ""
	}
	return fmt.Sprintf("project %s builds from %d repositories (%s) and kelson's build plane produces one image per "+
		"build, so a push to one of them could only deploy by reusing the others' images from a different commit. "+
		"Per-component image production is issue #252; until it lands, set spec.build.by: ci and report the images "+
		"your pipeline built with `kelson ci report-build` (ADR-0036 decision 3, build/several-sources).",
		p.Metadata.Name, len(repositories), strings.Join(repositories, ", "))
}

// buildPushedHead runs kelson's own build plane at the commit a push carried,
// and returns the image every component bound to that source gets.
//
// It is the webhook path's half of "two trigger paths in, one pipeline out": a
// `by: ci` project arrives with its images already named, and a `by: kelson`
// project arrives with a commit and nothing else, so this is where the second
// one catches up with the first.
//
// One build, one image, every bound component. That is not a simplification —
// it is [build.SourceToBuild]'s contract (one clone shared by every component on
// one source) and it is why [severalSources] refuses the multi-repository case
// before this runs.
//
// The second return is a *note* rather than an error for the absences that are
// postures rather than faults: a server with no build plane wired, and a project
// with nothing bound to build. Both mean "this push moves nothing here", which
// the delivery answer reports; failing the webhook over either would make GitHub
// redeliver something that will be declined identically forever.
func (s *Server) buildPushedHead(ctx context.Context, spec decoded, t PushTrigger) (map[string]string, string, error) {
	if s.build == nil {
		return nil, "this server was started without an in-cluster build plane, so a push to a kelson-built " +
			"project has nothing to build it with", nil
	}
	globals, err := s.globalSources(ctx)
	if err != nil {
		return nil, "", err
	}
	// The first environment that follows this push decides the build's
	// namespace and the bindings it reads. Any of them would do — the build is
	// per source and per commit, not per environment (build.Request leaves
	// Component empty for exactly this reason) — so the first in sorted order is
	// chosen to make the answer reproducible.
	env, resolved, stale := firstTracking(spec, globals, t)
	if env == nil {
		return nil, "", nil
	}

	binding, err := build.SourceToBuild(spec.project.Metadata.Name, resolved)
	if err != nil {
		return nil, "", err
	}
	detection, err := build.ResolveStrategy(spec.project.Spec.Build, nil)
	if err != nil {
		return nil, "", err
	}
	prefix := s.buildDefaults.Registry
	if prefix == "" {
		return nil, "this server was started without --registry (KELSON_REGISTRY), so a build triggered by a push " +
			"has nowhere to push the image it produces", nil
	}
	image, err := registry.Repository(prefix, spec.project.Metadata.Name)
	if err != nil {
		return nil, "", err
	}
	namespace := s.buildDefaults.Namespace
	if namespace == "" {
		namespace = resolved.Environment.Namespace
	}

	plane, err := s.build(ctx, BuildTarget{
		Project:          spec.project.Metadata.Name,
		Environment:      env.Metadata.Name,
		Strategy:         string(detection.Strategy),
		Namespace:        namespace,
		PushSecret:       s.buildDefaults.PushSecret,
		SourceConnection: binding.Source.Connection,
	})
	if err != nil {
		return nil, "", unavailable("api: building the build plane: %w", err)
	}
	if plane == nil || plane.Builder == nil {
		return nil, "", unavailable("api: the build plane produced no builder for %s/%s",
			spec.project.Metadata.Name, env.Metadata.Name)
	}

	// The commit is the push's own head and is never re-resolved: the delivery
	// said which commit moved the ref, and asking the forge again would race a
	// second push and build a commit nobody pushed to this trigger.
	revision := build.NormalizeCommit(t.SHA)
	res, err := plane.Builder.Build(ctx, build.Request{
		Project:          spec.project.Metadata.Name,
		Environment:      env.Metadata.Name,
		SourceGit:        binding.Source.Git,
		SourceRef:        revision,
		SourceName:       binding.Source.Name,
		SourceConnection: binding.Source.Connection,
		Dockerfile:       build.DockerfilePath(spec.project.Spec.Build),
		Image:            image,
		Tag:              build.DestinationTag(spec.project.Metadata.Name, revision),
		Revision:         revision,
	}, discard{})
	if err != nil {
		return nil, "", err
	}
	if res.Reference == "" || registry.Mutable(res.Reference) {
		return nil, "", fmt.Errorf("api: the build for %s at %s produced %q, which is not pinned by digest",
			spec.project.Metadata.Name, revision, res.Reference)
	}

	images := make(map[string]string, len(stale))
	for _, name := range stale {
		images[name] = res.Reference
	}
	return images, "", nil
}

// firstTracking is the first environment this push makes anything stale in, with
// its resolved spec and its stale set. It exists so the build path resolves the
// same way the write path does — one resolver, one answer — rather than deciding
// what to build from a spec nothing checked against the push.
func firstTracking(spec decoded, globals []model.Source, t PushTrigger) (*model.Environment, *model.Resolved, []string) {
	repositories := t.repositories(spec.project)
	for _, env := range spec.environments {
		resolved, errs := model.Resolve(spec.project, env, globals...)
		if len(errs) > 0 {
			continue
		}
		if stale := staleFor(resolved, repositories, t.Ref); len(stale) > 0 {
			return env, resolved, stale
		}
	}
	return nil, nil, nil
}

// discard swallows the build's log stream. A push has no terminal attached and
// nothing is streaming this build, so the logs go where a Job's logs already
// live — the cluster — rather than into this process's memory. `kelson logs` and
// the Job itself are how a failed auto-deploy build is read back.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// deployStatusPath is where an auto-deploy's commit status points: the
// environment's history page, which is the deepest route the UI actually serves
// for an environment (`projects/:project/:env/history`, ui/src/App.tsx). It is
// the right page as well as the deepest one — what a status is reporting is that
// a revision was triggered, and the history is where that revision appears.
func deployStatusPath(project, environment string) string {
	if environment == "" {
		return "/projects/" + project
	}
	return "/projects/" + project + "/" + environment + "/history"
}

// DeployStatusContext is the check name an auto-deploy appears under on a
// commit, and it is deliberately not [PreviewStatusContext].
//
// A forge keys statuses by context, so sharing one would make a preview publish
// and an auto-deploy of the same commit overwrite each other — and they answer
// different questions about it ("does this change request run" versus "did the
// branch this commit landed on deploy"). Branch protection names a required
// check by this exact string, so changing it silently makes an existing rule
// match nothing (the same contract [PreviewStatusContext] states).
const DeployStatusContext = "kelson/deploy"

// reportDeployStatus writes an auto-deploy back onto the commit (ADR-0036
// decision 4, through ADR-0034 decision 5's machinery), and returns the sentence
// to report a *failure* to do so with.
//
// It degrades exactly as [Server.reportPreviewStatus] does and for the same
// reason: no reporter, no connection covering the repository, or a connection
// whose provider cannot write statuses are all "statuses are a courtesy of the
// integration, not a delivery dependency", and an environment that was triggered
// was triggered whether or not the forge ever hears about it.
//
// The state is terminal, like the preview's, and the sentence says precisely
// what kelson knows: the spec was written and the controller reconciles it next.
// A `pending` would describe more of the truth and nothing here would ever
// resolve it — what happens after the write is the controller's and Flux's — and
// a required check stuck pending forever blocks merges.
// The repositories written to are the ones the trigger is about — the one a
// webhook named, or every source the Project declares when a report named none
// ([PushTrigger.repositories]). A project reading two repositories gets the check
// on both, which is right: the commit is in one of them and kelson cannot tell
// which from a report that carries images rather than an origin, and a check
// against a commit a repository does not have is a 422 the forge answers and
// this reports as a failed write-back.
func (s *Server) reportDeployStatus(ctx context.Context, repositories []string, t PushTrigger, out PushOutcome) string {
	if s.outcomes == nil || t.SHA == "" {
		return ""
	}
	state, description := deployStatusDescription(t, out)
	var failures []string
	for _, repo := range repositories {
		_, fullName, ok := splitRepository(repo)
		if !ok {
			// Unreachable for a validated spec — a `source.git` is a URL the
			// model checks — and silent rather than reported if it ever is: a
			// status is a courtesy, and nothing about what was triggered changes.
			continue
		}
		err := s.outcomes.ReportCommitStatus(ctx, CommitStatus{
			Repo:     repo,
			FullName: fullName,
			SHA:      t.SHA,
			// One context for every environment this push moved, because a forge
			// keys statuses by it: a per-environment context would leave a check
			// per environment on every commit rather than one answer.
			Context:     DeployStatusContext,
			State:       state,
			Description: description,
			Path:        deployStatusPath(t.Project, firstOr(out.Triggered, "")),
		})
		if err != nil {
			failures = append(failures, display(repo)+": "+err.Error())
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "the commit status could not be written back (" + strings.Join(failures, "; ") +
		"), which changes nothing about what was triggered"
}

// deployStatusDescription is the one line a human reads beside the check.
//
// A refusal is `failure` because something the author can fix is in the way. A
// push that moved nothing is `success` and says so: a commit on a ref no
// environment follows is not a broken pipeline, and a red check for it would
// train everybody to ignore the green ones.
func deployStatusDescription(t PushTrigger, out PushOutcome) (state, description string) {
	switch {
	case out.Refused != "":
		return "failure", "refused: " + firstSentence(out.Refused)
	case out.moved():
		return "success", fmt.Sprintf("deploying %s to %s", displaySHA(t.SHA), strings.Join(out.Triggered, ", "))
	default:
		return "success", "no environment follows " + display(t.Ref)
	}
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

// firstSentence bounds a refusal for a forge's description field, which is short
// and truncated silently by the API rather than by kelson.
func firstSentence(s string) string {
	if cut := strings.Index(s, ". "); cut > 0 {
		return s[:cut]
	}
	return s
}

// displaySHA abbreviates a commit for prose. The full value is in the status's
// own subject and in the audit record; what a description needs is something a
// person can compare at a glance.
func displaySHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
