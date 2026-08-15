# Tooling map

[`architect.md`](architect.md) is written in terms of capabilities so it survives a change of agent
harness. This file maps those capabilities onto concrete tools. If you are running something other
than Claude Code, substitute the equivalents and the playbook still holds.

| Capability the playbook assumes | Claude Code | Notes |
|---|---|---|
| Delegate a bounded task to a subagent | `Agent` | Runs in the background by default and notifies on completion — that is what makes "dispatch first, then ask" work |
| Run 2–3 tracks concurrently | `Agent` × N **in one message** | Separate messages serialize them |
| Pick the model per task | `model:` — `opus`, `sonnet`, `haiku`, `fable` | Or a full ID such as `claude-opus-5` |
| Give a writing agent its own working directory | `isolation: "worktree"` | Required whenever two agents write at once; skip it for read-only agents, they start faster |
| See what is running now | `ListAgents`, `TaskList`, `TaskOutput` | Far cheaper than re-reading scrollback |
| Continue an agent with its context intact | `SendMessage` (agent ID or name) | A fresh `Agent` call starts over |
| Broad read-only sweep | `Explore` subagent | Returns conclusions rather than file dumps |
| Ask a blocking, structured question | `AskUserQuestion` | Blocks your turn only — dispatched agents keep running |
| Schedule a self check-in | `send_later` (claude-code-remote MCP), `/loop`, `CronCreate` | For gaps nothing else will wake you from. Never `sleep` |
| GitHub without `gh` | `mcp__github__*` | `gh` is frequently absent in remote sessions; `which gh` first |
| Reclaim context | `/compact` | At cycle boundaries — see §6 of the playbook |

## Role definitions

The roles in [`roles/`](roles/) are plain markdown so any harness can load them as a system prompt.

Claude Code additionally exposes them as subagent types via thin adapters in `.claude/agents/` that
carry the model and effort defaults in frontmatter and delegate their body to these files. Pass them as
`subagent_type`:

| Role | Subagent type | Model | Effort |
|---|---|---|---|
| [implementer](roles/implementer.md) | `kelson-implementer` | `opus` | `high` |
| [scout](roles/scout.md) | `kelson-scout` | `sonnet` | `medium` |
| [tidy](roles/tidy.md) | `kelson-tidy` | `sonnet` | `medium` |

Editing a file under `roles/` changes the behaviour for every harness at once. Edit the adapter in
`.claude/agents/` only to change a Claude-specific default such as the model, the effort or the tool
allow-list.

## Starting an Architect session

Anything that gives the coordinator a strong reasoning model at high effort works. On Claude Code:

- **Desktop or web** — pick Fable 5 and "Extra" effort when creating the session, set permission mode
  to `auto`, then say *"what's next"* or invoke `/architect`.
- **From an existing session** — `create_session` with `model: "claude-fable-5"`, an initial prompt of
  `/architect`, and the repository as its source.

The coordinator's model is a session-level setting; the playbook cannot change it from the inside. If
it is not what you wanted, say so once and carry on.
