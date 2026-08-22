[English](README.md) | 中文

# Bot Bureau —— 不同 AI Agent，协作为你服务

**Bot Bureau** 是一间运行在本机的 AI 事务所。你可以像给团队成员发消息一样交代工作；成员会独立完成多步骤任务，需要人工判断时再请求批准，记住长期偏好，把重复工作保存为定时例程，也能在群聊中互相分工。

项目由 Go 引擎和跨平台 Electron 客户端组成，可连接 Anthropic、OpenAI 兼容接口、Ollama 等模型服务。聊天、记忆、密钥和工作目录默认都留在本机。

~~~text
┌────────────────────────┐   HTTP + SSE   ┌────────────────────────────┐
│  Electron 桌面客户端   │ ◄────────────► │  Go 后端引擎               │
│  app/                  │   127.0.0.1   │  常驻成员 · agent loop      │
│  群聊/私聊 · 审批       │                │  审批门 · 记忆 · 例程       │
│  例程 · 新建成员        │                │  Anthropic / OpenAI 兼容    │
└────────────────────────┘                └────────────────────────────┘
~~~

## 功能概览

| 能力 | 说明 |
|---|---|
| 像团队成员一样发消息 | **群聊**中可以直接叫成员名字或 @；没有点名时由群里的第一位成员处理；说「大家」或 @all 则全员回应；其他成员只旁听上下文；**私聊**则是一对一独立工作 |
| 每位成员都有独立工作区 | 默认目录为 `data/workspaces/<bot>/`，也可以把你在对话中明确指出的现有目录加入；文件访问会检查边界，bash 仍受权限档位控制 |
| 常驻、多步骤执行 | 每位成员都是独立的 Go 工作协程和 agent loop，不会阻塞其他成员 |
| 需要人工判断时回来询问 | 非只读命令会暂停等待审批；侧栏可以一键批准或拒绝，并附可选原因 |
| 从空团队开始 | 首次启动没有成员和群聊，会依次介绍负责人身份、可用功能，再引导你聘请第一位成员；整个引导可以跳过 |
| 长任务显示进度 | 成员工作时，聊天里会显示“执行中 · N 步”，展开后可查看每一步 |
| 不必输入 @ | 在群聊中直接说 `scout 看一下` 等同于 `@scout`，不会误匹配更长的单词；说「大家」或 `@all` 则全员回应 |
| 浅色与深色 | 默认跟随系统，也可以在设置中固定外观 |
| 四档权限 | 每次询问、可写文件、自动工作、无需审批；团队有默认值，成员可以单独覆盖。越出工作目录的操作和插件调用只有在“无需审批”时才会跳过审批 |
| 记忆与例程 | 每位成员有 `MEMORY.md`；说“每隔 30 分钟……”即可保存定时例程，重启后仍然存在 |
| 任务看板与分工 | 成员通过 `message_bot`、`assign_task`、`update_task` 协作；每个子任务只有一个负责人，避免重复工作 |
| 跨服务商协作 | Claude、Grok、DeepSeek、Ollama 等成员通过 Go 消息总线交换普通文本，共享任务板和团队记忆 |
| 网页访问 | Claude 成员提供服务商侧 `web_search` / `web_fetch`；其他服务可通过 bash 访问网络，但会受审批规则约束 |
| 创建和编辑成员 | 在侧栏点击“＋ 新建 Bot”填写名称、人设和模型；悬停会话点铅笔即可修改名称、头像、人设、模型和权限，保存后按新配置重启 |
| 多个群聊 | 每个群聊有独立名称、头像、成员和上下文；默认群聊不能删除 |
| 订阅登录 | ChatGPT Plus/Pro 和 SuperGrok 支持 OAuth 设备码登录，不打开本机回调端口，令牌以 0600 权限保存在本机 |
| 模型动态获取 | 引擎向服务商请求当前模型列表；获取失败会显示错误并允许手动填写，不会偷偷使用过期的内置列表 |
| Markdown 消息 | 支持标题、列表、引用、代码块和 http/https 链接；链接交给系统浏览器打开，渲染只使用 DOM |
| 随时中断 | 成员跑偏时可以中止当前回合，不必一直等待 |
| 重启不丢历史 | 消息历史和每位成员的会话上下文都会持久化到 `data/events.jsonl` 与 `data/workspaces/<bot>/sessions.json` |
| API 密钥管理 | 可在新建成员对话框或设置中录入密钥，保存在 `data/keys.json`（0600），界面只显示掩码，并优先于环境变量 |
| 群成员管理 | 可在群聊设置中增删成员；不在群里的成员收不到该群消息，也不能被 @ 或分配任务，私聊不受影响 |
| 团队共享记忆 | `remember scope=team` 写入 `data/TEAM_MEMORY.md`，不同模型服务商的成员都能使用 |
| 中文 / English | 默认跟随系统；设置中可选“跟随系统 / 中文 / English”，界面、后端消息和成员系统提示词一起切换 |
| MCP 插件 | 插件面板支持本地 stdio 插件和远程 Streamable HTTP 连接器；成员按需订阅，非只读调用进入审批，配置保存在 `mcp.yaml` |
| 多设备 | 一台设备运行引擎，其他设备作为客户端连接；团队数据留在运行引擎的设备上，不经过云端 |

## 快速开始

需要 Node.js 22.12 或更高版本，以及 Go 1.26.6 或更高版本。仓库不提交后端二进制，npm 脚本会自动编译。

~~~bash
cd bot-bureau/app
npm install
npm start
~~~

首次启动会打开引导：确认你是负责人、了解这里能做什么，然后选择服务商、登录或填写密钥，再从引擎获取的列表中选择模型。跳过也可以，侧栏顶部的 ＋ 随时能新建成员。

可以试试：

- 在私聊中让成员写一个脚本；执行脚本时会弹出审批。
- 新建群聊，加入几位成员，然后说：`scout 查一下今天的 AI 新闻`（不必输入 @）。说「大家好，介绍一下自己」则全员回应。
- 对成员说“每隔 60 分钟检查 HN 首页”，创建一个定时例程。

## 自动化测试

Go 测试放在对应 package 旁边的 `*_test.go` 文件中；Electron 测试分别放在 `app/test/` 和 `app/e2e/`：

~~~bash
# Go 后端
cd backend
go test ./...
go test -race ./...
go vet ./...
go test ./... -coverprofile=/tmp/botbureau.cover
go tool cover -func=/tmp/botbureau.cover

# Electron 单元测试和覆盖率
cd ../app
npm ci
npm run test:unit
npm run test:coverage

# 真正启动 Electron + Go 后端的冒烟测试
npm run build:backend
BOTBUREAU_RUN_E2E=1 npm run test:e2e
~~~

Electron E2E 使用临时数据目录，不会连接真实模型、Telegram、OAuth 或 MCP 服务。没有设置 `BOTBUREAU_RUN_E2E=1` 时会跳过该测试；CI 会运行后端测试、race detector、静态检查、前端覆盖率和 Electron 冒烟测试。

架构和贡献流程见 [`docs/architecture.md`](docs/architecture.md)、[`docs/development.md`](docs/development.md)、[`docs/agent-runtime.md`](docs/agent-runtime.md) 与 [`CONTRIBUTING.md`](CONTRIBUTING.md)。在仓库根目录执行 `make test` 可运行快速检查，`make test-e2e` 可运行桌面冒烟测试。

## 数据保存位置

开发模式下（在仓库内执行 npm start），bots.yaml、mcp.yaml、data/ 和 connect.json 都位于仓库根目录，并且被 gitignore。可以复制模板，也可以直接在界面创建：

~~~bash
cp bots.example.yaml bots.yaml
cp mcp.example.yaml mcp.yaml
~~~

安装后的默认数据目录：

| 系统 | 目录 |
| --- | --- |
| macOS | ~/Library/Application Support/Bot Bureau/ |
| Windows | %APPDATA%\Bot Bureau\ |
| Linux | ~/.config/Bot Bureau/ |

目录大致如下：

~~~text
bots.yaml              团队成员定义
mcp.yaml               插件 / 连接器定义
connect.json           已保存的远程引擎地址和证书指纹
data/
  keys.json             API 密钥（仅当前用户可读）
  xai_oauth.json        SuperGrok 订阅令牌
  chatgpt_oauth.json    ChatGPT 订阅令牌
  token                 局域网配对码
  events.jsonl          追加式消息历史
  workspaces/<bot>/     成员的工作目录、MEMORY.md、sessions.json
  TEAM_MEMORY.md        团队共享记忆
  tasks.json            任务看板
  routines.json         定时例程
  groups.json           群聊和成员关系
  settings.json         语言、权限和群聊设置
~~~

安装包内的只读资源与可写数据分开保存，后端二进制也随客户端打包，因此只下载客户端也能正常启动；首次运行会创建空配置，不会把开发机的 bots.yaml、成员或密钥带进来。

### 把数据放到其他目录

BOTBUREAU_DATA_DIR 会整体迁移上述配置和数据。客户端与后端都会读取它；后端命令行的 -data 参数优先级更高。

~~~bash
# 工作与个人两套互不影响的配置
BOTBUREAU_DATA_DIR=~/BotBureau/work npm start
BOTBUREAU_DATA_DIR=~/BotBureau/personal npm start

# 在临时目录运行，保持仓库干净
BOTBUREAU_DATA_DIR=/tmp/bb-dev npm start
~~~

目录不存在时会自动创建，终端会打印最终位置。迁移目录也会更换引擎身份，因为配对码保存在 data/token；其他设备需要重新配对。

## 界面语言

客户端默认读取 Electron 的系统语言，后端默认读取 `LANG` / `LC_ALL`；设置中可以选择“跟随系统 / 中文 / English”。选择会同时影响：

1. 客户端界面文字；
2. 引擎的审批提示、错误、例程和额度提醒；
3. 成员的系统提示词以及它回复用户时使用的语言。

独立运行后端时，语言来自 `data/settings.json`；没有明确设置时再读取 `LANG` 或 `LC_ALL`。成员的系统提示词也会跟随语言切换，并要求成员使用用户的语言回复。`bots.yaml` 中的 role 和 description 是你的数据，不会被自动翻译；需要英文版本时可增加 `role_en` 和 `description_en`。

## 权限档位

| 档位 | 免审批 | 仍需审批 |
| --- | --- | --- |
| 每次询问（默认） | 只读命令、读取文件、只读插件 | 写文件、命令、写入型插件 |
| 可写文件 | 以上 + 工作目录内的文件写入 | 命令、插件 |
| 自动工作 | 以上 + 工作目录内的普通命令 | 越出工作目录的动作、插件 |
| 无需审批 | 全部操作 | 无 |

工作目录是成员自己的目录，加上你在消息中明确指定的、确实存在的目录。路径访问会逐一检查这些根目录。bash 的审批仍是启发式扫描；在有操作系统后端时，进程会被沙箱包裹，写入出不去工作目录和 `/tmp`。详见 [`docs/sandbox.md`](docs/sandbox.md)。

引擎会直接执行明确的只读命令，包括管道、命令序列和不带副作用的 find。写入真实文件的重定向、命令替换、网络访问、越出工作目录的路径，以及 find 的 -delete / -exec 等动作会进入审批。只有“无需审批”会放行所有边界外动作和非只读插件调用。

设置中的权限是团队默认值，每位成员都可以单独覆盖，修改立即生效。任何越出工作目录的操作都需要审批（“无需审批”除外）；非只读 MCP 调用也只有在“无需审批”时才会直接执行。无效或空的权限值会回退到最保守的档位，不会意外变成“无需审批”。

## 连接模型

界面按三步连接模型：

1. 选择服务商：Anthropic Claude、OpenAI / ChatGPT、xAI Grok、DeepSeek、Kimi（开放平台按量）、Kimi Code（会员）、Ollama、自定义服务或 Fake 离线模型；
2. 选择登录方式：使用订阅登录，或粘贴 API key。订阅登录使用设备码流程，不需要本机回调端口；密钥会保存到本机密钥库，优先于环境变量。Kimi Code 要贴 Code 控制台的密钥，不是开放平台按量 Key；
3. 选择模型：引擎调用服务商的模型列表接口，选择当前可用模型。列表不可用时会显示错误并允许手动填写，不会替换成猜测的模型列表。

### bots.yaml 示例

~~~yaml
bots:
  - name: chief
    role: Lead
    description: Breaks work down, assigns it, and reports back.

  - name: gpt
    role: Advisor
    description: Offers a second opinion.
    provider: openai
    provider_id: openai
    auth: chatgpt
    model: gpt-5.1-codex

  - name: grok
    role: Researcher
    description: Digs up sources and compares them.
    provider: openai
    provider_id: xai
    auth: key
    model: grok-4
    base_url: https://api.x.ai/v1
    api_key_env: XAI_API_KEY

  - name: local
    role: Local assistant
    description: A small model on this machine.
    provider: openai
    model: qwen3:14b
    base_url: http://127.0.0.1:11434/v1

  - name: demo
    role: Demo
    description: An offline echo bot.
    provider: fake
~~~

默认 provider 是 anthropic；它提供服务商侧 `web_search` / `web_fetch`，并支持拒答回退。OpenAI 兼容服务也能通过 bash 访问网络，但会受审批规则约束。

## 代码结构与本地化约定

~~~text
backend/                 Go 引擎
  main.go                 启动与组装
  internal/
    i18n/                  T() 与 locales/zh.json
    config/                bots.yaml、设置、权限
    secret/                密钥库、xAI / ChatGPT OAuth
    model/                 服务商实现与模型目录
    plugin/                MCP stdio / Streamable HTTP 客户端
    engine/                消息总线、成员、工具、任务板、记忆、例程
    bridge/                Telegram
    netx/                  mDNS、TLS、引擎锁
    httpx/                 通用 HTTP 辅助函数
app/                     Electron 客户端
  main.js                启动后端、窗口和多设备发现
  renderer/              原生 DOM 界面
    core/                 运行时、DOM、API 与时间工具
    views/                侧栏、聊天、任务、插件、设置等视图
    dialogs/              bot、群聊、引导对话框
    utils/                头像等纯 UI 工具
    locales/zh.js        中文翻译表
  test/                  Vitest 单元 / 集成测试
  e2e/                   Electron 冒烟测试
  scripts/               开发模式下修正 Dock 名称与图标
assets/make_icon.py      图标生成脚本
docs/                    架构、开发和插件开发说明
CONTRIBUTING.md          贡献指南
SECURITY.md              安全边界与漏洞报告
bots.example.yaml        成员配置模板
~~~

`engine` 中的文件相互协作紧密：消息总线持有成员工作器，工作器也要使用消息总线，因此保持在同一包中是设计选择，而不是为了偷懒。

用户文案统一用英文源字符串标识：Go 使用 `i18n.T`，渲染器使用 `t` 或 `data-i18n`，中文分别放在 `backend/internal/i18n/locales/zh.json` 和 `app/renderer/locales/zh.js`。占位符使用 `%s` 等格式，先查表再插值；找不到翻译时会回退到英文，不会显示空白。源码注释统一使用英文。

## 代码约定

所有用户可见字符串都先以英文写在代码中，再用英文源字符串作为键放进翻译表：

- Go：`i18n.T("Message is empty")` → `backend/internal/i18n/locales/zh.json`；
- 渲染器：`t("Search")` 或 HTML 的 `data-i18n="Search"` → `app/renderer/locales/zh.js`；主进程也读取同一张表；
- 使用 `%s` 等占位符，例如 `t("%s members", n)`，不要在查表前先插值。

## 外观与打包

界面支持跟随系统、浅色和深色。外观选择保存在客户端本地，不随远程引擎共享，因此同一个引擎连接多台设备时，每台设备可以有自己的外观。

图标由 `assets/logo.svg` 和 `assets/icon-layers/` 生成浅色、深色两套资源。macOS 仍使用带 Apple 图标网格边距的超椭圆；Windows / Linux 使用铺满画布的圆角超椭圆，外沿尺寸与原来的方图一致。应用运行时，主进程监听系统外观变化并切换 Dock / 任务栏图标；应用未运行时，macOS 使用打包时写入的固定图标。可选的 `assets/AppIcon.icon` 会在 `afterPack` 阶段由 `actool` 编译并写入包内，缺少完整 Xcode 时会跳过这一步而不影响构建。

桌面端使用 electron-builder 打包：

~~~bash
cd app
npm install
npm run dist:mac:arm64
npm run dist:mac:x64
npm run dist:mac:universal
npm run dist:win:x64
npm run dist:win:arm64
npm run dist:linux:x64
npm run dist:linux:arm64
~~~

三个桌面平台共用一个 Electron 客户端，Go 引擎作为子进程启动。打包后的引擎位于 `Resources/app.asar.unpacked/bin/`，必须放在 asar 外才能执行；构建脚本会分别编译 arm64 / amd64 并在 macOS 上用 `lipo` 合并。其他平台脚本见 `app/package.json`。

macOS 签名和公证由 electron-builder 配合 Developer ID 证书完成；本地复制或局域网分享不需要公证。开发模式下 Dock 名称由 `app/scripts/fix-electron-identity.js` 修正，图标缓存必要时可用 `killall Dock` 清除。

## 成员之间如何协作

不同服务商的成员可以无缝协作并共享记忆。协作发生在应用层：Go 消息总线传递普通文本，Claude 成员可以把任务交给 Grok 成员，所有成员也可以读写任务板和 `TEAM_MEMORY.md`。每位成员的会话历史仍独立保存，不会因为服务商不同而互相覆盖；API key 只决定使用哪一个模型，不改变协作界面。

避免重复工作的机制有四层：

1. 群聊路由：未被点名、未被指派的成员只读取背景，不会行动（说「大家」或 @all 时全员行动）；
2. 任务板：assign_task 为每个子任务指定唯一负责人，update_task 在开工和完成时公开状态；
3. 系统提示词：明确要求先看任务板，不重复接手别人正在做的事；
4. 可见性：任务板同时注入成员上下文并显示在侧栏。

## 多设备连接

引擎只有一个事实源：运行引擎的设备保存成员、聊天、记忆、插件、任务板和密钥，其他设备作为客户端访问它。

配对码只通过 `Authorization` 请求头传输。SSE 浏览器接口不能自定义请求头，因此客户端先用 POST `/api/sse-ticket` 交换一个十分钟有效、只能访问 `/api/events` 的短票据；配对码不会出现在 URL 或反向代理日志中。

本机模式也需要配对码：`127.0.0.1` 不是安全边界，本机打开的网页也能访问 localhost。配对码保存在 `data/token`（0600），网页无法读取，客户端可以。局域网发现通过 mDNS 完成，但发现是非阻塞的：窗口先打开，本机引擎照常启动；发现其他引擎后只显示提示，选择“配对”时才要求输入配对码。除发现探针外，所有接口都会验证 Authorization 请求头。

同一局域网内，设备点对点连接，不经过云端：

- 一台设备运行引擎，其他设备启动 Bot Bureau 后通过 mDNS 自动发现它；连接后客户端可以聊天、审批、管理插件和成员，状态只有一份，不会发生同步冲突；
- 配对码在引擎设备的设置中查看，其他设备第一次连接时输入一次即可；
- 将 `bots.yaml`、`mcp.yaml` 和 `data/` 放进 iCloud / Syncthing 可以冷迁移引擎。`data/engine.lock` 和心跳会阻止两台设备同时运行，崩溃遗留的锁 30 秒后过期；
- 调试时可用 `BOTBUREAU_BACKEND_URL=http://<ip>:<port> npm start` 直接进入客户端模式，或用 `BOTBUREAU_LOCAL_ONLY=1` 关闭广播；
- macOS 首次运行可能要求允许本地网络访问或接受传入连接，需要允许。

发现过程不会阻塞启动：窗口打开后才扫描局域网，另一个引擎出现时在设置按钮上方显示提示；选择“配对”才会询问配对码，“暂不”会记住这台设备而不再询问。

跨网络时推荐先使用 Tailscale 等虚拟组网，再在设置中填写 `http://100.x.y.z:8973`；流量通过 WireGuard 端到端加密。也可以使用 SSH 隧道，或把配置目录放在 Syncthing / iCloud 中做冷迁移。不要把 8973 端口直接暴露到公网。

### 公网服务器

如果必须跨公网连接，可开启内置 TLS：

~~~bash
# 方案一：内置 TLS，不需要域名
./botbureau-backend -port 8973 -tls auto -config bots.yaml -mcp mcp.yaml -data data

# 方案二：使用域名和反向代理（Caddy 会自动申请 HTTPS）
caddy reverse-proxy --from botbureau.example.com --to localhost:8973
~~~

客户端会记住首次连接看到的 SHA-256 证书指纹；之后指纹变化会拒绝连接并提示可能存在中间人攻击。也可以使用自己的 `-tls cert.pem:key.pem` 证书。即使启用 TLS，也仍需配对码；明文 HTTP 只适合可信局域网或虚拟网络。

## Telegram 桥接

1. 在 Telegram 找 @BotFather，执行 /newbot 并保存 token；
2. 在 Bot Bureau 设置的密钥页保存为 TELEGRAM_BOT_TOKEN，启用 Telegram；
3. 给机器人发送 /start；第一个发送者会独占绑定。

常用命令：

- `/bind group`：连接团队群聊；
- `/bind scout`：把会话切到某位成员的私聊，普通消息会直接发给它；
- `/bind`：查看当前绑定；
- `/dm coder 内容`：一次性给某位成员发消息，不改变当前绑定；
- `/bots`：查看团队。

群聊中仍然可以使用 `@scout` 点名；命令审批会以 Telegram 内联的“✅ 批准 / ❌ 拒绝”按钮送达。无论当前绑定到群聊还是私聊，额度耗尽提醒都会发送。桥接使用官方 Bot API 长轮询，不要求公网 IP。

个人账号没有官方微信机器人 API，第三方逆向方案可能违反服务条款并导致账号被封，因此暂不提供微信桥接。Discord、Slack、飞书等可以参照 `backend/internal/bridge/telegram.go` 的模式接入。

## 引擎不可用时

单引擎架构下，引擎离线时团队也会离线，客户端只是视图。客户端连续 3 次重连失败后会显示离线提示，并提供“切换到本机引擎”或“重启本机引擎”；引擎恢复后会自动继续连接。也可以在常开设备上单独运行后端：

~~~bash
./app/bin/botbureau-backend -port 8973 -config bots.yaml -mcp mcp.yaml -data data
~~~

如果数据目录位于同步文件夹，engine.lock 会阻止两台设备同时运行引擎；锁过期后可以在另一台设备接管。会话上下文只保存在引擎内存中，重启会清空；需要长期保留的内容应使用 remember 写入记忆。

推荐让 Mac mini、NAS 或树莓派等设备一直运行后端，并用 `nohup`、`launchd` 或 `systemd` 设置开机启动。若原设备不可用，等待引擎锁过期后，另一台设备可以“在本机运行”并接管成员、插件、记忆、任务板、例程和密钥；对话上下文仍会在重启后清空。

## MCP 插件

~~~yaml
servers:
  - name: fs
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Documents"]
  - name: atlassian
    url: https://mcp.atlassian.com/v1/mcp/authv2
    auth: oauth
~~~

插件面板内置一键安装目录（GitHub、Slack、Figma、Jira、Notion、Google Drive、Stripe、Sentry 等）。安装只是把连接器登记到团队；还需要在成员设置里订阅（例如 `mcp: [fs, atlassian]`）。

成员也可以在聊天里启用目录中的连接器：`list_connectors` 查看可用项，`enable_connector`（经你批准）会按需安装并为当前成员订阅。需要 OAuth 的连接器随后会打开浏览器授权。

工具名会以 `mcp_<plugin>_<tool>` 的形式提供给订阅该插件的成员。MCP 放在引擎层而不是某个服务商 API 内，因此 Grok、DeepSeek、Ollama 等所有成员都能使用同一套插件。远程连接器可以使用静态 bearer token 或 OAuth（`auth: oauth`，在插件面板点「授权」，或从目录安装后自动打开浏览器）。多数 OAuth 连接器会走资源元数据发现、授权服务器发现、动态客户端注册、PKCE 和自动刷新。GitHub 的授权服务器没有注册端点，因此内置了公开 client id，走 GitHub 的 device-code（打开浏览器页面，和 ChatGPT / SuperGrok 登录同一思路）——不必自己建 OAuth App，也不必粘贴 PAT。Atlassian、Linear、Notion、Sentry 以及 GitHub 这类不发静态令牌的连接器只能走 OAuth。

工具很多时，可以在插件面板选择子集，选择结果会写入 `mcp.yaml`；留空表示全部工具，后续新增工具也会自动加入。本地插件只继承受限环境变量白名单；如果需要其他变量，要在 `env:` 中显式声明，避免插件继承 `SSH_AUTH_SOCK` 或 shell 中导出的密钥。插件进程崩溃后引擎会退避重连，服务端发送 `tools/list_changed` 时也会刷新工具列表。

## Skills（Agent Skills）

技能是“如何完成一类工作”的书面流程，不是插件提供的工具。目录示例：

~~~text
data/skills/release-notes/
  SKILL.md          # YAML frontmatter 包含 name 和 description，正文写给模型
  build.py          # 可选脚本和配套材料
~~~

SKILL.md 的 frontmatter 必须包含 name 和 description。系统提示词只带每个技能的一行摘要；成员判断适用后才调用 `read_skill` 读取全文：安装 50 个技能也只增加 50 行提示词，而不是把 50 份文档全部塞进去。description 是成员判断技能是否适用的唯一依据，因此要写清楚适用场景。

新团队会得到一套起步技能：改代码、跑测试、调研、PDF、Office 文档（`edit-code`、`verify`、`research`、`pdf`、`documents`）。`read_file` 会从 PDF / Word / Excel / PowerPoint 抽出文本，包括已经放进 `inbox/` 的聊天附件。你已经有的技能目录不会被覆盖；缺少的起步技能会在启动时补上。

技能由整个团队共享，不需要逐个分配给成员。技能附带的脚本会通过 bash 按完整路径运行；脚本位于工作目录之外，因此需要审批，这也是对待第三方代码的安全边界。

## 插件包

Bot Bureau 不另造插件格式，直接安装 Claude / Codex 的 `.claude-plugin/plugin.json` 包。这样开发者只需编写一次插件，就能让 Claude Code、Codex 和 Bot Bureau 共用。

插件可以从面板用 git 地址或本机目录安装，也可以直接复制到 `data/plugins/`；文件系统是唯一事实源，不维护额外索引。带有 `.claude-plugin/marketplace.json` 的市场仓库也支持安装：粘贴市场地址后可以选择其中的具体插件。

| 包内内容 | Bot Bureau 的处理 |
| --- | --- |
| mcpServers 或根目录 .mcp.json | 注册为 MCP 插件，并按包名隔离 |
| skills/ | 合并到技能库 |
| agents/*.md | 转为可新建的成员模板，正文追加到系统提示词 |
| commands/ | 不支持；Bot Bureau 没有斜杠命令 |
| hooks/ | 不支持；安全控制由权限档位负责 |

安装后，不支持的内容会明确列出，而不是静默忽略。点击已安装插件的“更新”会在原位置升级：新增的 MCP 服务会加入，已经移除的会删除，现有工具选择和授权会保留，不会重置插件设置。`agents/*.md` 会成为真正的成员模板，拥有自己的工作目录、记忆和任务板条目，可以被 @ 和分配任务。

## 额度提醒

当服务商返回“余额 / 额度耗尽”类错误时（区别于普通的 429 限流）：

1. 当前会话立即出现错误气泡，并保留服务商原始信息；
2. 所有会话都会显示全局额度提醒，Telegram 也会收到；
3. 每个服务商十分钟内最多提醒一次。

目前会识别 Anthropic 的余额 / 账单错误，以及 OpenAI 兼容服务的 `insufficient_quota`、`exceeded your current quota` 和 HTTP 402。额度耗尽不会自动暂停例程；补充额度后任务会继续，或者手动删除例程。

## 实现说明

- Claude 使用官方 `anthropic-sdk-go`（beta endpoint），支持 `claude-opus-5`、adaptive thinking、服务商侧 fallback、拒答时整回合回滚，以及 `max_tokens` 截断后的成对 tool_result 修复；OpenAI 兼容服务使用原生 `net/http`，支持历史和工具双向转换。
- 只读 shell 命令和管道可直接执行；写入重定向、命令替换、联网和带副作用的 find 进入审批。
- 消息历史追加写入 `data/events.jsonl`；内存只缓存最近尾部。群聊和私聊上下文分别保存在各成员的 `data/workspaces/<bot>/sessions.json`，过长上下文会在完整回合边界裁剪。
- 有操作系统后端时（macOS Seatbelt、Linux bubblewrap 或 Landlock），bash 跑在沙箱里；设置里可以跳过已包裹命令的审批，并用 `unsandboxed=true` 在宿主机上重试。没有后端时仍只是工作目录检查加审批门。详见 [`docs/sandbox.md`](docs/sandbox.md)。
- 分发使用 electron-builder；npm start 是开发入口。

## 未来计划

下一步的引擎工作是 agent 运行时：按行读取和补丁编辑、上下文压缩、私聊中的计划门、两段式记忆，以及每个服务商都能用的搜索。顺序、文件清单和验收标准见 [`docs/agent-runtime.md`](docs/agent-runtime.md)。下面的平台项仍在路线图上，但不能代替这项工作。

未来面向用户的路线图包括：

- **消息提醒**：为新消息、审批请求和其他需要关注的事件提供可选的桌面与移动端提醒；
- **原生移动端（iOS / Android）**：同时支持独立引擎设备模式和客户端模式。作为引擎设备运行时，手机或平板可以在本地运行团队，负责模型调用、bash、插件、聊天、记忆、任务板和审批；作为客户端运行时，则连接到其他设备上的引擎。
- **watchOS companion**：查看任务进度、接收审批推送并批准 / 拒绝工作。watchOS 只作为客户端运行，不运行引擎。

| 平台 | 形态 | 状态 |
|---|---|---|
| macOS / Windows / Linux | Electron，内嵌引擎 | ✅ 已发布 |
| Telegram | 桥接，任何能运行 Telegram 的设备都可使用 | ✅ 已支持 |
| iOS / Android | 可独立运行引擎，也可连接其他引擎的原生应用 | 计划中 |
| watchOS | 用于查看进度和处理审批的 companion 客户端 | 计划中 |

原生移动端上线前，可以使用上文的 Telegram 桥接从手机聊天并处理审批。
