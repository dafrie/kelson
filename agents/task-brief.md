# Writing a task brief

A delegated agent has your context only through this text. It cannot ask you a quick question
mid-flight, and you will not learn the brief was ambiguous until it reports back — often an hour
later, with a diff that touches four packages you did not mean it to touch.

So a brief is not a summary of the issue. The issue says what should be true; the brief says what
*this agent* is to do, where its authority ends, and how it proves it is finished.

## Template

```
GOAL
One sentence. What is true after this lands that is not true now.

ISSUE
#N — <title>. Read it and its epic before starting; the acceptance criteria there are yours.

YOU OWN (exclusively, for this task)
internal/<pkg>/, <other paths>
Nothing outside this list. If the work needs a change elsewhere, stop and report it — do not
edit across the boundary. Another agent owns that package right now.

CONTRACTS — use these exactly, do not redesign
<type / signature / wire field, verbatim>
These are shared with a task running in parallel. If one looks wrong, say so and stop; changing
it unilaterally breaks the other side silently.

CONSTRAINTS
- <ADR-00NN governs this; contradicting it is a design discussion, not a code change>
- <purity / dependency / RBAC constraints that apply here>

DONE MEANS
- go test ./... passes (make test for -race)
- make lint passes
- <the specific new test that proves the goal>
- Conventional-commit messages referencing #N

DELIVER
Branch ag/<letter>-<topic> off main, push, open a PR against main using the repo template,
reference #N and any ADR. Do not merge — report back with the PR number.

REPORT
PR number, what you changed, anything you found that I should know, and anything you deliberately
did not do.
```

Drop sections that do not apply. A docs task does not need a contracts block. Keep `YOU OWN`,
`DONE MEANS` and `REPORT` in every brief — those three are what make a report trustworthy.

## Worked example

```
GOAL
Controller tests can exercise the delivery path against real Flux CRDs instead of skipping.

ISSUE
#243 — Flux CRD fixtures in envtest. Part of the CRD-native rebuild (#223).

YOU OWN (exclusively)
internal/controller/, test/ fixtures under it, hack/e2e/ if the fixtures need vendoring.
Do not touch internal/artifact/ or deploy/chart/ — both are live in other tasks this cycle.

CONTRACTS — use these exactly
The OCIRepository/Kustomization pair is built by the existing SSA helper in internal/controller.
Do not re-derive the object shapes in test; assert against what the helper produces.

CONSTRAINTS
- ADR-0028 fixes the spine as render → OCI artifact → Flux. Tests assert that path, not a
  substitute for it.
- Vendored CRD YAML must be pinned to the Flux version the chart installs, not "latest".

DONE MEANS
- go test ./internal/controller/... passes with the envtest fixtures present
- The previously skipped delivery-path tests now run and pass
- make lint passes

DELIVER
Branch ag/w-envtest off main, push, PR against main referencing #243 and ADR-0028. Do not merge.

REPORT
PR number, which tests un-skipped, and whether any needed behaviour changes in non-test code —
if they did, describe it rather than making it.
```

## Common ways a brief goes wrong

**"Fix the X issue."** The agent re-reads the issue, guesses at scope, and returns something
adjacent to what you wanted. Name the goal in your own words.

**No ownership list.** The agent helpfully fixes a related thing in a package another agent is
rewriting. Both PRs then conflict, and the merge is yours to untangle.

**"Make sure it works."** Not checkable. `go test ./...` and a named new test are checkable, and an
agent that knows the exact command will run it before reporting.

**Inventing a shared type.** Two agents meeting at one seam will produce two incompatible types
unless you hand both the same text. Fix the contract before dispatch, never during.

**Letting the agent merge behaviour changes.** Low-stakes work can self-merge once CI is green.
Anything touching behaviour, golden files, a public contract or an ADR-governed decision comes back
to you. Say which of the two this task is, in the brief, so the agent does not have to guess.
