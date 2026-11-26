---
name: bswarm
description: >-
  Zero-MCP hierarchical multi-agent orchestrator (`bswarm`) for Antigravity:
  tree-scoped subagents, official and hot-reloaded external OpenAI/Gemini API
  routing, concurrency-aware model pools, private inbox, worktree isolation,
  and zero-token watchdog.
metadata:
  logo: logo.svg
---

# `bswarm` — Hierarchical Zero-MCP Swarm

Binary: [`../../bin/bswarm`](../../bin/bswarm) (`bswarm` if on `PATH`). Stdout capped `<= 6 KB` (`status <= 1.8 KB`).

## Model Routing & Hot-Reloaded Providers
Config discovery (hot-reloaded without restart): `BSWARM_PROVIDERS_FILE` -> `.bswarm/providers.json` -> `<store>/providers.json` -> `~/.gemini/bswarm-providers.json`.
- Official Antigravity models (default, no external API required): omit `--model` or pass an official model ID.
- External OpenAI/Gemini-compatible API: `--provider <name> --model <id>` or `--model <provider>:<id>`.
- Dynamic model pool: `--model pool:<pool_name>` (selects an idle slot, queues automatically when limits are reached, and fails over to backup candidates on upstream errors).

## Rules
- **Tree encapsulation (depth <= 3)**: Children report only to their direct parent.
- **Pass-by-reference**: Write long reports, logs, or diffs to files in the node artifacts directory; pass `--summary "<=400 chars"` and `--artifact <path>`.
- **Reuse idle nodes**: Finished nodes remain `idle` (0-token sleep). Use `spawn --reuse --role <r>` or `assign <id>` to resume warm context.
- **Isolate writes**: Use `--worktree` for parallel code edits or disjoint `--owns "src/a/**"` in shared workspaces (`PreToolUse` blocks cross-owner writes).

## Commands
```bash
bswarm providers [--json]                                                 # list providers, pools, and active slots
bswarm providers add <name> --url <url> --key-env <VAR> --models m1,m2    # hot-add an external API provider
bswarm init --goal "..."                                                  # optional; first spawn auto-initializes
bswarm spawn --id w1 --role worker --worktree --goal "..."                # --provider, --model, --owns, --depends-on, --verify
bswarm spawn --reuse --role worker --goal "..."                           # reuse an idle child node with warm context
bswarm assign <node_id> --goal "..." [--verify "cmd"]                     # wake a specific idle child node
bswarm done --summary "<=400 chars>" [--artifact <path>]                  # report to direct parent and transition to idle
bswarm ask <parent|node_id> --summary "..." [--artifact <path>]           # direct parent/child mail (reply via `bswarm reply`)
bswarm inbox [--all] && bswarm status [--archived]                        # unread private mail and active tree status
bswarm merge <node_id|all> [--strategy squash] [--verify "cmd"]           # merge worktree branches with rollback protection
bswarm retire <node_id> --cascade                                         # archive subtree, stop workers, and remove worktrees
bswarm gc [--native-audit] [--apply --older-than 7d]                      # clean orphan worktrees or native swarm leftovers
```

## Workflow
1. Split tasks with disjoint `--worktree` or `--owns`, run `spawn`, then end turn until woken. On Antigravity Desktop, include `[bswarm](sidecar://bswarm/bswarm_board/)` in your reply after spawning so the user can open the hierarchical tree board in one click.
2. On wake, run `bswarm inbox`, inspect artifacts as needed, then reuse idle nodes or run `merge` and `retire --cascade`.
