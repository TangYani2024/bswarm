# bswarm

[English](README.md) | 简体中文

`bswarm` 是面向 Google Antigravity 的零 MCP、单二进制 Go 插件，提供树状多代理编排能力。默认开箱即支持官方 Antigravity 模型，同时支持通过热加载配置将子代理按需路由至任意 OpenAI 或 Gemini 兼容的外部 API。

## 核心特性

- **树状多层级编排**：支持 `Root -> Lead -> Worker` 多级任务拆解、严格父子封装与级联回收。
- **私有收件箱与按引用传递**：消息摘要自动限制在 400 字符以内，长输出落盘至节点独立产物文件。
- **热节点复用**：任务完成后节点转为 `idle` 状态，可通过 `--reuse` 或 `assign` 复用上下文，减少冷启动开销。
- **写入隔离**：支持独立 Git Worktree（`--worktree`）及基于钩子强制校验的路径锁（`--owns`）。
- **官方与外部模型混用**：零外部配置即可纯使用官方 Antigravity 模型；也可在同一任务树中通过隔离运行环境混合调度外部 OpenAI / Gemini 兼容端点。
- **外部 API 热加载与并发池**：CLI 与后台 Watchdog 自动热加载 `providers.json` 变更，无需重启；支持按平台/模型/前缀配置并发上限、自动排队、流式空闲超时及模型池故障切换。
- **零 Token 后台监控**：Watchdog 持续监控活跃节点，转发收件箱通知、推进排队的 DAG 或并发槽位任务、对长时间无进展的节点进行中途催促与死循环提前熔断，并在外部网关子代理卡死时自动回收进程组以释放并发槽位。

## 构建与安装

环境要求：Go >= 1.22（CLI 环境建议安装 Node.js；Antigravity 桌面客户端在缺少系统 `node` 时会自动回退至内置 `agy-node` 或 `PATH` 中的 `bswarm`）。

> 提示：安装并启用本插件前，建议先关闭原有的原版 swarm 插件（可执行 `agy plugin list` 查看名称，并通过 `agy plugin disable <plugin-name>` 禁用，同时注意清理或覆盖 `PATH` 中旧的 `bswarm` 软链接），避免技能触发、钩子拦截或同名命令发生冲突。

### Antigravity CLI（`agy`）

```bash
git clone https://github.com/TangYani2024/bswarm.git
cd bswarm

go build -ldflags="-s -w" -o bin/bswarm ./cmd/bswarm
agy plugin validate --strict .
agy plugin install .
agy plugin enable bswarm

# 可选：将 bswarm 软链至 PATH
ln -sf ~/.gemini/config/plugins/bswarm/bin/bswarm ~/.local/bin/bswarm
```

### Antigravity 桌面客户端

官方 Antigravity 桌面客户端同样读取 `~/.gemini/config/plugins/`，原生支持官方子代理、Hooks 与内置 Swarm UI 面板。若未安装 `agy` CLI，可将编译完成的仓库复制或软链至 `~/.gemini/config/plugins/bswarm`，并在 `~/.gemini/config/config.json` 中确保 `"plugins": { "bswarm": { "enabled": true } }`。

> 说明：在桌面客户端中调用第三方外部 API（`--provider` / `pool:`）需要本机同时安装 `agy` CLI（桌面客户端内置的 `language_server` 不包含无头 `--output-format stream-json --print` 模式）。
> 说明：在终端沙箱中运行时，与本地 Antigravity Language Server（`ANTIGRAVITY_LS_ADDRESS`）通信需要允许环回网络访问（`sandboxAllowNetwork`）。

## 使用方法

### 基础多代理编排

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

### 外部 API 提供方（可选，支持热加载）

未配置任何外部提供方时，`bswarm` 完全使用官方 Antigravity 模型运行。

如需混用第三方 OpenAI 或 Gemini 兼容接口，可创建或修改 `~/.gemini/bswarm-providers.json`（或项目内 `.bswarm/providers.json`，也可通过环境变量 `BSWARM_PROVIDERS_FILE` 指定）。配置更新后立即生效，无需重启。

```bash
# 生成初始配置文件
bswarm providers init

# 热添加提供方（默认不限制并发）
bswarm providers add deepseek \
  --url https://api.deepseek.com/v1 \
  --key-env DEEPSEEK_API_KEY \
  --models deepseek-chat,deepseek-reasoner

# 查看已配置的提供方与当前并发槽位（密钥自动脱敏）
bswarm providers

# 将子代理调度至外部模型或受控并发池
bswarm spawn --id w1 --provider deepseek --model deepseek-chat --goal "Audit parser"
bswarm spawn --id w2 --model deepseek:deepseek-reasoner --goal "Review edge cases"
bswarm spawn --id w3 --model pool:fast-pool --goal "Summarize test logs"
```

完整配置项（包括 `wire_protocol`、`api_key_file`、`max_concurrency`、`concurrency_scope`、`idle_timeout` 与 `pools`）参见 [`PROVIDERS.example.json`](PROVIDERS.example.json)。

## 开发与验证

```bash
go vet ./...
go test -race ./...
agy plugin validate --strict .
```
