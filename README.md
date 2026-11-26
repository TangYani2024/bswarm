# bswarm

`bswarm` is a zero-MCP, single-binary Go plugin for Google Antigravity that provides hierarchical multi-agent orchestration. It supports official Antigravity models out of the box and optionally routes subagents to any OpenAI- or Gemini-compatible external API with hot-reloaded configuration.

## Features

- **Hierarchical tree orchestration**: Supports multi-level `Root -> Lead -> Worker` delegation with strict parent-child encapsulation and cascade retirement.
- **Private inbox with pass-by-reference**: Bounds message summaries to 400 characters and spills long outputs to per-node artifact files.
- **Warm node reuse**: Finished nodes transition to `idle` and can be resumed via `--reuse` or `assign` without cold-starting.
- **Write isolation**: Supports isolated git worktrees (`--worktree`) and hook-enforced shared-workspace path locks (`--owns`).
- **Official + external model mixing**: Runs official Antigravity models with zero external configuration, or mixes in external OpenAI/Gemini-compatible endpoints in the same tree using isolated worker environments.
- **Hot-reloaded providers & concurrency pools**: Dynamically reloads `providers.json` across CLI calls and the background watchdog without restarting, with optional per-provider/per-prefix concurrency limits, automatic queuing, stream idle timeout, and pool failover.
- **Zero-token watchdog**: Monitors active nodes, relays sandboxed inbox notifications, promotes queued DAG/concurrency tasks, and alerts on stalls or quota errors.

## Build and Install

Requires Go >= 1.22 and Node.js.

```bash
git clone https://github.com/TangYani2024/bswarm.git
cd bswarm

go build -ldflags="-s -w" -o bin/bswarm ./cmd/bswarm
agy plugin validate --strict .
agy plugin install .
agy plugin enable bswarm

# Optional: expose `bswarm` on PATH
ln -sf ~/.gemini/config/plugins/bswarm/bin/bswarm ~/.local/bin/bswarm
```

> Note: Communication with the local Antigravity Language Server (`ANTIGRAVITY_LS_ADDRESS`) requires loopback network access when running inside a terminal sandbox (`sandboxAllowNetwork`).

## Usage

### Core Orchestration

```bash
bswarm init --goal "Refactor storage layer"
bswarm spawn --id worker-a --role coder --worktree --goal "Implement module A"
bswarm spawn --id worker-b --role coder --owns "docs/**" --model gemini-4-hydrogen-high --goal "Update docs"
bswarm status
bswarm inbox
bswarm assign worker-a --goal "Add unit tests for module A"
bswarm merge all --strategy squash --verify "go test ./..."
bswarm retire worker-a --cascade
```

### External API Providers (Optional, Hot-Reloaded)

Without any provider configuration, `bswarm` runs entirely on official Antigravity models.

To mix in third-party OpenAI- or Gemini-compatible APIs, create or update `~/.gemini/bswarm-providers.json` (or `.bswarm/providers.json` in a repository, or set `BSWARM_PROVIDERS_FILE`). Changes take effect immediately without restarting.

```bash
# Generate a starter config
bswarm providers init

# Hot-add a provider (unlimited concurrency by default)
bswarm providers add deepseek \
  --url https://api.deepseek.com/v1 \
  --key-env DEEPSEEK_API_KEY \
  --models deepseek-chat,deepseek-reasoner

# List configured providers and active concurrency slots (secrets redacted)
bswarm providers

# Spawn subagents on external models or a concurrency-managed pool
bswarm spawn --id w1 --provider deepseek --model deepseek-chat --goal "Audit parser"
bswarm spawn --id w2 --model deepseek:deepseek-reasoner --goal "Review edge cases"
bswarm spawn --id w3 --model pool:fast-pool --goal "Summarize test logs"
```

See [`PROVIDERS.example.json`](PROVIDERS.example.json) for full options including `wire_protocol`, `api_key_file`, `max_concurrency`, `concurrency_scope` (`provider`, `model`, `prefix:/`), `idle_timeout`, and `pools`.

## Development

```bash
go vet ./...
go test -race ./...
agy plugin validate --strict .
```
