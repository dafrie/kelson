# agents/

Playbooks and role definitions for AI agents working on kelson. Plain markdown, no vendor lock-in —
[`AGENTS.md`](../AGENTS.md) at the repository root is the entry point every agent should read first,
and this directory holds the longer material it links to.

| File | What it is |
|---|---|
| [`architect.md`](architect.md) | The coordinator playbook: orient, plan a slice, keep 2–3 delegated agents busy, land the work, keep the tracker honest |
| [`task-brief.md`](task-brief.md) | Template and worked example for briefing a delegated agent |
| [`tooling.md`](tooling.md) | Maps the capabilities the playbook assumes onto concrete tools, and how to start a coordinator session |
| [`roles/`](roles/) | Role definitions — [implementer](roles/implementer.md) (Opus 5), [scout](roles/scout.md) (Sonnet 5, read-only), [tidy](roles/tidy.md) (Sonnet 5) |

## Using these

**Coordinating a session** — read [`architect.md`](architect.md). On Claude Code the `/architect`
skill loads it for you.

**Working a single task** — you were probably given a brief written from
[`task-brief.md`](task-brief.md); read the matching file under [`roles/`](roles/) and
[`AGENTS.md`](../AGENTS.md).

## Harness adapters

`.claude/skills/architect/` and `.claude/agents/*.md` are thin Claude Code adapters. They exist so the
skill triggers and the subagent types carry the right model defaults; the content lives here. Adding an
adapter for another tool should mean a few lines of frontmatter pointing back at these files, not a
second copy of the prose.

Keep it that way — two copies of a playbook diverge within a week, and the one the agent actually read
is the one that was stale.
