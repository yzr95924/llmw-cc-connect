# agent/llmw — llmw workspace 整合 agent

把 `llm-workspace-cli`（llmw）的 workspace / wiki 作为一等 agent 接入 cc-connect：
IM 端发 `/wikis` 列出 wiki，回复序号或 `/enter <wiki名>` 进入，后续消息即与该 wiki
的 agent 对话（cwd=wiki 目录，model 用 llmw 写的 overlay）。

**设计文档**：`raw/discussions/cc-connect-llmw-integration-design.md`（设计 v2）。
**实施任务书**：`raw/discussions/cc-connect-llmw-integration-tasks.md`。

## 原理（30 秒版）

- 本 agent **不直接管理 claude/opencode 进程**——包装仓库内已有的 `agent/claudecode`
  和 `agent/opencode`，协议 / 权限 / 流式 / 生命周期全部委托。
- 进入 wiki = 用 `work_dir=<wiki 绝对路径>` 构造内层 agent 并 `StartSession`。
  claude CLI 在 cwd=wiki 时**原生读取** `.claude/settings.local.json`、opencode 原生
  读取 `opencode.json`（都是 llmw 写的 overlay）——**本 agent 不解析 overlay，
  API key 不经手 cc-connect**（H7 已实测：claude init 事件 model=glm-5.2[1m] 与
  overlay 一致）。
- 会话是**稳定 facade**：wiki 切换时内层 session 换掉，对外事件通道不变；
  agent session ID 编码为 `llmw:<wiki>:<innerID>`，cc-connect 重启后自动恢复 wiki 绑定。

## 配置

`config.toml` 项目段：

```toml
[project.agent]
type = "llmw"
[project.agent.options]
backend = "claude"    # "claude"（默认）或 "opencode"
```

- `backend` 全 workspace 统一（v1 不支持按 wiki 混用）。
- `work_dir` / `model` 两个 key 会被本 agent **忽略**（model 真源 = llmw overlay）。
- 其余 options（`mode` / `cmd` / `env` 等）透传给内层 agent。

## 使用

| IM 输入 | 行为 |
| --- | --- |
| `/wikis` | 列出 workspace 的 wiki（序号 + display_name + model）；TG 命令菜单可见（CommandProvider）|
| 回复 `1` | 进入第 1 个 wiki（列表 60s 内有效）|
| `/enter foo` | 按 name / display_name 进入 wiki foo |
| 普通消息 | 发给当前 wiki 的 agent |

进入/切 wiki 的回执 ≤3s（claude 冷启动主导，超 5s 报错）。

## 依赖与环境

- `llmw` 在 PATH（`llmw list --json` 可用）；workspace 根 = `$LLMW_WORKSPACE` 或
  `~/yzr-llm-wiki-workspace`（需含 `workspace.toml`）。
- `enter_byobu=true`（`workspace_local.toml`）时，进入 wiki 会顺带
  `llmw wiki --name=X enter` 同步 byobu 窗口并刷新 overlay；关闭或失败时 IM 侧
  **降级继续**（只告警，不影响进入）。
- 内层 CLI：`claude` / `opencode` 需已安装（启动失败会 IM 报错）。

## 已知限制（v1）

- 纯文本命令交互，无 IM 按钮（Q3 已确认；AskUserQuestion 借壳按钮列为将来增强）。
- `backend` 不支持按 wiki 混用（Q1）。
- opencode backend 下：IM 侧会话与 byobu TUI 会话同目录共享 opencode 的 session
  存储，`opencode` TUI 里能看到 IM 侧产生的 session（Q2 已接受）。
- cc-connect 重启后恢复 wiki 绑定，但**内层对话不恢复**（新会话；除非 facade ID 里
  的 innerID 恰好可 resume——claude 会话 id 在重启后有效时可恢复）。
- 引擎的 `/list`（会话列表）对 llmw 显示为空；`/model` / `/compress` 等依赖
  内层可选接口的命令在本 agent 会话上不可用（model 控制走 llmw，C6）。
- Web 管理界面的 agent 下拉框不含 "llmw"（前端硬编码列表，未改）。

## 故障排查

| 症状 | 检查 |
| --- | --- |
| `/wikis` 报 "llmw 不可用" | `which llmw`；`llmw list --json` 手动跑；`$LLMW_WORKSPACE` 是否指向含 workspace.toml 的目录 |
| 进入报 "无法启动 agent" | `which claude` / `which opencode`；`claude --version` |
| 进入报 "无法启动 agent 会话" | 看 cc-connect 日志里内层 stderr（`journalctl -u cc-connect`）|
| byobu 窗口没出现 | `byobu list-windows -t llm_workspace`；`workspace_local.toml` 是否 `enter_byobu = true`（linear 模式不会建窗口，IM 侧不受影响）|
| 切 wiki 慢 | claude 冷启动 1-3s 正常；超 5s 看日志 |
| 重启后进了旧 wiki | 预期行为（session ID 恢复绑定）|
