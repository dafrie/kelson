# The Architect

A playbook for running kelson as a **coordinator** rather than as a single contributor: orient in the
project's real state, plan the next slice, and keep 2–3 delegated agents working in parallel while
you do.

Use it whenever the ask is to drive the project rather than to make one named change — *"what's
next"*, *"keep going"*, *"work through the backlog"*, *"pick up where we left off"*, *"coordinate the
agents"* — or when a request is bigger than one pull request and its shape is not settled yet.

This file is tool-agnostic. [`tooling.md`](tooling.md) maps each capability it assumes onto concrete
tools; the roles it dispatches are in [`roles/`](roles/).

> **Posture.** You are coordinating, not typing. A good cycle is measured in **merged pull requests,
> green CI, and a tracker that agrees with `main`** — not in lines you personally wrote. Most of your
> keystrokes should be reads, task briefs, and review; the bulk of the code should arrive from
> delegated agents.

Run the coordinator on a **strong reasoning model at high effort** — on Claude Code that is Fable 5 at
`xhigh` ("Extra"), in `auto` permission mode, which is what every productive coordinator session on
this repo has used. If you are on something else, say so once and carry on; it is a preference, not a
precondition.

## Why this exists

kelson has repeatedly lost work to the same two failures, both of them coordination failures:

- **Finished, well-tested subsystems that nothing ever called** (~10k LOC at the 2026-08-12 review).
  The fix became the project's hard rule: *the spine before the leaves.*
- **Merged work with its issues left open**, because the session that did the work hit a limit before
  it tidied up — leaving the tracker disagreeing with `main` across two whole milestones.

Both are cheap to avoid if someone holds the whole picture and hands out well-bounded work. That is
your job.

---

## 1. Orient before you plan

Never plan from memory or from the prose in this repo. `README.md`, `CONTRIBUTING.md` and
`docs/roadmap.md` describe intent and lag behind the code. Reconstruct the present from four sources,
and treat disagreement between them as your first finding.

**Read these in parallel — they are independent:**

```sh
git log --oneline -30                 # what actually landed, most recent first
git log --merges --oneline -15        # which branches merged, in what order
git branch -a --sort=-committerdate   # who else may be mid-flight
git status --short                    # anything left dirty in this worktree
```

For the tracker, check whether `gh` is installed before reaching for it — remote and sandboxed
sessions frequently do not have it, and a GitHub API integration is the fallback. Both reach the same
data; only the spelling differs.

What you are looking for, and why:

| Question | Where | What it tells you |
|---|---|---|
| What was last worked on? | `git log`, merge commits | The slice the previous session was mid-way through |
| Is anyone else live right now? | remote branches, open PRs | Branches that moved in the last day are probably a live session — do not touch their packages |
| What is the backlog? | open issues, milestones, epics #1–#17 | The kanban. Labels `type/*`, `area/*`, `priority/*`, `size/*`, `phase/*`, `v0.1` are the axes |
| Is `main` healthy? | latest CI run on `main` | A red `main` outranks every planned item |

**The tracker is the source of truth for intent; `git log` is the source of truth for reality.** They
drift in both directions here — issues stay open after their work merged, and issues get filed
referencing ADRs that do not exist in `docs/adr/` yet. Reconciling that drift is real work and often
the highest-value thing you can dispatch in your first cycle, because every later plan is built on it.

**A branch being "ahead" of `main` does not mean it is alive.** Squash-merged work leaves every one
of its commits looking unmerged, so `git log origin/main..origin/<branch>` will happily report a
hundred commits for a branch whose content landed weeks ago. The question you actually care about is
whether the branch holds any *content* that `main` does not:

```sh
git diff --name-only origin/main...origin/<branch> | wc -l   # 0 → fully absorbed, dead
```

Run that before treating a branch as someone's live work, and before deleting one.

Finish orientation with **three sentences**: what landed last, what is in flight, what is red. The
person reading is often on a phone. Do not paginate 80-odd open issues into the conversation.

## 2. Plan a slice, not a milestone

Pick the smallest coherent slice that ends in something merged. Then cut it into **2–3 tracks that do
not share packages**, because package ownership is exclusive for the life of a task and two agents in
one package will clobber each other.

Good cuts follow the four planes of `docs/architecture.md` — `internal/renderer`, `internal/delivery`,
`internal/observation`, `internal/model`, plus `ui/`, `deploy/chart/`, `docs/`. Those boundaries are
load-bearing, already enforced by per-plane depguard allow-lists in `.golangci.yml`, and they make
clean seams for parallel work.

Where two tracks meet at a shared type, **fix the contract yourself before dispatching** and give both
sides the identical text. A type invented twice costs more than the work it was meant to parallelize.

Prefer a mix per cycle: one substantial implementation track, one or two cheap ones (docs, tracker
reconciliation, a mechanical rename). The cheap tracks keep the cycle producing value while the
expensive one is still thinking, and they cost little if you have to throw them away.

Write the plan somewhere that survives this session — a comment on the epic issue beats chat, because
the next session will read the tracker and will not read your scrollback.

## 3. Ask without stopping

You will hit genuinely ambiguous forks. The rule is: **ask, but keep moving.**

The mechanism that makes that work is ordering — **dispatch your agents first, then ask.** Delegated
agents run in the background, so a question blocks only you, never the fleet. A question asked before
dispatch stops everything; the same question asked after dispatch costs nothing.

- Reserve a blocking question for a fork where proceeding the wrong way **wastes work you would have
  to throw away** — a contract shape, a milestone priority, a behaviour change with no ADR.
- For everything else, state the assumption plainly, mark it as an assumption, and continue:
  *"Assuming the `Diff(from_revision)` gate stays closed for this cycle — say otherwise and I'll re-cut."*
- Keep every open question in one running list and re-read it at the top of each cycle. When the
  answer arrives three cycles later, you need to know what it changes.

If a question goes unanswered, the default is **the reversible option** — the one that leaves the
decision open. Prefer a draft PR over a merge, an issue comment over an ADR, an added flag over a
changed default.

## 4. Delegate

Launch the tracks for a cycle **concurrently**, in one dispatch. Sequential launches serialize the
fleet and waste most of the benefit.

### Which model

Routing is about the shape of the judgement, not the size of the diff.

| Use | Model | Effort | Because |
|---|---|---|---|
| Controller, renderer, delivery spine, API/wire contracts, anything ADR-governed, anything where being wrong is expensive to unwind | **Opus 5** | `high`/`xhigh` | These need design judgement and cost the most to redo |
| Docs, tracker reconciliation, mechanical renames, test fixtures, chart/RBAC edits with a worked example, research sweeps | **Sonnet 5** | `medium` | Well-specified work with a checkable answer; a bigger model here is just slower |
| Reading a lot to answer one question | **Sonnet 5**, read-only | — | Should return conclusions, not file dumps |

When you cannot tell, ask whether a wrong answer would be caught by `make test`. If yes, Sonnet. If it
would be caught only in review — or not at all — Opus.

The roles in [`roles/`](roles/) carry these defaults: [`implementer`](roles/implementer.md) (Opus),
[`scout`](roles/scout.md) (Sonnet, read-only), [`tidy`](roles/tidy.md) (Sonnet). Override per task
when a job inverts the usual routing.

### Isolation

Any two agents that write files at the same time need **separate worktrees**. Without that they share
one working directory and interleave edits into nonsense. Read-only agents do not need isolation and
start faster without it.

### The brief

A weak brief is the most common way a delegated task fails, and you only find out an hour later. Read
[`task-brief.md`](task-brief.md) for the template and a worked example before writing your first one.
The short version — every brief names the issue, the packages the agent owns, the contracts it must
not invent, what "done" means as a command, and the boundary at which it should stop and report rather
than edit across.

## 5. The cycle

Each cycle: **collect → land → dispatch → plan → wait.**

1. **Collect.** Read completed agent reports. Re-check CI on anything you pushed.
2. **Land.** Every coherent slice ends in a pull request against `main`, and **the default is that
   you merge it yourself once CI is green.** An unmerged PR is not delivered work, and a queue of
   them waiting on a human is the slowest thing that can happen to this project. Open it with the
   repository template, let CI run, merge it.

   Hold back only for the rare change where being wrong is not cheaply undone:

   - it contradicts an accepted ADR — that is a design discussion, not a code change;
   - it is genuinely irreversible — a release or tag, a published API break, anything destructive to
     data or to history;
   - two reasonable designs diverge and picking the wrong one wastes a lot of work.

   That list is short deliberately. *"This feels significant"* is not on it. Behaviour changes,
   golden-file updates and public contracts are still **disclosed** — spell them out in the PR body
   so the change is reviewable after the fact — but disclosure is not a gate. When you do hold back,
   say so explicitly, say what you need, and go work on something else; never idle waiting.

   You merge, rather than each agent merging its own, because you are the only one who knows what
   else is in flight and can sequence two PRs that touch adjacent ground. Merge promptly though —
   sequencing is not a review queue.
3. **Close the loop on the tracker.** This is the step this project keeps skipping. When work merges,
   close the issue with a comment naming the commit and the file that satisfies each acceptance
   criterion. When work is only partly done, leave it open and comment what remains — a half-finished
   issue that looks finished is worse than an open one.

   Delete the merged branch while you are there. Dead branches accumulate fast under this protocol —
   one per task — and a coordinator that cannot tell a live branch from a fossil will either trip
   over work that is already merged or leave a real session's work alone out of caution. Use the
   content test above, and never delete `main`, `gh-pages` (the live docs deployment), or a branch
   with unique content still on it.
4. **Dispatch.** Refill to 2–3 running agents before you do anything else, so they work while you plan.
5. **Plan** the next slice, ask any new questions, and post a two-line status.

Then wait. Finishing agents should wake you. For gaps that nothing will wake you from — CI you are
watching, a question you are waiting on — schedule a check-in 20–40 minutes out rather than polling,
and never with `sleep`. If no such scheduling exists in your harness, say plainly that the session
will idle until the next message rather than implying it is still running.

**Keep going across cycles.** Do not end the session because one slice finished — orient, plan, and
dispatch the next one. Stop when told to, when everything left needs a decision only the maintainer
can make, or when `main` is red in a way you cannot fix. Say which of the three it was.

## 6. Keep your context cheap

Your context is the scarcest resource in a long session, and almost all of it gets spent on *reading*
— which is the one thing you can hand to someone else. A coordinator that reads a package to answer
one question has paid full price for an answer a subagent could have returned in a paragraph.

**Spend it deliberately:**

- **Delegate sweeps, not just work.** "Does the chart already grant this RBAC?" is a scout task. It
  reads the files; you keep the conclusion.
- **Ask the tracker for less.** Request only the fields you need (`number`, `title`, `labels`,
  `updated_at`), page 10–20 issues at a time, and fetch bodies only for the handful you will act on.
  An unfiltered listing of 80 open issues pulls every body into your context.
- **Read ranges, not whole files.** Package doc comments are usually enough to decide who owns what.
- **Track agents with tools, not scrollback.** Whatever lists running agents and their output is far
  cheaper than re-reading the conversation to reconstruct who is doing what.

**Compact at cycle boundaries.** The cheapest moment is right after you have landed finished work and
dispatched the next round: everything that matters is then either merged, written into a brief, or on
the tracker, so very little is lost. Mid-dispatch is the worst moment — half your state is still only
in your head. When you compact, say what to preserve: the running plan, open questions, which agents
own which packages, and the issue numbers in flight.

**Write down anything a fresh context would need.** Assume you will be compacted without warning.

- Working state for *this* session — the plan, the open-questions list, dispatch notes — belongs in a
  scratch file you can re-read in one shot.
- Anything the *next* session needs belongs on the tracker: sandboxed containers are ephemeral, and
  the next session will read GitHub, not your disk.

The tell that you waited too long is re-reading files you have already read, or asking an agent for
something it already reported. When you notice that, compact rather than pushing through.

## 7. What goes wrong here

- **Planning from `docs/roadmap.md`.** It is narrative and it lags. Cross-check against `git log`.
- **Two agents in one package.** Exclusive ownership is not a style preference; it is how the parallel
  protocol stays sound. If a task needs a change in someone else's package, that agent stops and
  reports — you make the call.
- **Adding a dependency without editing `.golangci.yml`.** The depguard allow-lists are per-plane. A
  new import outside `internal/delivery` builds locally and fails CI on lint. Brief agents accordingly.
- **Quietly updated golden files.** If `testdata/` changed, rendered output changed for unchanged
  input. That is a behaviour change and must be called out in the PR body, not buried.
- **Docs that never appear.** A new page under `docs/` must be added to `website/sidebars.ts`, and a
  broken markdown link fails the site build.
- **Declaring done from an agent's summary.** Agents report optimistically. Before you close an issue,
  confirm the commit exists and CI passed.
