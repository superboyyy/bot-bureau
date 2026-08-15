[English](README.md) | 中文

# Bot Bureau — 常驻本机的 AI 同事团队（Go + Electron）

**Bot Bureau** 是一间跑在你自己机器上的「AI 事务所」：一支常驻的 AI 同事团队，你像给同事发消息一样派活，他们独立完成多步骤任务、需要人工决策时回来找你审批、记住你的偏好、把重复的活存成定时例程，彼此还能在群聊里分工协作。

**Go 引擎 + Electron 跨平台桌面客户端**，接哪家模型都行；数据、记忆、工作目录全在本地。

```
┌────────────────────────┐   HTTP + SSE   ┌─────────────────────────────┐
│  Electron 桌面客户端    │ ◄────────────► │  Go 后端引擎 (backend/)      │
│  app/                  │   127.0.0.1    │  bot 常驻协程 · 智能体循环    │
│  群聊/私聊 · 审批面板   │                │  审批门 · 记忆 · 定时例程     │
│  例程管理 · 新建 Bot    │                │  Anthropic / OpenAI 兼容      │
└────────────────────────┘                └─────────────────────────────┘
```

## 功能

| 能力 | 实现 |
|---|---|
| 像同事一样发消息 | **群聊**（@点名派活，不点名默认给 chief；bot 协作全程可见）+ **私聊**（一对一独立干活） |
| 每个 Bot 自己的电脑 | 每个 bot 独立工作目录 `data/workspaces/<bot>/`，bash / 文件读写限制在内 |
| 自主多步任务、always-on | 每个 bot 是常驻 goroutine，各自跑智能体循环，互不阻塞 |
| 需要人工决策时回来找你 | 非只读操作挂起等审批，就地一键 批准 / 拒绝（可附原因） |
| — | **空手起步**：出厂没有 bot、没有群聊；首次打开走三步引导（你是老板 → 能做什么 → 请第一位同事），可跳过 |
| — | **长思考有反馈**：bot 在跑时对话里挂一个打字气泡，旁边一行小字「正在干活 · N 步」，点开看每一步在做什么 |
| — | **不用 @ 也能点名**：群里直接叫名字（`scout 查一下`）等同于 `@scout`；不会误命中 `scouting` 这类更长的词 |
| — | **深浅色**：跟随系统，也可以钉死 |
| — | **权限档位**：每次都问 / 可改文件 / 自动干活 / 完全放开，全队一档、每个 bot 可另设；越界操作和插件调用只有「完全放开」才免审批 |
| 记住偏好 | 每 bot 一份 `MEMORY.md` 长期记忆，跨会话注入 |
| 例程 | 说"每隔 30 分钟…"自动存成例程，重启后仍在，侧栏可删 |
| 多 Bot 协作 | 群聊里 bot 互相转交任务（`message_bot`），其他 bot 能"听到"群聊内容 |
| 跨网页干活 | Claude 系 bot 自带服务端 `web_search` / `web_fetch` 联网 |
| — | **自建 Bot**：UI 里点"＋ 新建 Bot"填名字/人设/模型即可，落盘 `bots.yaml` |
| — | **随时改 Bot**：会话列表悬停点铅笔（或点标题栏头像）改显示名、头像、人设、模型与 API；保存后该 bot 带新配置重启 |
| — | **群聊也能改**：群名与群头像可自定；不设就用「群聊」+ 前两位成员的脸叠放 |
| — | **多个群聊**：侧栏顶部"＋"建群，每个群独立名字/头像/成员，各自一套上下文；默认群不可删 |
| — | **用订阅代替 API key**：ChatGPT Plus/Pro 与 SuperGrok 可直接登录（OAuth 设备码，不起本地回调端口，令牌 0600 存本机），省掉买 API 额度 |
| — | **模型现拉现选**：选完服务商就向对方要一份当前可用模型列表，从下拉里挑，不用手打型号名；拉不到会如实说明并允许手填 |
| — | **消息按 Markdown 渲染**：标题/列表/引用/围栏代码/链接；纯 DOM 构造，链接只放行 http/https 且交给系统浏览器打开 |
| — | **随时打断**：bot 跑飞了可以直接中止当前这轮，不用等它跑完 |
| — | **重启不丢**：会话上下文与消息历史落盘（`data/sessions/`、`data/events.json`），关掉引擎再开还在 |
| — | **多模型**：Anthropic 原生 + OpenAI 兼容端点（OpenAI / xAI Grok / DeepSeek / Kimi / 本地 Ollama / 自定义…），服务商目录由引擎给出 |
| — | **API Key 管理**：新建 Bot 时就地粘贴即可（也可在设置里统一管理）。存 `data/keys.json`，0600，界面只显示掩码；优先于环境变量 |
| — | **群成员管理**：在群聊设置里拉人/移出；群外 bot 收不到该群消息、@ 不到、不可被指派，私聊不受影响 |
| — | **任务看板 + 分工协议**：bot 拆解任务时用 `assign_task` 指派唯一负责人，`update_task` 当众认领/交付；看板在侧栏可见，从机制上避免重复干活 |
| — | **团队共享记忆**：`remember scope=team` 写入全队可见的 `data/TEAM_MEMORY.md`（跨模型平台共享） |
| — | **中英双语界面**：默认跟随系统语言，设置里可切换「跟随系统 / 中文 / English」；切换会同时作用于界面、后端消息与 bot 的提示词语言 |
| — | **插件（MCP）**：插件面板接入本地插件（stdio 命令，如 `npx @modelcontextprotocol/server-*`）或远程连接器（Streamable HTTP + Bearer）；引擎级实现，**任何 provider 的 bot 都能用**；每个 bot 按需勾选订阅；非只读插件操作一律先审批（`readOnlyHint` 注解的直接执行）；配置存 `mcp.yaml` |

## 快速开始

依赖：Node.js ≥ 20（跑 Electron）。macOS Apple Silicon 已附带编译好的后端二进制；其他平台需装 Go ≥ 1.22 后 `npm run build:backend` 重编译。

```bash
cd bot-bureau/app
npm install
npm start
```

**第一次打开是空的**——没有 bot，也没有群聊。引导会带你走三步：先说清楚你是这家事务所的老板、bot 是你雇来的同事，再指一遍创建和管理的入口，最后请第一位同事（选服务商 → 登录订阅或填 key → 从拉取到的列表里选模型）。跳过也行，侧栏的 ＋ 随时能补。

试试：私聊它写个脚本（运行会触发审批）；建个群聊把几位 bot 放一起，直接叫名字派活（`scout 查一下今天的 AI 新闻` —— 不用打 @）；说"每隔 60 分钟看下 HN 头条"。

## 数据存在哪里

**开发态**（在仓库里 `npm start`）：都在仓库根目录，`bots.yaml`、`mcp.yaml`、`data/`、`connect.json` 就在眼前，方便直接改。这四样都在 .gitignore 里——里面是你的密钥、你的 bot、你的聊天记录。起步可以拷模板，也可以直接在界面里建一个 bot，让 app 自己写：

```bash
cp bots.example.yaml bots.yaml && cp mcp.example.yaml mcp.yaml
```

**装了 app 之后**：全部落在系统的用户数据目录，跟着账号走，卸载 app 也不会带走：

| 平台 | 位置 |
|---|---|
| macOS | `~/Library/Application Support/Bot Bureau/` |
| Windows | `%APPDATA%\Bot Bureau\` |
| Linux | `~/.config/Bot Bureau/` |

里面是什么：

```
bots.yaml              团队定义（你在界面里建 bot 时由 app 写入）
mcp.yaml               插件/连接器定义
connect.json           记住的远程引擎地址与证书指纹
data/
  keys.json            API key（0600，仅当前用户可读）
  xai_oauth.json       订阅令牌（0600）
  chatgpt_oauth.json   订阅令牌（0600）
  token                局域网配对码（0600）
  events.json          消息历史（重启不丢）
  sessions/            每个 bot 的对话上下文
  workspaces/<bot>/    每个 bot 自己的工作目录 + MEMORY.md
  TEAM_MEMORY.md       全队共享记忆
  tasks.json           任务看板
  routines.json        定时例程
  groups.json          群聊与成员
  settings.json        语言、权限档位、群聊元信息
```

**只下载客户端、没有源码，完全没问题**——后端二进制打在包里，首次启动时创建一份空配置：没有 bot，没有群聊，什么都不预置。（只读资源和可变数据以前共用一条路径，所以只装了 app 的人一启动就会往只读的 `app.asar` 里写、然后失败。现在两者分开了，打包机自己的配置也不会跟着包一起发出去。）


### 换个位置放

设 `BOTBUREAU_DATA_DIR` 可以把上面这一整套挪到别处，客户端和直接跑的后端二进制都认（后端显式给了 `-data` 时以 `-data` 为准）：

```bash
# 一台机器上并存两套互不相干的配置：bot、key、记忆各自独立
BOTBUREAU_DATA_DIR=~/BotBureau/work     npm start
BOTBUREAU_DATA_DIR=~/BotBureau/personal npm start

# 开发时别把仓库弄脏（默认落点就是仓库根目录，跑一次就会生成 data/、connect.json 并改写 bots.yaml）
BOTBUREAU_DATA_DIR=/tmp/bb-dev npm start
```

支持 `~` 展开和相对路径（相对当前工作目录），目录不存在会自动建。启动时终端会打印实际落点：

```
[bot-bureau] data dir: /Users/you/BotBureau/work
```

**换目录 = 换一整套身份**：配对码在 `data/token` 里，所以换了目录之后，其它设备要重新配一次；`bots.yaml` 也是新的一份，会重新走首次引导。

## 界面语言

默认**跟随系统语言**（客户端读 Electron locale，引擎读 `LANG`/`LC_ALL`）；设置里可显式切换「跟随系统 / 中文 / English」。一次切换同时作用于三层：

1. **界面**：所有静态文案与动态提示（客户端本地记住选择）；
2. **引擎消息**：审批提示、错误、例程触发、额度告警等事件文本（持久化在 `data/settings.json`，重启保留）；
3. **bot 语言**：系统提示词整体切换语言，并要求 bot「用用户的语言回复」——所以中文界面下 bot 说中文，英文界面下说英文。

裸后端单独运行时，语言取 `data/settings.json`，没有则跟随 `LANG`（例如 `LANG=en_US.UTF-8 ./botbureau-backend …`）。注：`bots.yaml` 里的 `role`/`description` 是你自己写的数据，不会被翻译——想让默认团队在两种语言下都自然，可加可选的 `role_en` / `description_en` 字段（见下）。

## 权限档位

bot 干活时哪些动作要先问你，分四档。**边界都是引擎真能判定的东西**，不是文案上的松紧：

| 档位 | 免审批 | 仍要问 |
|---|---|---|
| **每次都问**（默认） | 只读命令、读文件、只读插件 | 写文件、执行命令、写插件 |
| **可改文件** | ＋ 工作目录内的写文件 | 命令、插件 |
| **自动干活** | ＋ 工作目录内的普通命令 | 越界操作、插件 |
| **完全放开** | 全部 | 无 |

两条贯穿所有档位的线：

- **越出工作目录的动作永远要问**，只有「完全放开」例外——否则「自动干活」就等于「完全放开」；
- **插件（MCP）的非只读调用同样只有「完全放开」才免审批**。引擎判断不了插件的作用域：fs 插件写文件和 GitHub 插件建 issue 在协议层长得一模一样，前者只动本机沙箱，后者动的是你的线上仓库。

关于「工作目录内」怎么判的，说清楚免得误解：文件读写被路径解析硬钉在工作目录里，出不去；bash 是**启发式**判断——绝对路径、`..`、`~` 会被拦，命令替换（`` ` `` 和 `$(`）因为让目标在执行前不可知，一律按越界处理。管道和重定向本身不算越界（命令的工作目录就钉在那儿，相对路径出不去），否则「自动干活」连 `echo x > note.txt && cat note.txt` 都跑不了，这一档就没意义了。

设置 → 权限是**全队默认**；每个 bot 可以在自己的设置里另设一档，默认「跟随全局」。改档位即时生效，不用重启。配置写错（非法值、空值）一律回落到最保守的「每次都问」，绝不会意外落到「完全放开」。

## 连接模型

界面上分三步，不用记 base_url，也不用手打型号名：

1. **选服务商** —— Anthropic Claude / OpenAI · ChatGPT / xAI Grok / DeepSeek / Kimi / Ollama / 自定义 / Fake（离线试用）。这张目录由引擎给出（`GET /api/providers`），客户端不自带。
2. **选接入方式** —— OpenAI 和 xAI 支持两种：
   - **订阅登录**：点「登录」，浏览器里输入弹窗显示的配对码即可。走 OAuth 设备码（xAI 另加 PKCE），**不在本机开回调端口**；令牌 0600 存 `data/`，过期自动刷新。用 ChatGPT Plus/Pro 或 SuperGrok 的额度，不必另买 API credit。
   - **API Key**：就地粘贴，自动存进钥匙串，不用先绕去设置页。
   
   选定哪种就用哪种：以前是"没存 key 才回退到订阅"，于是存过一个无关的 `OPENAI_API_KEY` 会把刚登录的订阅悄悄顶掉。
3. **选模型** —— 引擎拿着凭据去问服务商当前有哪些模型（`POST /api/models`），你从下拉里挑。**拉不到就如实报错并允许手填**，不会塞一份编出来的型号表——那正是选到已下线模型、发消息才炸的来源。

首次启动会用同一套面板做一次引导：选一次，套给所有还没配模型的同事；之后每位同事都能单独改。

## 配置多模型（bots.yaml）

界面上做的事最终都落到这个文件，也可以直接手写：

```yaml
bots:
  - name: chief          # 默认 anthropic + claude-opus-5
    role: 主管
    description: 拆解任务、派活、汇总。
    role_en: Lead        # 可选：界面/提示词切到英文时用这套人设
    description_en: Breaks work down, assigns it, and reports back.

  - name: gpt            # 用 ChatGPT Plus/Pro 订阅，不需要 api key
    role: 顾问
    description: 用 GPT 的视角提供第二意见。
    provider: openai
    provider_id: openai  # 界面回填用
    auth: chatgpt        # key（默认）/ chatgpt / xai / none
    model: gpt-5.1-codex

  - name: grok           # xAI Grok，这里用 API key
    role: 研究员
    description: 查资料、比对信息源。
    provider: openai
    provider_id: xai
    auth: key
    model: grok-4
    base_url: https://api.x.ai/v1
    api_key_env: XAI_API_KEY

  - name: local          # 本地 Ollama，无需 key
    role: 本地助手
    description: 跑在本机的小模型。
    provider: openai
    model: qwen3:14b
    base_url: http://127.0.0.1:11434/v1

  - name: demo           # 离线回声，无需任何 key（试用界面）
    role: 演示
    description: 回声机器人。
    provider: fake
```

`provider: anthropic`（默认）独享服务端联网搜索与 refusal fallback；OpenAI 兼容 bot 想上网可用 bash 的 `curl`（走审批）。
`auth` 留空的老配置仍按旧规则（照 `base_url` 猜）工作。

## 目录结构

```
backend/                 Go 引擎（HTTP + SSE）
  main.go                只做装配
  internal/
    i18n/                T() + locales/zh.json（叶子包）
    config/              bots.yaml、设置、权限档位
    secret/              钥匙串 + xAI / ChatGPT 的 OAuth
    model/               provider 实现 + 服务商目录 + 模型列表
    plugin/              MCP 客户端（stdio / Streamable HTTP）
    engine/              总线、群聊、bot worker、工具、看板、记忆、例程
    bridge/              Telegram
    netx/                mDNS、TLS、引擎锁
    httpx/ textutil/     跨包共用的小工具
app/                     Electron 客户端
  main.js                拉起后端子进程、窗口、多设备发现
  renderer/              纯 DOM，无框架
    locales/zh.js        中文译文表（主进程也读这一份）
  scripts/               开发态身份修正（Dock 名字与图标）
assets/make_icon.py      标记与图标生成（SVG + 深/浅两套 + macOS 超椭圆裁形）
bots.example.yaml        团队定义模板（拷成 bots.yaml 用；后者不进版本库）
```

`engine` 里那几个文件是真耦合——总线持有 worker，worker 反过来用总线——拆开只会逼出一堆没意义的接口，所以它们留在一个包里是设计不是偷懒。

## 代码约定

**源码只写英文。** 所有用户可见文案在代码里都是英文原文，其它语言放在翻译文件里按原文查表：

- Go：`i18n.T("Message is empty")` → `backend/internal/i18n/locales/zh.json`
- 渲染层：`t("Search")`、HTML 的 `data-i18n="Search"` → `app/renderer/locales/zh.js`（主进程 `main.js` 读同一份，两边共用一张表）
- 带值的用 `%s` 占位：`t("%s members", n)`。**不能**先拼好字符串再查表——插值后的串每次都不一样，永远查不中。

查不到就原样返回英文，**漏翻译不会变成空白界面**。加一门语言 = 再加一个同结构的文件。

**注释中英双语。** 这条和上一条不冲突：注释是给维护者看的，双语能让两边的人都读懂"为什么这么写"；文案是给用户看的，必须能整体切换。

## 外观

界面跟随系统深/浅色，设置 → 通用里可以钉死「跟随系统 / 浅色 / 深色」。选择存在本机而不是引擎——同一个引擎可能被好几台设备连着，屏幕亮度是每台设备自己的事。

标记是一对叠放的 B——前面那个的下字碗里嵌了一张脸，后面那个只是它的影子，不给脸。矢量在 `assets/logo.svg`，字形全部由圆角矩形与圆拼出：同一份几何既拼成 SVG 的圆弧路径，也压平成多边形交给位图，两边不会走样。前后两个 B 之间那道留白也是算出来的——把前一个 B 的每个部件各自外扩再并起来，等于把并集整体外扩。

应用图标出了深浅两套（`assets/icon-dark.png` / `icon-light.png`）：浅色白底，深色黑底，标记在黑底上提亮到同色相的浅棕，否则那个棕压在黑上几乎看不见。图标不是画成一张平图，而是按 iOS 26 液态玻璃的分层来叠：底色一层、标记一层、高光一层——顶边提亮、底边压暗、下方投影，全部从标记自己的遮罩推出来，所以永远贴着字形走。macOS 版按苹果的图标网格做了超椭圆裁形和留白——系统不会替你切圆角，直接给一张方图放进 Dock 就是个突兀的方块。打包用的是深色版：Dock 和任务栏的材质偏深，深色版在两者上都稳。

### 图标跟着系统外观切换

分两层，各补各的缺口：

**应用跑起来的时候**，主进程监听 `nativeTheme`，系统切深浅就换 Dock 图标（Windows / Linux 换的是窗口图标，任务栏跟着窗口走）。跟的是**系统**外观，不是设置里那档「跟随系统 / 浅色 / 深色」——图标待在 Dock 里和别人的图标挨着，用的是系统的底，不是这个应用自己的界面。所以两张图标都得随包分发，`build/icon.png` 和 `build/icon-light.png`。

**应用没跑的时候**，Finder 和 Dock 显示的是包里那张死图，上面那套够不着。要让它也随外观切换，得用 macOS 26 的 `.icon` 格式——但 electron-builder 25 不认这个格式，它只认 `icon` 字段那张 `.icns`。所以补了个 afterPack 钩子（`app/scripts/mac-liquid-icon.js`），在打包之后、签名之前自己把资源目录塞进去：

1. 打开 Icon Composer（在 Xcode 里，`Xcode.app/Contents/Applications/`），把 `assets/icon-layers/` 那几张扁平层拖进去——底色一张、标记一张，深浅各一套
2. 存成 `assets/AppIcon.icon`
3. `npm run dist:mac` 照常打包，钩子自己会认出它

钩子做的事：`actool` 把 `.icon` 编成 `Assets.car`，拷进 `Contents/Resources/`，再往 Info.plist 里加一个 `CFBundleIconName`。**`CFBundleIconFile` 不动**——那是 electron-builder 放的 `.icns`，留给 macOS 26 以下的系统兜底。两张图标并存不冲突，各管各的系统，不用为此分两个包。

时序是关键：`afterPack` 跑在签名**之前**，改动会被随后的签名一起封进去；同样的事放到 `afterSign` 里做，就是亲手破坏刚签好的签名。

electron-builder 只认一个 `afterPack`，所以配置里挂的是 `app/scripts/after-pack.js`，它按顺序串起两步：先补图标资源目录（`mac-liquid-icon.js`），最后清扩展属性（`strip-xattr.js`）。清理必须排在最后——否则后一步往包里写的文件带着属性进去，codesign 照样拒签。

没有 `assets/AppIcon.icon`、或者机器上没装完整 Xcode（`actool` 在 Xcode 里，不在命令行工具里），这一步整段跳过，打包照常出包，只是没有外观切换——锦上添花的东西不该让打包失败。

> 换过图标之后如果 Dock 里还是旧的，是系统图标缓存没刷新，`killall Dock` 一下。

## 打包与各端计划

### 桌面端（现在就能打）

```bash
cd app
npm install           # 首次需要，装 electron-builder
npm run dist:mac      # 一个通用 dmg（Intel + Apple Silicon 通吃）
npm run dist:win      # nsis 安装包，x64
npm run dist:linux    # AppImage + deb，x64
```

三端共用同一份 Electron 客户端，Go 引擎作为子进程随 app 启动。产物结构：

```
Bot Bureau.app/Contents/
  MacOS/Bot Bureau                   Electron 主程序
  Resources/app.asar                 main.js + preload.js + renderer/（只读）
  Resources/app.asar.unpacked/bin/   botbureau-backend（引擎，必须在 asar 外）
  Resources/bots.yaml                种子配置，首次启动拷进用户数据目录
```

两个坑写在这儿免得重踩：

- **引擎二进制必须解包**（`build.asarUnpack`）。asar 里的文件读得到但 **exec 不了**，留在里面的话打包版一启动就找不到引擎。
- **架构不能靠一句 `go build`**。裸的 go build 只出当前机器的架构，而 dmg 要跑在两种 Mac 上；`scripts/build-backend.js` 会分别编 arm64 和 amd64 再用 `lipo` 合成通用二进制，并校验两个切片都在。Windows / Linux 走交叉编译（引擎是纯 Go，`CGO_ENABLED=0`）；换架构就改 `--arch` 再跑一次。

> **签名与公证**：钥匙串里有 `Developer ID Application` 证书时，electron-builder 会自动拿它签名（带 hardened runtime 和时间戳）——所以打包会慢，几百个文件每个都要联网打时间戳。
>
> 但**签名不等于能直接打开**。macOS 10.15 起，从网上下载的 app 还必须经过 Apple **公证**（notarization）：把包传给 Apple 扫描，再把回执 staple 进去。没公证的话用户下载后仍会被拦，得右键打开一次，或 `xattr -d com.apple.quarantine "Bot Bureau.app"`。
>
> 要加公证：给 electron-builder 配 `mac.notarize` 并提供 Apple ID / app-specific password / team id（或 API key）。本地拷贝或局域网分发用不着——隔离标记只有从浏览器下载才会带上。

> **开发态的 Dock 名字**：`npm start` 跑的是 `node_modules` 里的 Electron，Dock 显示的是**那个** app 的名字。`scripts/fix-electron-identity.js` 会在 prestart 时改它的 `Info.plist` 并重新 ad-hoc 签名（改完不签，macOS 会因签名失效拒绝启动或反复弹权限）。即便如此，macOS 的 Launch Services 会缓存应用名，第一次改完可能要重登录或 `killall Dock` 才刷新。**打包产物没有这个问题**——那是它自己的 bundle。

### 移动端与手表端（计划）

引擎已经是标准的 HTTP + SSE 服务，认证是配对码，跨网络走 Tailscale 这类虚拟组网就能连——所以其它端本质上是**再写一个客户端**，不用动引擎。

| 端 | 形态 | 状态 |
|---|---|---|
| macOS / Windows / Linux | Electron，内置引擎 | ✅ 已实现 |
| Telegram | 桥接，任何装了 Telegram 的设备都能用 | ✅ 已实现（见下文） |
| iOS / Android | 原生客户端，连家里那台跑引擎的机器 | 计划中 |
| watchOS | 看聊天进度、收审批推送、抬腕批准/拒绝 | 计划中 |

手机端和手表端**不跑引擎**——模型调用、bash、插件都需要一台常开的机器，手机只当客户端。watchOS 那端尤其克制：只做「现在有 bot 在等你点头」这一件事，列表 + 批准/拒绝两个按钮，加上一眼能看完的任务进度。

想现在就在手机上用：开 Telegram 桥（见下文），聊天和审批都能在手机上完成，不必等原生客户端。

## 跨平台协作与分工的原理

**不同平台的 bot 可以无障碍协作、共享记忆。** 协作发生在应用层（Go 消息总线传纯文本），与底层模型无关：Claude bot 转交任务给 Grok bot、共同读写任务看板和 `TEAM_MEMORY.md`，都只是文本进出各自的上下文；每个 bot 的对话历史以各自 provider 的原生格式独立保存，互不接触。API key 只决定"这个 bot 用谁的脑子"，不影响协作面。

**避免重复干活的四层机制**：① 路由层——群聊里没被 @点名/指派的 bot 根本不会触发行动（只旁听入上下文）；② 看板层——`assign_task` 每个子任务恰好一个负责人，`update_task` 置 doing 等于当众认领；③ 协议层——系统提示词写死分工规则（宽泛任务先查看板再拆解指派、别人的活不碰）；④ 可见层——看板注入每个 bot 的群聊提示词并显示在 UI 侧栏，人和 bot 都能一眼看到谁在干什么。

## 多设备同步（无服务器）


配对码只走 `Authorization` 头。消息流（SSE）例外——浏览器的 `EventSource` API 加不了请求头（换 WebSocket 也一样），凭据只能进 URL，而 URL 会被反向代理原样写进 access log。所以那条连接改用**短时效票据**：客户端先用带头的 `POST /api/sse-ticket` 换一张十分钟过期的票据，票据只对 `/api/events` 有效。配对码本身永远不出现在任何 URL 里。


> **本机模式也要配对码。** "只绑 127.0.0.1" 不是安全边界——本机上每个网页都能访问 localhost。
> 早先 `-listen local` 完全免认证，任何你打开的站点都能读走整个团队和聊天记录，还能建 bot、
> 把全局权限改成「完全放开」、给 bot 发消息；串起来就是从一个网页在你机器上执行任意命令。
> 现在两种模式都要配对码，它存在 `data/token`（0600），网页读不到、客户端读得到。

同一局域网内，多台设备之间**点对点直连**，不经过任何云服务器：

- **一台设备跑引擎**（bot、聊天、记忆、看板全在它上面），其余设备启动 Bot Bureau 时会通过 **mDNS 自动发现**它，弹窗询问"连接它，还是本机独立运行"。选连接后本机就是纯客户端——聊天、审批、管理插件/成员全都能做，且天然强一致（只有一份状态，无同步冲突）。
- **配对码**：引擎设备的 设置里可以看到；其他设备首次连接输入一次即记住。除发现探针外的所有接口都要求配对码（常数时间比对；`data/token`，0600）。
- **换设备跑引擎**：把 `bots.yaml`、`mcp.yaml`、`data/` 放进 iCloud/Syncthing 等同步文件夹即可冷迁移。**引擎锁**（`data/engine.lock` + 心跳）会阻止两台设备同时跑引擎——第二台会被明确拒绝，避免 bot 重复应答；崩溃残留的陈旧锁 30 秒后自动失效。
- 调试/手动指定：`BOTBUREAU_BACKEND_URL=http://<ip>:<port> npm start` 直接以客户端模式连接；`BOTBUREAU_LOCAL_ONLY=1` 则只监听 127.0.0.1、不广播。
- macOS 首次运行可能弹"允许访问本地网络/接受传入连接"，允许即可。

发现是**非阻塞**的：打开应用不会被任何弹窗拦住，本机引擎照常启动；窗口出来之后才扫一遍局域网，发现别的设备就在设置按钮上方冒一条提示，点「配对」才会问配对码，点「不用了」就记住这台、不再提第二次。

### 跨网络（不在同一局域网）

NAT 决定了跨互联网直连必须有人帮忙穿透——纯"无任何第三方"的打洞不存在。推荐做法是**把不同网络变成同一张私有网络**：

1. **Tailscale（推荐）**：两台设备都装 [Tailscale](https://tailscale.com)（个人免费）并登录同一账号；在跑引擎的设备上查到它的 Tailscale IP（`100.64.0.0/10` 段）；另一台设备打开 Bot Bureau → 设置 → "连接远程引擎"填 `http://100.x.y.z:8973` → 输一次配对码即可。地址会记住（`connect.json`），下次启动自动直连；连不上会提示并可退回本机运行。流量走 WireGuard 端到端加密、数据面点对点。引擎端口固定为 8973（被占用才退避随机口）。
2. **SSH 隧道**：有一台能 SSH 到引擎设备的机器时：`ssh -N -L 8973:127.0.0.1:8973 user@引擎机`，然后连 `http://127.0.0.1:8973`。
3. **同步盘冷迁移**：`bots.yaml + mcp.yaml + data/` 放 Syncthing/iCloud（Syncthing 本身就是跨网络 P2P 同步），到哪台设备就在哪台跑引擎，引擎锁保证同时只有一台在跑。

⚠️ 不要把 8973 端口直接暴露到公网（明文 HTTP + 配对码不适合公网环境），务必经由上述加密通道。

### 有服务器？公网直连

引擎放你自己的 VPS/服务器上，客户端从任何网络直连（先在服务器上交叉编译或 `go build`）：

```bash
# 方式一：内置 TLS（无需域名）——自签名证书 + 客户端指纹钉扎（TOFU）
./botbureau-backend -port 8973 -tls auto -config bots.yaml -mcp mcp.yaml -data data
# 启动日志会打印证书 SHA-256 指纹；客户端首次连接 https://<服务器IP>:8973 时自动记住指纹，
# 此后指纹变化（可能的中间人）会被直接拒绝并警告

# 方式二：有域名 → 反向代理拿正规证书（Caddy 一行自动 HTTPS）
caddy reverse-proxy --from botbureau.example.com --to localhost:8973
# 客户端直接连 https://botbureau.example.com

# 也支持自备证书：-tls cert.pem:key.pem
```

客户端 设置 →「连接远程引擎」填 `https://...` + 配对码即可。安全模型：TLS 加密信道 + 配对码认证 + 指纹钉扎防中间人；明文 http 仅限内网/虚拟组网。

### 接入 Telegram（把团队装进手机聊天软件）

1. Telegram 里找 **@BotFather** → `/newbot` 创建，拿到 token；
2. Bot Bureau 设置 → API Key 区存为 `TELEGRAM_BOT_TOKEN` → Telegram 桥点「开启」；
3. 在 Telegram 里给你的 bot 发 `/start`——**首个发送者独占绑定**，他人使用会被拒。

之后在手机上：

- **自选接入目标**：`/bind group`（默认）把会话接到团队群聊，`/bind scout` 改接到某个 bot 的私聊——之后普通消息直达 ta，转发也只推该会话的内容（互不刷屏）；随时 `/bind` 查看当前绑定；
- 群聊模式下 `@scout …` 点名照常；`/dm coder 内容` = 不改绑定、临时和某个 bot 说一句（其后续回复也会推送）；`/bots` 看名单；
- **命令审批直接弹 ✅批准/❌拒绝 按钮**，点一下即可；
- **额度告警必达**：任何一家模型的余额/配额耗尽，无论当前绑定什么都会推送提醒。

桥用官方 Bot API 长轮询实现——引擎在家里内网也能用，无需公网 IP。

微信没有官方个人 bot API（第三方逆向方案违反条款、极易封号），故不提供；Discord/Slack/飞书可按 `backend/telegram.go` 的模式扩展（消息进 `bus.PostGroup`/`Deliver`，事件出 `bus.EventsSinceCtx`）。

### 引擎机关机怎么办（可用性）

单引擎架构下，引擎关机 = 该团队离线（客户端只是视图）。三层应对：

1. **失联提示与快速切换**：客户端断线 3 次后顶部亮"引擎失联"横幅，自动持续重试；可一键「切到本机引擎」（远程模式，转为本机独立团队）或「重启本机引擎」（本机引擎崩溃时）。引擎回来后自动恢复，无需操作。
2. **引擎常驻（推荐）**：引擎不需要 Electron，把裸后端放在常开设备（Mac mini / NAS / 树莓派）上跑即可，客户端照常发现/直连：
   ```bash
   ./app/bin/botbureau-backend -port 8973 -config bots.yaml -mcp mcp.yaml -data data
   ```
   配合 `nohup` / `launchd` / `systemd` 开机自启；跨平台部署时在目标机器上 `cd backend && go build` 重编译即可。
3. **换机接管**：数据目录走同步盘（见上文），引擎锁 30 秒过期后在任一台设备"本机运行"即接管全部持久状态（成员/插件/记忆/看板/例程/密钥）。对话上下文本就只存引擎内存，引擎重启即清空——长期信息靠 remember 记忆机制，这是刻意取舍。

## 插件（MCP）示例

```yaml
# mcp.yaml —— 也可全部在 插件面板里操作
servers:
  - name: fs                # 本地插件：官方文件系统 server
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Documents"]
  - name: linear            # 远程连接器
    url: https://mcp.linear.app/mcp
    bearer_key: LINEAR_TOKEN   # 从密钥仓库 / 环境变量取
```

工具以 `mcp_插件名_工具名` 暴露给勾选了该插件的 bot（bots.yaml 里对应 `mcp: [fs, linear]`）。
选择引擎级 MCP（而非 Anthropic API 自带的服务端 MCP 连接器）是为了让 Grok / DeepSeek / Ollama 等所有 provider 的 bot 共用同一批插件；stdio 本地插件也只有引擎级才能支持。

远程连接器有两种接入方式：**静态令牌**（`bearer_key`，指向密钥仓库里的一条）和 **OAuth**（`auth: oauth`，
在插件面板点「授权」触发）。OAuth 走的是完整的一套：发现受保护资源元数据 → 发现授权服务器 →
动态客户端注册（RFC 7591）→ 授权码 + PKCE → 自动刷新。Linear、Notion、Sentry 这些不发静态令牌的
连接器只能走这条路。

插件工具多的时候（GitHub 官方 MCP 有九十多个）在面板里点「选择工具」勾一个子集，
对应 `mcp.yaml` 里的 `tools:` 列表；留空表示全部，这样插件以后更新新增的工具会自动跟上。

**本地插件拿不到你的完整环境**：插件进程只收到一份白名单（PATH/HOME/语言、代理与证书变量等）
加上它自己在 `env:` 里声明的那些。因为面板上点一下就能装的东西，不该顺手拿到 `SSH_AUTH_SOCK`
（等于能用你的 SSH 私钥）或你 shell 里的各种令牌。**如果某个插件需要别的环境变量，在 `env:` 里
写出来**——值以 `$` 开头就从密钥仓库取。

**本地插件掉线会自己回来**：进程崩了、机器睡醒后管道断了，引擎会把状态改成不可用并按退避重连
（几次不成就停下等你手动点）。以前那颗点会一直是绿的，而模型对着一份已经不存在的工具列表接着调。
服务端说工具列表变了（`tools/list_changed`）时也会自动刷新，不用手动重连。

## 技能（Agent Skills）

技能是把某类工作的**做法**写下来，和插件给的「能做什么」是两回事。一个技能就是一个目录：

```
data/skills/release-notes/
  SKILL.md          # YAML frontmatter: name + description，正文是给模型看的说明
  build.py          # 可选，随手放的脚本和资料
```

```markdown
---
name: release-notes
description: 把合并了的 PR 整理成符合团队风格的发版说明。什么时候该用我，这句话要写清楚。
---

1. 按用户可感知的影响分组，不要按模块分。
2. 每条以动词开头。
```

**两段式加载**是这里唯一重要的设计：系统提示里只放每个技能的一行 `name: description`，
模型判断用得上时才调 `read_skill` 拉全文。所以装几十个技能只多几十行提示词，而不是几十篇文档。
`description` 是模型唯一的判断依据——它必须写清楚"什么时候该用我"，这是自己写技能时最容易漏的一点。

技能全队共享，不按 bot 订阅（一行摘要的成本可以忽略，而"谁该用哪个"本来就由描述匹配决定）。
技能自带的脚本用 bash 按完整路径执行；那里在工作目录之外，所以会先经审批——第三方代码就该这样对待。

## 插件包（Claude / Codex 格式）

**Bot Bureau 不自造插件格式，直接装 `.claude-plugin/plugin.json` 那一套。** 理由很实在：
自造格式意味着从零招开发者；沿用既有格式意味着开发者写一次，Claude Code、Codex、Bot Bureau 都装得上。
所以对开发者来说，"兼容 Bot Bureau" 的成本是 0——照常写你的 Claude 插件即可。

在插件面板里填 git 地址或本机文件夹路径安装，或直接把目录拷进 `data/plugins/`（文件系统是唯一事实，
不额外维护索引）。

**市场仓库也认**。生态里插件的分发单位常常不是「一个仓库一个插件」，而是「一个仓库一个市场，
里面列着若干插件」——那种仓库根目录放的是 `.claude-plugin/marketplace.json`。抽样二十个真实仓库，
**约一半是这种形态**，所以贴市场地址进来会列出里面的插件让你挑一个装，而不是报「找不到 plugin.json」。
清单里 `source` 指向仓库根、那个目录又没有自己的 `plugin.json` 时，用清单条目里的名字和描述兜底
（实测这种写法确实存在）。

装好的插件点「升级」原地更新：git 源走 `git pull`，市场装的照着清单那条重新取。**升级是调和不是重建**
——新增的 MCP server 加进来、消失的删掉，已存在的连同你挑好的工具子集和跑过的授权一起原样保留。

### 支持矩阵——哪些字段生效

| 插件包里的东西 | Bot Bureau 怎么处理 |
|---|---|
| `mcpServers`（清单里的）或根目录 `.mcp.json` | 注册成 MCP 插件，名字加包名前缀（`acme` 包里的 `notes` → `acme_notes`）。`${CLAUDE_PLUGIN_ROOT}` 会展开成安装后的真实路径 |
| `skills/` | 并入技能库，来源标为包名 |
| `agents/*.md` | **变成团队成员模板**。frontmatter 的 name/description 填进新建 bot 的表单，正文进「详细角色说明」（附在系统提示末尾） |
| `commands/` | 不支持——Bot Bureau 没有斜杠命令，直接在聊天里跟 bot 说 |
| `hooks/` | 不支持——没有钩子系统；安全那一面由四档权限覆盖 |

不支持的部分会在装完后**明确列出来**，不会装完悄悄少一半功能。

`agents/` 那一行是 Bot Bureau 独有的：同一个插件包在别处只能把 agent 降级成子代理，
在这里它就是一位真正的同事——有自己的工作目录、记忆、任务看板身份，能被 @、能被派活。

## 额度用尽会提醒吗？

会，双通道：模型平台返回「余额/配额耗尽」类错误（区别于普通限流 429）时——

1. 出错的会话里立即出现 ⚠️ 报错气泡（含平台原始信息）；
2. 同时发一条**全局 💳 告警**：所有聊天面板可见、Telegram 桥无论绑定什么都会推送；同一 provider 十分钟内只告警一次，防止例程/重试刷屏。

识别覆盖 Anthropic（credit balance / billing）与 OpenAI 兼容系（insufficient_quota / exceeded your current quota / HTTP 402）。注意：额度耗尽后 bot 不会自动暂停例程，请充值后正常继续，或删掉相关例程。

## 实现说明

- Claude 走官方 `anthropic-sdk-go`（beta 端点）：`claude-opus-5`、自适应思考、服务端 `fallbacks:"default"`（安全分类器拒答时自动换推荐模型接续）、refusal 整回合回滚、`max_tokens` 截断时自动补配对 tool_result。
- OpenAI 兼容层为原生 `net/http` 实现，历史/工具双向转换，`base_url` 指到哪家就是哪家。
- 只读 bash（ls/cat/grep 等，且不含 `;|&$<>` 等元字符）直接执行，其余一律审批；`find` 因自带 `-delete/-exec` 不在只读名单。审批期间该 bot 阻塞等待——这正是"需要人工判断时回来找你"的落地方式。
- 会话历史仅存内存（重启清空），群聊/私聊上下文相互独立；长对话在完整回合边界截断。长期信息让 bot `remember`。
- bash 的"沙箱"只是工作目录 + 审批门，**不是**真隔离；批准前请看清命令。
- 打包分发（dmg/exe/AppImage）可加 electron-builder；当前以 `npm start` 开发态运行。

## 目前不做的（有意从简）

云端常驻虚拟机、代你登录真实网页应用（可用 Claude computer use 扩展）、演示一遍就学会工作流、移动端原生推送（现由 Telegram 桥代替）。
