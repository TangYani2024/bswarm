# bswarm

[English](README.md) | 简体中文

`bswarm` 是面向 Google Antigravity 的零 MCP、单二进制 Go 插件，提供树状多代理编排能力。默认开箱即支持官方 Antigravity 模型，同时支持通过热加载配置将子代理按需路由至任意 OpenAI 或 Gemini 兼容的外部 API。

## 核心特性

- **树状多层级编排**：支持 `Root -> Lead -> Worker` 多级任务拆解、严格父子封装、基于 `parentConversationId` 的原生子会话归属（避免左侧顶级对话列表刷屏）与级联回收。
- **深度集成官方 Swarm UI 面板**：直接投影至 Antigravity 内置的 **Swarm** 面板（`swarm_board`），完整呈现多层级树（`L0 -> L1 -> L2 -> L3`）、Hive 蜂巢视图、按子树隔离的 Board 频道，以及官方与外部 API 节点的实时步骤状态。
- **私有收件箱与按引用传递**：消息摘要自动限制在 400 字符以内，长输出落盘至节点独立产物文件。
- **热节点复用**：任务完成后节点转为 `idle` 状态，可通过 `--reuse` 或 `assign` 复用上下文，减少冷启动开销。
- **写入隔离**：支持独立 Git Worktree（`--worktree`）及基于钩子强制校验的路径锁（`--owns`）。
- **官方与外部模型混用**：零外部配置即可纯使用官方 Antigravity 模型；也可在同一任务树中通过隔离运行环境混合调度外部 OpenAI / Gemini 兼容端点。
- **外部 API 热加载与并发池**：CLI 与后台 Watchdog 自动热加载 `providers.json` 变更，无需重启；支持按平台/模型/前缀配置并发上限、自动排队、流式空闲超时及模型池故障切换。
- **零 Token 后台监控**：Watchdog 持续监控活跃节点，转发收件箱通知、推进排队的 DAG 或并发槽位任务、对长时间无进展的节点进行中途催促与死循环提前熔断，并在外部网关子代理卡死时自动回收进程组以释放并发槽位。

## 构建与安装

环境要求：Go >= 1.22（CLI 环境建议安装 Node.js；Antigravity 桌面客户端在缺少系统 `node` 时会自动回退至内置 `agy-node` 或 `PATH` 中的 `bswarm`）。

> 提示：安装并启用本插件前，建议先关闭旧版 `better-swarm` 或其他同名/同类自定义 swarm 插件（可执行 `agy plugin list` 查看名称，并通过 `agy plugin disable <plugin-name>` 禁用，同时注意清理或覆盖 `PATH` 中旧的 `bswarm` 软链接），避免技能触发、钩子拦截或同名命令发生冲突。如需在官方 Swarm UI 面板（`swarm_board`）中查看任务树，请保持 Antigravity 内置的 `swarm` 插件处于启用状态。

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

官方 Antigravity 桌面客户端同样读取 `~/.gemini/config/plugins/`，原生支持官方子代理、Hooks、内置 Swarm UI 面板，以及**免装 `agy` CLI 的第三方外部 API 路由**。在未安装 `agy` CLI 的纯桌面环境下，`bswarm` 会自动使用桌面客户端内置的 `language_server` 启动独立隔离的无头进程执行外部模型任务，回合结束后通过 `--exit_on_stdin_close` 自动退出清理。

若未安装 `agy` CLI，可将编译完成的仓库复制或软链至 `~/.gemini/config/plugins/bswarm`，并在 `~/.gemini/config/config.json` 中确保 `"plugins": { "bswarm": { "enabled": true } }`。

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
bswarm sync-ui
bswarm merge all --strategy squash --verify "go test ./..."
bswarm retire worker-a --cascade
```

### 官方 Swarm UI 集成

请保持 Antigravity 内置的 `swarm` 插件处于启用状态。点击对话顶部栏的 `+` 菜单并选择 **Swarm** 即可打开面板，面板会自动监听当前活动对话。

`bswarm` 将完整的多代理编排状态直接投影至官方内置 Swarm 面板（`swarm_board`）：

- **Overview 标签页（`view.json` + `view.html`）**：渲染多层级任务树（`L0 -> L1 -> L2 -> L3`）、角色/模型/提供方徽章（区分官方模型与外部 API）、路径所有权范围、异常告警，以及一键操作（打开官方子代理对话、跳转至 Board 帖子或打开产物文件）。
- **Hive 蜂巢图（`Graph -> Hive`）**：将每个 depth-1 子树映射为一个蜂巢单元，实时展示工作节点、进度环、等待依赖箭头与消息连线。
- **Board 与子树频道（`#channels`）**：当 depth-1 Lead 派生 depth-2+ 子节点时，自动创建按子树隔离的频道（例如 `#lead-api`），使子树内部协作保持条理，同时根级协调保留在 `#main` 中。
- **Members 与外部 API 实时活动（`transcript.jsonl` 镜像）**：将隔离运行的外部 API 工作节点（同时支持免装 CLI 的桌面端 `language_server` 与 `agy` CLI 后端）的实时步骤日志镜像写入宿主对话的 `transcript.jsonl`，使外部 API 节点与官方模型一样展示实时步数、活跃状态、工具/思考动态以及错误或配额告警。
- **原生子代理谱系与整洁侧边栏**：官方模型工作节点通过 ConnectRPC `StartCascade(SUBAGENT, parentConversationId)` 创建，子节点严格归属于父对话之下，不会污染左侧顶级对话列表。

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
