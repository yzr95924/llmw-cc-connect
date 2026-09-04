# agent/llmw — llmw workspace 整合 agent

把 `llm-workspace-cli`（llmw）的 workspace / wiki 作为一等 agent 接入 cc-connect：
IM 端发 `/llmw list` 列出 wiki，回复序号或 `/llmw enter <wiki名>` 进入，后续消息即与该 wiki
byobu 窗口里的 opencode TUI 对话——主机 `llmw wiki enter` 与 IM 进入汇合到**同一个窗口**，
主机侧与 IM 侧看到完全一致的状态（`llmw status` 全可见）。

**后端唯一**：v3 pane driver 只支持 opencode（llmw 的 model overlay / skill 生态都在
opencode 上）；`backend` option 写其他值会在启动时显式报错。

## 原理（30 秒版）

- 本 agent **不 spawn 任何 headless 进程**——直接驱动 llmw 建好的 byobu 窗口里的
  opencode TUI（pane driver）。
- **输入**：tmux `load-buffer` + `paste-buffer -p` + Enter（bracketed paste 保多行完整）。
- **回合完成检测**：轮询 `capture-pane`，`esc interrupt` 忙标记出现过后消失且画面
  稳定 = 回合结束；权限弹窗在场也算忙（弹窗期间忙标记会消失）。
- **回复内容**：`opencode export <session>`（屏幕 capture 找不回滚走的内容）；
  会话经 `opencode session list --format json` 按目录发现、注入文本匹配确认后粘住。
- **渐进推送**：回合内每 2s 轮询 export 快照做 diff，工具事件（EventToolUse）与
  文本增量按 part 顺序推给引擎——TG 进度卡实时滚动，钉钉 StreamingCard 打字机生效。
- **权限弹窗**：TUI 弹窗（`Permission required` + Allow once / Allow always / Reject）
  被检测后以 IM 按钮呈现；用户选择经 Left/Right + Enter 按键回注（2026-08-30 实机探测）。
  **"Allow always" 有两级确认**（实机实证 2026-08-30）：第一级选 Allow always 后弹出
  `△ Always allow` / `Confirm / Cancel` 第二页（Confirm 预选中）——按键序列为
  Right+Enter，停 600ms，再 Enter 确认。第二页计入 busy 并集（锚点
  `This will allow the following patterns`），否则回合会被误判提前结束。
- **question 工具弹窗（L1，2026-08-30 实机探测）**：agent 用 question 工具提问时 TUI 弹
  模态选择框（问题 + `N.` 编号选项 + `Type your own answer` + footer `⇆ select / enter
  submit / esc dismiss`）。检测锚点 = footer 三词同行；弹窗计入 busy 并集（不误判回合
  结束、不注入按键——防输入污染），每个弹窗发一次 IM 通知（含问题预览），**回答在主机
  窗口操作**，完成后回合自动继续。上游 headless 对接（`opencode run`）无此问题——无
  TUI 即无弹窗，question 工具直接失败。L2（IM 序号回答：解析选项 → 回复数字 →
  Down×N+Enter）为未来工作。
- 会话是**稳定 facade**：wiki 切换时换 pane，对外事件通道不变；agent session ID
  编码为 `llmw:<wiki>:<pane>`。**绑定永远显式**：cc-connect 重启（含部署）后 facade
  重建为未绑定态，需 `/llmw enter`（或 `/llmw switch` → 序号）重新接入——StartSession
  不做隐式 resume，被 stop 的窗口永不复活（2026-08-30 拍板）。

## 配置

`config.toml` 项目段：

```toml
[project.agent]
type = "llmw"
[project.agent.options]
backend = "opencode"   # 唯一合法值（省略即默认）；写别的值启动报错
```

- `work_dir` / `model` 两个 key 会被本 agent **忽略**（model 真源 = llmw overlay）。
- 其余 options 一律接受并忽略（pane driver 无内层 agent 可透传）。

## 使用

**命令面（2026-08-30 硬切换）：每个命令是独立的 `/llmw_xxx` 自定义命令**（菜单条目 + 手敲均可达；菜单点击不能带参数，手敲 `/llmw_enter agent-tools` 的参数经模板追加进 parser）。唯一保留的手敲形式是裸 `/llmw`（=状态）；任何 `/llmw <子命令>` 手敲形式与 CLI 原生粘贴形式（`/llmw wiki --name=…`）都已移除，会收到指向下划线形式的提示。菜单共 10 条：`/llmw` `/llmw_list` `/llmw_enter` `/llmw_switch` `/llmw_stop` `/llmw_detach` + 内建 `/model` `/mode` `/compress` `/help`（其余 39 个内建命令经 `disabled_commands` 禁用）：

| IM 输入 | 行为 |
| --- | --- |
| `/llmw`（裸，菜单点击或手敲） | 主机窗口列表（每窗口一行：window · backend · state · ctx · up · idle；不用 Markdown 表格——钉钉 bot markdown 不渲染表格，Telegram 表格需 `<pre>` 块，纯文本行在所有平台都可读）+ 本会话绑定 + 用法行；state 为 llmw 的 ASCII 契约值（dead / shell / working / waiting / unknown）。“上下文”字段 = 该 wiki 最新 opencode 会话末条 assistant 消息的 tokens（total，缺省 input+cache）——用于判断是否需要 `/compress`；同 wiki 的 main/tg 窗口显示同一最新会话（近似值）；获取失败显示 `…`、dead 窗口不显示上下文而显示 `exited <dur> ago`；整个采样预算 30s、每窗口 8s，大 wiki 的 status 会慢几秒 |
| `/llmw_list` | 列出 workspace 的 wiki（序号 + display_name + model）；列表 60s 内有效 |
| 回复 `1` | 进入第 1 个 wiki（数字选择，非命令） |
| `/llmw_enter <名> [suffix]` | 按 name / display_name 进入 wiki（大小写不敏感，白名单解析）；**菜单裸点退化为 wiki 列表** |
| `/llmw_stop [名] [suffix]` | 关主机 tmux 窗口（`llmw wiki --name=X stop --yes`）。**一律先确认**（对齐 CLI `--yes`）：回复 `y` 才执行，其它输入取消（`--yes` 直通通道已随 CLI-native 形式移除）。opencode 会话落盘可重开续上。**裸命令 = 停当前绑定窗口**；**带参停的正是当前绑定的窗口时行为同裸命令**（绑定非 main（如 tg）时停完后**懒换绑 main**，绑定 main 时 wiki 会话下线且**本会话解绑**——否则下条消息会复活刚停的窗口）。停别的窗口不受影响；多窗口歧义时 llmw 错误原样透传。确认 **60 秒内有效**，超时后 y/yes 被消费并提示重新发起（不会漏进 opencode 聊天） |
| `/llmw_detach` | **安全退出**：解绑当前会话（关 pane 镜像、`status` 显示未绑定），主机窗口原样保留——不动主机资产时用（对比：stop 会关窗） |
| `/llmw_new` | **同窗口开启新会话**：注入 opencode TUI `/new`，窗口上下文清零，pane 绑定的 sticky session 重置（下条消息自动发现新 session）。**旧会话保留**在 opencode DB（`opencode session list` 可见、可续）——不删数据，无需确认。**生命周期不同于 /compact**：`/new` 不触发 busy 标记（2026-08-30 实测），走同步短等待（等空闲 footer `ctrl+p commands` 回现即确认，超时则提示"未确认执行"且不重置绑定）。引擎侧 transcript 也随新会话翻篇——缓解无限增长 |
| `/llmw_abort` | **中止当前窗口进行中的回合**：向 TUI 发 ESC（忙判定取自实时 capture，**主机手动开的回合也能中止**）。中止后回合以已产出内容收尾（export 里的部分回复照常回包，无内容则回中止通知）。**边界**：权限/question 弹窗期间 ESC 只关弹窗不中止回合——再发一次 `/llmw_abort` 即可；进度卡停止按钮仍只拆 IM 侧会话、不触窗口 |
| `/llmw_switch` | 列出 **live** 主机窗口（带序号 + 当前绑定标记），回复序号即换绑到该窗口（走 enter 路径：reattach 活窗，绝不建窗）。替代被禁用的引擎 `/switch`（其换绑依赖已删除的隐式 resume）。dead 窗口不列；无 live 窗口时提示 enter |
| `/mode build` / `/mode plan` | 切 opencode 子代理（Tab 键 + 底栏确认；回合中拒绝） |
| `/model` / `/model switch <名>` | 列出 / 切换模型（`opencode models` 清单按 `yzr*` 前缀过滤 + TG 按钮；经 TUI 模型对话框驱动，底栏确认；会话级，不改 overlay；回合中拒绝） |
| `/compress`（引擎内置） | 压缩当前窗口会话上下文（注入 opencode TUI `/compact`，等忙闲循环后回确认；TUI 命令不落库为消息，跳过回复提取；回合中拒绝）。**限制**：daemon 重启后需先发过一条普通消息（上游交互状态检查，无活跃会话时提示"请先发送一条消息"）。auto-compress 因 llmw 不上报 token 用量不会触发。注：若 `/compact` 未触发 TUI 忙标记（命令无效），将走到 10 分钟回合超时 |
| 普通消息 | 注入当前 wiki 窗口的 TUI；窗口忙则拒发提示稍后重发 |

旧命令 `/wikis`、`/enter` 已移除（v2.1 硬切）；启动时自动清理 agent 自己生成的 `wikis.md` 命令文件（用户改过的保留并告警）。

## 依赖与环境

- `llmw` 在 PATH（`llmw list --json` 可用）；workspace 根 = `$LLMW_WORKSPACE` 或
  `~/yzr-llm-wiki-workspace`（需含 `workspace.toml`）。
- **daemon 环境必须有 `HOME`**（systemd system service 默认只给 `USER` 不给
  `HOME`）：byobu 启动器要求当前用户 own `$HOME`，`HOME` 为空时直接拒绝——
  症状是 IM 回复序号进入 wiki 报
  `Cannot run byobu because [root] does not own []`（`llmw list`/`status` 不经
  byobu，照常工作）。agent 侧已兜底（子进程 HOME 为空时从 passwd 推导，
  `agent/llmw/env.go`），但仍建议 unit/env 文件显式设 `HOME`。手动部署路径
  （登录 shell 启动）天然有 HOME，不受影响。
- `enter_byobu=true`（`workspace_local.toml`）：IM `/llmw enter` 与主机
  `llmw wiki enter` 走同一命令，窗口天然共享。
- `opencode` ≥ 1.18 在 PATH（`opencode session list --format json` / `opencode export`）。
- 构建：`make build-noweb`（fork 惯例，web 管理界面排除出二进制）。

## 已知限制

- cc-connect 重启后绑定作废（显式重进），但**对话上下文留在 opencode session 里**：
  重进同一 wiki 窗口即续上（TUI 还在主机 byobu 里活着）。
- **TUI 锚点漂移被动自检**：attach 窗口时（`/llmw_enter` / `/llmw_switch` / 懒重建）
  检查三类锚点任一可读——busy 标记 / 空闲 footer（`ctrl+p commands`）/ 底栏模式段；
  全部读不到则 IM 告警"界面契约异常"（opencode 升级改 UI 或窗口里不是 opencode）。
  只做 attach 时检查，不持续轮询——运行中漂移仍靠升级验收清单。
- TG 菜单 12 条：`/llmw`（status）`/llmw_list` `/llmw_enter` `/llmw_switch`
  `/llmw_stop` `/llmw_detach` `/llmw_abort` `/llmw_new` + 内建 `/model` `/mode`
  `/compress` `/help`；其余 39 个内建命令经项目 config `disabled_commands` 全部禁用（会话管理类
  语义不合、特权类未配 admin、llmw 未实现的能力类如 /usage /reasoning、语义撞车类
  如引擎 /stop /new /cancel——中止走 `/llmw_abort`，不依赖引擎 /cancel）。`/llmw_enter`
  菜单裸点退化为 wiki 列表。
- **中止能力已补**（2026-08-30）：`/llmw_abort` 向 TUI 发 ESC 中止进行中回合
  （含主机手动开的回合）；替代被禁用的引擎 `/cancel`（其 CancelCommand 契约
  依赖未被采用）。剩余缺口：进度卡停止按钮仍只拆 IM 侧会话、不触窗口。
- **附件不支持**：TUI 提示框无法注入图片/文件——带附件的消息会显式回一句
  "暂不支持附件"，仅文本部分照发（不静默丢弃）。
- **权限弹窗 "Allow always" 两级确认已实证并修复**（2026-08-30 沙盒实机：第二页
  Confirm 预选中，裸 Enter 即确认；按键序列与 busy 处理见上文）。
- `/model` 清单来自全局 opencode 配置（不含 wiki overlay），且只列 `yzr*` 前缀
  的 provider（内置 opencode 目录 128 个模型对 workspace 是噪音）；切换是**会话级**
  （窗口重启后回到 overlay 默认）。模型切换依赖 TUI 对话框契约（`Select model`
  搜索只索引显示名、行按词边界匹配），opencode 大版本改版需重新探测。
- 权限弹窗依赖 TUI 文案契约（`Permission required` / `Allow once`），opencode
  大版本改版需重新探测按键语义。
- Web 管理界面的 agent 下拉框不含 "llmw"（fork 按 `build-noweb` 构建，web 整体不启用）。

## 运维

> 部署/安装/配置脚本**不放本仓**（fork 最小分叉面）——日常走 yzr-agent-tools 的
> `cc-connect-mgr` 工具（`install` / `config` / `upgrade` / `uninstall`，
> 含二进制来源校验、npm 制品安装、systemd 看门狗、secrets 走 0600 env 文件，
> 详见该工具 README）。以下手动命令仅作工具不可用时的兜底。

**凭据**：bot token 放 `/root/.cc-connect/env`（`chmod 600`）——不要写在命令行里
（会进 shell history 与 ps）：

```bash
printf 'TELEGRAM_BOT_TOKEN=123456:ABC...\n' > /root/.cc-connect/env && chmod 600 /root/.cc-connect/env
```

钉钉凭据不走 env——`config.toml` 里 `[[projects.platforms]]` 块配 `client_id` /
`client_secret`（见上游 `docs/dingtalk.md`）。

**看门狗**（推荐 systemd，单元内容）：

```ini
# /etc/systemd/system/cc-connect.service
[Unit]
Description=cc-connect daemon (llmw pane driver fork)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/root/cc-connect/cc-connect
WorkingDirectory=/root
# systemd system service 不设 HOME，而 byobu/llmw/opencode 都需要它
# （agent 侧有 passwd 兜底，显式设置最稳）
Environment="HOME=/root"
EnvironmentFile=/root/.cc-connect/env
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now cc-connect
# 之后的部署：make build-noweb VERSION=... && systemctl restart cc-connect
```

未启用 systemd 时维持手动部署：`set -a; . /root/.cc-connect/env; set +a;
pkill -9 -x cc-connect; cd /root && setsid nohup /root/cc-connect/cc-connect >
/tmp/cc-connect-daemon.log 2>&1 < /dev/null &`。**注意**：token 若曾在命令行
明文出现过（history / 会话记录），建议去 BotFather 轮换一次。多机并存时每机
独立 bot token（同一 token 被两个 daemon 轮询会互踢丢消息）。

## 发布

tag 触发 GitHub Actions（`.github/workflows/release.yml`，已配置）全自动出包：

```bash
git tag v1.5.0-llmw.2        # 规范 vX.Y.Z-llmw.N（baseline + fork 序号）
git push origin v1.5.0-llmw.2
```

Actions 做四件事：全平台构建（`make release-all`）→ GitHub Release 资产
（tar.gz/zip/checksums）→ npm 发布 `@yzr95924/llmw-connect`（身份注入，
`npm/package.json` 在 git 里与上游逐字节一致）→ 包内 `install.js` 安装时从
Release 下载对应平台二进制。

目标机安装：

```bash
npm install -g @yzr95924/llmw-connect
cc-connect --version
```

前置：GitHub 仓库 secrets 需有 `NPM_TOKEN`（npm 账号 access token）；包为
scoped public（公网可见，内容仅二进制下载器 + 文档，无机密）。

## 上游升级验收清单

本包与上游的耦合几乎全部是**行为契约**（编译不报错、测试不红——`llmwCmd` 测试
helper 手工构造展开文本，绕过了真实引擎），升级后按此清单实机验收。

### cc-connect rebase 后

| 契约接缝 | 验证动作 | 期望 |
| --- | --- | --- |
| ExpandPrompt 模板展开（sentinel 透传 + 参数拼接） | TG 菜单点 `/llmw_list`；手敲 `/llmw_stop foo` | 出 wiki 列表；出确认提示（若原文被当聊天转发给 opencode = sentinel 契约已变） |
| 权限事件流（EventPermissionRequest / RespondPermission） | 触发一次权限弹窗（如让它写 cwd 外文件），点 IM 按钮 | 弹窗被按键回答回合继续；含 Allow always 两级 |
| disabled_commands 配置语义 | 重启后看 TG 菜单 | 恰好 12 条；**上游新增内建命令会自动出现**（黑名单漂移）——对照 config.toml 补禁用 |
| session 持久化恢复（StartSession 传旧 id） | 重启 daemon 后在 TG 发消息 | 未绑定提示（无隐式 resume），`/llmw_enter` 重进正常 |
| 自定义命令注册（CommandDirs → 平台菜单） | 重启后看菜单 | `/llmw_xxx` 7 条均在 |

另跑：`go test -tags no_web -race -timeout 300s ./agent/llmw/`。

### opencode 升级后

**第一步先跑机器门禁**（沙盒 tmux + 真实 opencode，走生产代码路径，四阶段：TUI
启动 / 锚点自检 / 回合回环 / 权限弹窗 / question 通知，约 40s、消耗少量 token）：

```bash
go test -tags live -run TestLivePaneSmoke -v -timeout 600s ./agent/llmw/
```

红了再看锚点细节。TUI 屏幕文本锚点（大版本改版需重新探测，锚点定义都在
`pane_inner.go` 顶部常量/注释）：`esc interrupt`（busy）、`Permission required` /
`Allow once`（权限页 1）、`This will allow the following patterns`（权限页 2）、
`⇆ select / enter submit / esc dismiss`（question 弹窗）、`Select model`（模型
对话框）、`Build · <model>`（底栏）。单元测试只喂合成帧、升级时不红——live 冒烟
才是对真实 TUI 的把关。人工冒烟（冒烟测试没覆盖的部分）：`/model` 切换、
`/llmw_abort` 中止、点一次 Allow always（两级确认）。任一行为异常即重新沙盒探测
（`tmux new-session -d -s llmwprobe -c /tmp/opencode/probe 'opencode'`，勿碰生产窗口）。

## 故障排查

| 症状 | 检查 |
| --- | --- |
| `/llmw list` / `/llmw status` 报 "llmw 不可用" | `which llmw`；`llmw list --json` 手动跑；`$LLMW_WORKSPACE` 是否指向含 workspace.toml 的目录 |
| 回复序号进入报 `建立窗口失败 … Cannot run byobu because [root] does not own []` | daemon 环境 `HOME` 缺失（`tr '\0' '\n' < /proc/$(pgrep -x cc-connect)/environ \| grep ^HOME=`）；unit/env 文件补 `HOME=/root` 后 restart。旧版本还会留下假绑定（status 显示已绑定、再回数字提示"已在 wiki"）——升级到带回滚的版本或 `/llmw_detach` 解掉 |
| 进入报 "agent 不可用" | `which opencode`；`opencode --version`；主机 `llmw wiki --name=X enter` 手动跑看报错 |
| 消息发出无回复 | `llmw status` 看窗口 state；cc-connect 日志 grep `llmw pane`；窗口是否被主机占用（busy 闸门会拒发） |
| 回复"会话末尾不是本回合输入" | 主机在共享窗口插话触发 sticky echo 防错发保护；稍后重发即可 |
| 权限按钮点了没反应 | 看窗口弹窗是否已被主机手动回答（此时 IM 按钮变 no-op 属预期） |
