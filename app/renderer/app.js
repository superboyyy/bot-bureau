"use strict";
// Bot Bureau 渲染器：/api/state 引导 + SSE 增量更新，纯 DOM 渲染（无框架）。
// Bot Bureau renderer: bootstrap via /api/state + incremental updates over SSE, pure DOM rendering (no framework).

const PARAMS = new URLSearchParams(location.search);
const BACKEND = PARAMS.get("backend") || "http://127.0.0.1:8973";
const REMOTE = PARAMS.get("remote") === "1";
// 主进程给侧栏开了 macOS 原生 vibrancy 时，侧栏底下的画布必须让开，否则实色底把系统模糊盖住。
// 这里只把开关钉在 <html> 上，怎么让开由 style.css 决定。在浏览器里直接打开时没有这个参数，
// 页面照旧铺实色底，两边都不用写分支。
// When the main process turned on macOS vibrancy for the sidebar, the canvas behind it has to step
// aside or its flat fill buries the system blur. This only stamps the switch on <html>; style.css
// decides what stepping aside means. Opened directly in a browser the param is absent and the page
// keeps its opaque canvas, so neither side needs a branch.
if (PARAMS.get("vibrancy") === "1") document.documentElement.setAttribute("data-vibrancy", "");
let TOKEN = PARAMS.get("token") || localStorage.getItem("botbureau_token:" + BACKEND) || "";

let state = { bots: [], approvals: [], routines: [], tasks: [], keys: [], group_members: [], groups: [], mcp: [], default_bot: "" };
const isGroupChatId = (id) => id === "group" || /^g_/.test(id);
const groupsList = () => state.groups || [];
const groupOf = (id) => groupsList().find((g) => g.id === id);
const groupTitleOf = (id) => (id === "group" ? settingsOf().group_title : (groupOf(id) || {}).title) || t("Group chat");
const groupFaceOf = (id) => (id === "group" ? settingsOf().group_avatar : (groupOf(id) || {}).avatar) || "";
const groupMembersOf = (id) => (id === "group" ? (state.group_members || []) : ((groupOf(id) || {}).members || []));
const inGroupOf = (chat, name) => (groupMembersOf(chat) || []).includes(name);
let events = [];
let lastId = 0;
let current = "group"; // "group" 或 "dm:<bot>" / "group" or "dm:<bot>"
const unread = {};
let filter = "";                       // 侧栏搜索词 / sidebar search term
const expanded = new Set();            // 已展开的折叠块 / folds the user has opened
let refetchTimer = null;

// 服务商与模型不再写死在这里：目录来自引擎 /api/providers，模型名现拉 /api/models。
// 写死一张型号表迟早过期，用户选到已下线的名字只会在真正发消息时才炸。
// Vendors and models are no longer hard-coded here: the catalog comes from the engine's /api/providers
// and model names are fetched live from /api/models. A baked-in table goes stale, and a retired model id
// only blows up when a message is actually sent.


// 插件目录：面板里点一下就装的一批。每条要么免配置，要么只差一样东西（need），
// 装之前当场问，问完直接连——不让用户先去别处准备好再回来。
// 顺序按"上手成本"排：免配置的在前，要密钥/要授权的在后。
//
// Plugin catalog: the ones the panel installs in a single click. Each entry either needs nothing or is
// short exactly one thing (need), which is asked for on the spot and then installed — rather than
// sending the user elsewhere to prepare something and come back.
// Ordered by setup cost: nothing-to-configure first, key/authorization ones last.
//
// need.kind:
//   null   —— 直接装 / install straight away
//   "path" —— 追加一个目录参数 / append a directory argument
//   "key"  —— 先存一个密钥，再按 env / bearer_key 引用 / save a key first, then reference it
const MCP_CATALOG = [
  {
    name: "deepwiki", label: "DeepWiki",
    desc: "Ask questions about any public GitHub repository — its docs, architecture and code.",
    url: "https://mcp.deepwiki.com/mcp",
  },
  {
    name: "context7", label: "Context7",
    desc: "Up-to-date documentation and code examples for thousands of libraries.",
    url: "https://mcp.context7.com/mcp",
  },
  {
    name: "memory", label: "Memory",
    desc: "A knowledge graph the team can write facts into and search later.",
    command: "npx", args: "-y @modelcontextprotocol/server-memory",
  },
  {
    name: "thinking", label: "Sequential Thinking",
    desc: "A scratchpad for working through a hard problem one step at a time.",
    command: "npx", args: "-y @modelcontextprotocol/server-sequential-thinking",
  },
  {
    name: "fs", label: "Filesystem",
    desc: "Read and write files under one directory you choose.",
    command: "npx", args: "-y @modelcontextprotocol/server-filesystem",
    need: { kind: "path", label: "Directory to expose", placeholder: "/Users/you/Documents" },
  },
  {
    name: "playwright", label: "Playwright",
    desc: "Drive a real browser: open pages, click, fill forms, read what is on screen.",
    command: "npx", args: "-y @playwright/mcp@latest",
    slow: true,
  },
  {
    name: "github", label: "GitHub",
    desc: "Issues, pull requests, code search and file contents on GitHub.",
    url: "https://api.githubcopilot.com/mcp/",
    need: {
      kind: "key", key: "GITHUB_PAT", as: "bearer", label: "GitHub personal access token",
      hint: "Create a token at github.com/settings/personal-access-tokens — it is stored on this machine only.",
    },
  },
  {
    name: "exa", label: "Exa Search",
    desc: "Web search built for AI, returning full page contents rather than just links.",
    command: "npx", args: "-y exa-mcp-server",
    need: { kind: "key", key: "EXA_API_KEY", as: "env", label: "Exa API key", hint: "Get one at exa.ai." },
  },
  {
    name: "firecrawl", label: "Firecrawl",
    desc: "Scrape and crawl entire websites into clean markdown.",
    command: "npx", args: "-y firecrawl-mcp",
    need: { kind: "key", key: "FIRECRAWL_API_KEY", as: "env", label: "Firecrawl API key", hint: "Get one at firecrawl.dev." },
  },
  // 下面两个服务用 OAuth，而引擎侧只支持静态 Bearer，所以走 mcp-remote 桥接：
  // 它在本机跑完授权流程（浏览器里点同意），再以 stdio 把工具交给引擎。
  // These two use OAuth while the engine only speaks static Bearer, so they go through the mcp-remote
  // bridge: it runs the authorization flow locally (you approve in a browser) and then hands the tools
  // to the engine over stdio.
  {
    name: "linear", label: "Linear",
    desc: "Issues, projects and cycles in Linear.",
    command: "npx", args: "-y mcp-remote https://mcp.linear.app/mcp",
    oauth: true, slow: true,
  },
  {
    name: "notion", label: "Notion",
    desc: "Search, read and update pages and databases in Notion.",
    command: "npx", args: "-y mcp-remote https://mcp.notion.com/mcp",
    oauth: true, slow: true,
  },
];

const $ = (id) => document.getElementById(id);
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

let toastTimer = null;
function toast(msg) {
  const n = $("toast");
  n.textContent = msg;
  n.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { n.hidden = true; }, 3600);
}

let askResolver = null;
function finishAsk(v) {
  const r = askResolver;
  askResolver = null;
  if (r) r(v);
}

function ask({ title, hint = "", ok, cancel, danger = false, field = false, fieldLabel = "", placeholder = "" }) {
  return new Promise((resolve) => {
    askResolver = resolve;
    $("askTitle").textContent = title;
    $("askHint").textContent = hint;
    $("askHint").hidden = !hint;
    $("askFieldWrap").hidden = !field;
    $("askField").value = "";
    $("askField").placeholder = placeholder;
    $("askFieldLabel").textContent = fieldLabel;
    $("askOk").textContent = ok || t("OK");
    $("askCancel").textContent = cancel || t("Cancel");
    $("askOk").classList.toggle("danger", !!danger);
    $("askModal").showModal();
    if (field) $("askField").focus();
  });
}

$("askForm").addEventListener("submit", (e) => {
  const fieldOn = !$("askFieldWrap").hidden;
  if (e.submitter && e.submitter.value === "cancel") {
    finishAsk(fieldOn ? null : false);
    return;
  }
  e.preventDefault();
  const v = fieldOn ? $("askField").value : true;
  finishAsk(v);
  $("askModal").close();
});
$("askModal").addEventListener("close", () => {
  finishAsk(!$("askFieldWrap").hidden ? null : false);
});

// ---- 头像 / Avatars ----
// 圆脸 + 两只眼睛：底色区分身份，剪影统一。近黑背景下这些浅色圆是唯一的亮点，
// 所以底色一律高明度低饱和，避免界面出现刺眼的彩色块。
// A round face with two eyes: the fill distinguishes identity while the silhouette stays uniform.
// On the near-black canvas these pale circles are the only bright elements, so every fill is
// high-luminance and low-saturation — no jarring blocks of color.
const FACES = ["#dcdce1", "#c9b9a6", "#a8bccf", "#b2c9a6", "#cfadbc", "#a3c9c1", "#bcb2d9"];
const NS_SVG = "http://www.w3.org/2000/svg";

function hashOf(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h;
}
const faceColor = (name) => FACES[hashOf(name) % FACES.length];

function faceSVG(fill, size) {
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("class", "face");
  svg.setAttribute("width", size);
  svg.setAttribute("height", size);
  svg.setAttribute("viewBox", "0 0 40 40");
  const disc = document.createElementNS(NS_SVG, "circle");
  disc.setAttribute("cx", "20"); disc.setAttribute("cy", "20"); disc.setAttribute("r", "20");
  disc.setAttribute("fill", fill);
  svg.append(disc);
  for (const cx of [14.4, 25.6]) {
    const eye = document.createElementNS(NS_SVG, "ellipse");
    eye.setAttribute("cx", cx); eye.setAttribute("cy", "18.2");
    eye.setAttribute("rx", "2.7"); eye.setAttribute("ry", "3.5");
    eye.setAttribute("fill", "#141416");
    svg.append(eye);
  }
  return svg;
}

// 上传的头像存成 data URI，直接当圆图贴上去 / An uploaded avatar is a data URI, used as the circle directly
function faceIMG(src, size) {
  const img = document.createElement("img");
  img.className = "face";
  img.src = src;
  img.width = size;
  img.height = size;
  img.alt = "";
  return img;
}

// 头像取值：留空按名字哈希出底色，#rrggbb 用作底色，data: 则是用户传的图
// Avatar resolution: empty hashes a fill from the name, #rrggbb is a fill, data: is a user-supplied image
function faceNode(spec, fallbackName, size) {
  if (spec && spec.startsWith("data:")) return faceIMG(spec, size);
  return faceSVG(spec || faceColor(fallbackName), size);
}

const botOf = (name) => state.bots.find((b) => b.name === name);
const cfgOf = (name) => (botOf(name) || {}).config || {};
// 界面上显示的名字：自定义显示名优先，没设就用 @提及 id
// The name shown in the UI: a custom display name wins, otherwise the @mention id
const titleOf = (name) => cfgOf(name).display_name || name;
const modelSet = (name) => !!(cfgOf(name).provider);
const settingsOf = () => state.settings || {};

// 消息流里的头像尺寸，和标题栏那颗保持一致 / Avatar size in the message stream, matching the header's
const MSG_AV = 26;

function avatar(name, size = 40, busy = false) {
  const wrap = el("span", "av" + (busy ? " busy" : ""));
  wrap.style.width = size + "px";
  wrap.style.height = size + "px";
  wrap.append(faceNode(cfgOf(name).avatar, name, size));
  return wrap;
}

// 群聊头像：没设就把前两位成员的脸叠起来，轮廓一眼区别于私聊
// Group avatar: unless set, the first two members' faces are stacked — instantly distinct from a DM
function groupAvatarFor(id, size = 40) {
  const spec = groupFaceOf(id);
  if (spec) {
    const wrap = el("span", "av");
    wrap.style.width = size + "px";
    wrap.style.height = size + "px";
    wrap.append(faceNode(spec, id, size));
    return wrap;
  }
  const wrap = el("span", "av stack");
  wrap.style.width = size + "px";
  wrap.style.height = size + "px";
  const members = (groupMembersOf(id) || []).slice(0, 2);
  while (members.length < 2) members.push("bot-bureau-" + members.length);
  const small = Math.round(size * 0.68);
  wrap.append(faceSVG(faceColor(members[0]), small), faceSVG(faceColor(members[1]), small));
  return wrap;
}

// ---- 时间 / Time ----

const pad2 = (n) => String(n).padStart(2, "0");
const WEEK = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

function dayLabel(d) {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const days = Math.round((today - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
  if (days === 0) return t("Today");
  if (days === 1) return t("Yesterday");
  if (days < 7) return t(WEEK[d.getDay()]);
  return t("%s/%s", d.getMonth() + 1, d.getDate());
}

// 列表里只给最粗的粒度：今天给时刻，更早给日子 / The list shows the coarsest useful unit
function listTime(ts) {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  return dayLabel(d);
}

function sepTime(ts) {
  const d = new Date(ts * 1000);
  return `${dayLabel(d)} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// ---- 数据 / Data ----

async function api(path, body) {
  const headers = {};
  if (TOKEN) headers["Authorization"] = "Bearer " + TOKEN;
  const res = await fetch(BACKEND + path, body ? {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: JSON.stringify(body),
  } : { headers });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || ("HTTP " + res.status));
    err.status = res.status;
    throw err;
  }
  return data;
}

async function refetchState() {
  const s = await api("/api/state");
  state = s;
  events = s.events || [];
  if (events.length) lastId = Math.max(lastId, events[events.length - 1].id);
  renderAll();
}

function scheduleRefetch() {
  clearTimeout(refetchTimer);
  refetchTimer = setTimeout(() => refetchState().catch(console.error), 250);
}

let sseFailures = 0;

function showOffline() {
  $("offlineBanner").hidden = false;
  $("offlineText").textContent = REMOTE
    ? t("Engine unreachable (%s) — that device may be off or the network is down; retrying…", BACKEND)
    : t("Local engine unreachable, retrying…");
  const btn = $("takeLocalBtn");
  btn.hidden = !window.botBureauNative;
  btn.textContent = REMOTE ? t("Switch to local engine") : t("Restart local engine");
  renderEngineRow(false);
}

function hideOffline() {
  $("offlineBanner").hidden = true;
  renderEngineRow(true);
}

// 消息流的凭据走短时效票据，不是配对码本身。
//
// EventSource 加不了请求头，所以这条连接的凭据只能进 URL；而 URL 会被反向代理原样写进
// access log。配对码是长期的，落进日志就等于泄漏；票据十分钟过期，等人看到日志时早已作废。
// 换票那一步是 POST，能带头，配对码始终留在头里。
//
// The stream authenticates with a short-lived ticket rather than the pairing code itself.
//
// EventSource cannot set headers, so this connection's credential has to live in the URL — and a
// reverse proxy writes URLs verbatim into its access log. The pairing code is long-lived, so landing in
// a log means losing it; a ticket expires in ten minutes and is dead by the time anyone reads that log.
// Fetching the ticket is a POST, which can set headers, so the pairing code never leaves them.
async function streamURL() {
  const base = BACKEND + "/api/events?after=" + lastId;
  if (!TOKEN) return base;
  const r = await api("/api/sse-ticket", {});
  return base + "&ticket=" + encodeURIComponent(r.ticket);
}

async function connectSSE() {
  let url;
  try {
    url = await streamURL();
  } catch (err) {
    // 换不到票据（多半是配对码不对）：走和连接失败一样的重试路径
    // No ticket (usually a wrong pairing code): fall through to the same retry path as a failed connection
    console.log("stream ticket failed: " + err.message);
    if (err.status === 401) {
      $("tokenModal").showModal();
      return;
    }
    setTimeout(connectSSE, 1500);
    return;
  }
  const es = new EventSource(url);
  es.onopen = () => {
    if (sseFailures > 0) console.log("engine reconnected");
    sseFailures = 0;
    hideOffline();
  };
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (ev.id <= lastId) return;
    lastId = ev.id;
    events.push(ev);
    if (ev.kind === "msg" || ev.kind === "tool") {
      // 私聊里也要看见对方在群里的动静，所以群聊事件同样触发当前视图重绘
      // A DM also surfaces that bot's group activity, so group events redraw the current view too
      if (ev.chat === current || relevantToCurrent(ev)) renderMsgs();
      if (ev.chat !== current && ev.source !== "user") unread[ev.chat] = (unread[ev.chat] || 0) + 1;
      renderChatList();
    } else if (ev.kind === "status") {
      const bot = state.bots.find((b) => b.name === ev.source);
      if (bot) { bot.busy = !!ev.busy; renderChatList(); renderHeader(); renderMsgs(); }
    } else {
      scheduleRefetch(); // refresh / approval / approval_done / system
      if (ev.kind === "system" && (ev.chat === current || !ev.chat)) renderMsgs();
    }
  };
  es.onerror = () => {
    es.close();
    sseFailures++;
    console.log("sse disconnected, retry #" + sseFailures);
    if (sseFailures >= 3) showOffline();
    setTimeout(() => {
      refetchState()
        .then(() => {
          sseFailures = 0;
          hideOffline();
          console.log("engine reconnected");
          connectSSE();
        })
        .catch((err) => {
          if (err.status === 401 && REMOTE) $("tokenModal").showModal();
          else connectSSE();
        });
    }, 1500);
  };
}

$("retryBtn").onclick = () => location.reload();
$("takeLocalBtn").onclick = async () => {
  if (!window.botBureauNative) return;
  $("offlineText").textContent = t("Starting the local engine…");
  const r = await window.botBureauNative.connectLocal();
  if (r && !r.ok) $("offlineText").textContent = t("Failed to start: ") + r.error;
};

// ---- 渲染 / Rendering ----

function renderAll() {
  renderChatList();
  renderHeader();
  renderMsgs(true);
  renderTasks();
  renderRoutines();
  renderKeysList();
  renderMCPList();
  renderMCPCatalog();
  renderSkillList();
  renderBundleList();
  renderFooter();
}

// 卡片内的一条：标题 + 次要说明 + 等宽正文 + 操作按钮 / One card item: title, subtitle, mono body, actions
function item({ title, sub, code, actions }) {
  const node = el("div", "item");
  if (title) node.append(title instanceof Node ? title : el("div", "title", title));
  if (sub) node.append(sub instanceof Node ? sub : el("div", "sub", sub));
  if (code) node.append(el("div", "code", code));
  if (actions && actions.length) {
    const bar = el("div", "actions");
    for (const [label, cls, fn] of actions) {
      const b = el("button", cls, label);
      // 按钮本身传给回调：长动作（安装插件）要就地把自己改成"进行中"
      // The button is handed to the callback: a long action (installing a plugin) turns itself into a
      // progress label in place
      b.onclick = (e) => { e.preventDefault(); fn(b); };
      bar.append(b);
    }
    node.append(bar);
  }
  return node;
}

// ---- 会话列表 / Conversation list ----

// 每个会话的最后一条动静，一次遍历算出来 / Last activity per conversation, computed in one pass
function lastByChat() {
  const map = {};
  for (const ev of events) {
    if (!ev.chat) continue;
    if (ev.kind === "msg" || ev.kind === "tool" || ev.kind === "system") map[ev.chat] = ev;
  }
  return map;
}

const pendingIn = (chat) => (state.approvals || []).filter((a) => a.chat === chat).length;

function previewOf(ev) {
  if (!ev) return "";
  if (ev.kind === "tool") return `${titleOf(ev.source)} · ${ev.text}`;
  if (ev.kind === "msg" && isGroupChatId(ev.chat) && ev.source !== "user") return `${titleOf(ev.source)}: ${ev.text}`;
  return ev.text || "";
}

const ICON_PENCIL = "M4 20h4L19.5 8.5a2.1 2.1 0 0 0-3-3L5 17v3Z";
const ICON_TRASH = "M4 7h16M9 7V5.5A1.5 1.5 0 0 1 10.5 4h3A1.5 1.5 0 0 1 15 5.5V7m2 0v12a1.5 1.5 0 0 1-1.5 1.5h-7A1.5 1.5 0 0 1 7 19V7";

function glyphBtn(d, title, onclick, cls) {
  const b = el("button", cls || "");
  b.type = "button";
  b.title = title;
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("width", "14"); svg.setAttribute("height", "14");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.6");
  svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round");
  const path = document.createElementNS(NS_SVG, "path");
  path.setAttribute("d", d);
  svg.append(path);
  b.append(svg);
  b.onclick = (e) => { e.stopPropagation(); onclick(); };
  return b;
}

function convRow({ id, av, name, ev, badge, deletable, groupDel }) {
  const row = el("div", "conv" + (current === id ? " active" : ""));
  const body = el("div", "body");
  const line = el("div", "line");
  line.append(el("span", "name", name));
  const time = listTime(ev && ev.ts);
  if (time) line.append(el("span", "time", time));
  const empty = ev ? previewOf(ev) : (id.startsWith("dm:") && !modelSet(id.slice(3))
    ? t("No model selected")
    : t("No messages yet"));
  body.append(line, el("div", "prev", empty));
  row.append(av, body);
  if (badge) row.append(el("span", "badge", String(badge)));
  const acts = el("div", "conv-acts");
    acts.append(glyphBtn(ICON_PENCIL, t("Settings"),
    () => (isGroupChatId(id) ? openGroupModal(id) : openBotModal(id.slice(3)))));
  if (groupDel) {
    acts.append(glyphBtn(ICON_TRASH, t("Delete this group"), async () => {
      const ok = await ask({
        title: t("Delete \"%s\"", name),
        hint: t("Messages in this group are kept."),
        ok: t("Delete"),
        danger: true,
      });
      if (ok) {
        await api("/api/groups/delete", { id }).catch((err) => toast(err.message));
        if (current === id) switchChat("group");
      }
    }, "danger"));
  }
  if (deletable) {
    acts.append(glyphBtn(ICON_TRASH, t("Remove this bot"), async () => {
      const ok = await ask({
        title: t("Remove %s", name),
        hint: t("Its workspace and memory stay under data/."),
        ok: t("Remove"),
        danger: true,
      });
      if (ok) api("/api/bots/delete", { name: id.slice(3) }).catch((err) => toast(err.message));
    }, "danger"));
  }
  row.append(acts);
  row.onclick = () => switchChat(id);
  return row;
}

function renderChatList() {
  const nav = $("chatList");
  const last = lastByChat();
  const rows = [
    ...groupsList().map((g) => ({
      id: g.id, name: groupTitleOf(g.id), av: groupAvatarFor(g.id, 40),
      ev: last[g.id], badge: (unread[g.id] || 0) + pendingIn(g.id),
      groupDel: g.id !== "group",
    })),
    ...state.bots.map((b) => ({
      id: "dm:" + b.name, name: titleOf(b.name), av: avatar(b.name, 40, b.busy),
      ev: last["dm:" + b.name], badge: (unread["dm:" + b.name] || 0) + pendingIn("dm:" + b.name),
      deletable: true, role: b.role,
    })),
  ];
  const q = filter.trim().toLowerCase();
  const shown = q
    ? rows.filter((r) => (r.name + " " + r.id + " " + (r.role || "") + " " + previewOf(r.ev)).toLowerCase().includes(q))
    : rows;
  // 有动静的排前面，按时间倒序；没聊过的保持配置顺序垫底
  // Conversations with activity come first, newest on top; untouched ones keep config order at the bottom
  shown.sort((a, b) => ((b.ev && b.ev.ts) || 0) - ((a.ev && a.ev.ts) || 0));
  nav.replaceChildren(...shown.map(convRow));
  if (!shown.length) {
    nav.append(el("div", "empty", rows.length
      ? t("No matching conversations")
      : t("No teammates yet — press ＋ to hire your first one")));
  }
}

// 一个会话都没有的时候，主区给一句话的空态，而不是摆一个空群的标题和输入框
// —— 界面里出现"群聊 0 名成员"只会让人以为里面有人。
//
// With no conversations at all the main pane shows one line of empty state rather than a phantom group
// header and composer: a "Group chat · 0 members" title only suggests somebody is in there.
function hasAnyChat() {
  return state.bots.length > 0 || (state.groups || []).length > 0;
}

function renderHeader() {
  const h = $("chatHeader");
  h.replaceChildren();
  const blank = !hasAnyChat();
  $("composer").hidden = blank;
  if (blank) {
    $("input").placeholder = "";
    return;
  }
  if (isGroupChatId(current)) {
    const members = groupMembersOf(current);
    const n = members.length;
    const unset = members.filter((n) => !modelSet(n)).length;
    h.append(groupAvatarFor(current, 26), el("span", "title", groupTitleOf(current)));
    h.append(el("span", "desc", unset
      ? t("%s members · %s without a model", n, unset)
      : t("%s members · call someone by name to give them the job", n)));
  } else {
    const name = current.slice(3);
    const b = state.bots.find((x) => x.name === name);
    h.append(avatar(name, 26, b && b.busy), el("span", "title", titleOf(name)));
    if (b) {
      if (!modelSet(name)) {
        h.append(el("span", "desc", t("No model set · click to choose")));
      } else {
        const busy = b.busy ? t("working") : t("idle");
        const queued = b.queued ? t(" · queued %s", b.queued) : "";
        h.append(el("span", "desc", `${b.role} · ${busy}${queued}`));
      }
    }
  }
  // 点头像或名字直接进这个会话的设置 / Clicking the avatar or name opens this conversation's settings
  for (const node of [...h.children]) {
    node.style.cursor = "pointer";
    node.onclick = () => (isGroupChatId(current) ? openGroupModal(current) : openBotModal(current.slice(3)));
  }
  h.append(el("span", "spacer"));
  const busyNames = busyInChat();
  if (busyNames.length) {
    const stop = el("button", "icon-btn labeled danger");
    stop.type = "button";
    stop.title = t("Stop the current task");
    const stopSvg = document.createElementNS(NS_SVG, "svg");
    stopSvg.setAttribute("width", "13"); stopSvg.setAttribute("height", "13");
    stopSvg.setAttribute("viewBox", "0 0 24 24");
    stopSvg.setAttribute("fill", "currentColor");
    const sq = document.createElementNS(NS_SVG, "rect");
    sq.setAttribute("x", "6"); sq.setAttribute("y", "6");
    sq.setAttribute("width", "12"); sq.setAttribute("height", "12"); sq.setAttribute("rx", "2");
    stopSvg.append(sq);
    stop.append(stopSvg, el("span", "", t("Stop")));
    stop.onclick = () => {
      for (const name of busyNames) {
        api("/api/cancel", { name }).catch((err) => toast(err.message));
      }
    };
    h.append(stop);
  }
  setPlusBtn();

  $("input").placeholder = isGroupChatId(current)
    ? t("Message the team… call a name to give someone the job")
    : t("Message %s", titleOf(current.slice(3)));
}

// 当前会话里正在干活的 bot。标题栏的停止按钮和消息流的打字气泡共用这一套判断。
// Which bots are working in the current conversation. Shared by the header's stop button and the
// message stream's typing bubbles.
function busyInChat() {
  return isGroupChatId(current)
    ? state.bots.filter((b) => b.busy && inGroupOf(current, b.name)).map((b) => b.name)
    : state.bots.filter((b) => b.busy && current === "dm:" + b.name).map((b) => b.name);
}

// ---- 消息流 / Message stream ----

// 私聊里"对方在群里的动静"：与当前 bot 有关的群聊事件
// In a DM, "what this bot is up to in the group": group events involving the current bot
function relevantToCurrent(ev) {
  if (!current.startsWith("dm:") || !isGroupChatId(ev.chat)) return false;
  const name = current.slice(3);
  return ev.source === name || (ev.text || "").includes("@" + name);
}

function bubbleNode(text) {
  let node;
  try { node = renderMarkdown(text); }
  catch { node = el("div", "md", text); }
  node.classList.add("bubble");
  return node;
}

// 群聊里每条 bot 消息左边留一列给头像，但连着说的只有第一条真的挂头像，
// 后面几条放一个等宽占位——头像每条都挂会把一段连贯的发言剁成一串独立卡片。
// gutter 决定"这一列存不存在"（私聊里根本不需要），showWho 决定"这一列里有没有东西"。
//
// In a group chat every bot message reserves a column on the left for an avatar, but only the first
// message of a run actually carries one; the rest hold an equal-width spacer. Repeating the avatar on
// every line chops one continuous statement into a stack of unrelated cards.
// gutter decides whether the column exists at all (a DM has no use for it); showWho decides whether
// anything sits in it.
function msgNode(ev, gutter, showWho) {
  const node = el("div", "msg" + (ev.source === "user" ? " user" : ""));
  const body = el("div", "msg-body");
  if (showWho && ev.source !== "user") body.append(el("div", "who", titleOf(ev.source)));
  body.append(bubbleNode(ev.text));
  if (gutter && ev.source !== "user") {
    node.append(showWho ? avatar(ev.source, MSG_AV) : el("span", "av-slot"));
  }
  node.append(body);
  return node;
}

// 把一串工具事件收成一行小字，点开才展开。
// 聊天界面里最要紧的是对话本身，中间过程默认让路；但它必须一直看得见——
// 长任务时用户唯一能确认"还在跑"的证据就是这里的步数在涨。
//
// Folds a run of tool events into one small line that expands on click.
// The conversation is what matters in a chat window, so the intermediate steps step aside by default —
// but they must stay visible: during a long task, this growing step count is the only evidence the
// user has that anything is still happening.
function traceNode(key, evs, live) {
  const box = el("div", "trace" + (live ? " live" : ""));
  const open = expanded.has(key);
  const head = el("button", "trace-head");
  head.type = "button";
  head.append(el("span", "chev" + (open ? " open" : "")));
  const n = evs.length;
  head.append(el("span", "", live
    ? t("Working · %s steps", n)
    : t("Thinking and progress · %s steps", n)));
  head.onclick = () => {
    if (open) expanded.delete(key); else expanded.add(key);
    renderMsgs();
  };
  box.append(head);
  if (open) {
    const body = el("div", "trace-body");
    for (const ev of evs) body.append(toolNode(ev));
    box.append(body);
  }
  return box;
}

// 正在干活时的气泡：iMessage 那样的三点省略号。
// A typing bubble while work is in flight: the three-dot ellipsis, iMessage style.
function typingNode(name, inGroup) {
  const node = el("div", "msg");
  const body = el("div", "msg-body");
  if (inGroup) body.append(el("div", "who", titleOf(name)));
  const bubble = el("div", "bubble typing");
  for (let i = 0; i < 3; i++) bubble.append(el("span", "dot"));
  body.append(bubble);
  if (inGroup) node.append(avatar(name, MSG_AV));
  node.append(body);
  return node;
}

function toolNode(ev) {
  const node = el("div", "tool");
  node.append(el("span", "tool-who", titleOf(ev.source)), document.createTextNode(ev.text));
  return node;
}

// 折叠块：把 bot 在别处的一串往返收成一行，点开才展开
// Fold: a run of the bot's activity elsewhere collapses to one line until clicked
function foldNode(key, evs) {
  const box = el("div");
  const gid = (evs[0] && isGroupChatId(evs[0].chat)) ? evs[0].chat : "group";
  if (expanded.has(key)) {
    const back = el("button", "fold");
    back.type = "button";
    back.append(groupAvatarFor(gid, 18), el("span", "", t("Hide %s from the group chat", evs.length)));
    back.onclick = () => { expanded.delete(key); renderMsgs(); };
    box.append(back);
    let prev = null;
    for (const ev of evs) {
      box.append(ev.kind === "tool" ? toolNode(ev) : msgNode(ev, true, !prev || prev.source !== ev.source));
      prev = ev;
    }
    return box;
  }
  const btn = el("button", "fold");
  btn.type = "button";
  const n = evs.filter((e) => e.kind === "msg").length || evs.length;
  btn.append(groupAvatarFor(gid, 18), el("span", "", t("%s messages in the group chat", n)));
  btn.onclick = () => { expanded.add(key); renderMsgs(); };
  box.append(btn);
  return box;
}

function approvalNode(ap) {
  const box = el("div", "approval");
  box.append(el("div", "head", t("%s requests permission to run", titleOf(ap.bot))));
  box.append(el("div", "code", ap.action));
  const acts = el("div", "acts");
  const yes = el("button", "yes", t("Approve"));
  yes.type = "button";
  yes.onclick = () => api("/api/approve", { id: ap.id, approved: true }).catch((e) => toast(e.message));
  const no = el("button", "no", t("Reject"));
  no.type = "button";
  no.onclick = async () => {
    const reason = await ask({
      title: t("Reject this action"),
      hint: t("You can explain why — the bot will see it."),
      field: true,
      fieldLabel: t("Reason"),
      placeholder: t("Optional"),
      ok: t("Reject"),
      danger: true,
    });
    if (reason === null) return;
    api("/api/approve", { id: ap.id, approved: false, reason }).catch((e) => toast(e.message));
  };
  acts.append(yes, no);
  box.append(acts);
  return box;
}

const SEP_GAP = 15 * 60; // 超过这个间隔就打一条时间分隔 / a gap this long gets a time separator

function renderMsgs(forceBottom) {
  const box = $("msgs");
  if (!hasAnyChat()) {
    box.replaceChildren();
    const blank = el("div", "blank");
    blank.append(
      el("div", "blank-title", t("Nobody works here yet")),
      el("div", "blank-desc", t("Press ＋ in the sidebar to hire your first teammate, then talk to it here.")),
    );
    box.append(blank);
    return;
  }
  // 只有本来就贴在底部时才跟着新消息滚动，别把正在翻历史的用户拽回去
  // Follow new messages only when already pinned to the bottom — never yank a user who is reading history
  const stick = forceBottom || box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  box.replaceChildren();
  let lastTs = 0;
  let lastSource = null;
  let buffer = [];   // 攒起来的"别处的动静" / buffered activity from elsewhere
  let bufferKey = "";
  let trace = [];    // 攒起来的工具事件，成串折成一行 / buffered tool events, folded into one line
  let traceKey = "";

  const flushTrace = (live) => {
    if (!trace.length) return;
    box.append(traceNode(traceKey, trace, !!live));
    trace = [];
    lastSource = null;
  };

  // 注意：这里不能顺手 flushTrace——每轮循环都会调 flush()，
  // 那样每条工具事件都会被下一轮立刻收口，一串过程就散成一行一步。
  // Note: this must not flush the trace. flush() runs every iteration, so doing that would close the
  // trace after each single tool event and shatter one run of steps into a line per step.
  const flush = () => {
    if (!buffer.length) return;
    box.append(foldNode(bufferKey, buffer));
    buffer = [];
    lastSource = null;
  };

  for (const ev of events) {
    const kindOK = ev.kind === "msg" || ev.kind === "tool" || ev.kind === "system";
    if (!kindOK) continue;
    const inChat = ev.chat === current || (!ev.chat && ev.kind === "system");
    if (!inChat) {
      if (relevantToCurrent(ev) && ev.kind !== "system") {
        if (!buffer.length) bufferKey = "fold:" + current + ":" + ev.id;
        buffer.push(ev);
      }
      continue;
    }
    flush();
    if (ev.ts && ev.ts - lastTs > SEP_GAP) {
      flushTrace(false);
      box.append(el("div", "tsep", sepTime(ev.ts)));
      lastSource = null;
    }
    if (ev.ts) lastTs = ev.ts;

    if (ev.kind === "system") {
      flushTrace(false);
      box.append(el("div", "sys", ev.text));
      lastSource = null;
    } else if (ev.kind === "tool") {
      if (!trace.length) traceKey = "trace:" + current + ":" + ev.id;
      trace.push(ev);
    } else {
      flushTrace(false);
      const runStart = ev.source !== lastSource;
      // 群聊里才需要标发言人和头像；私聊双方明确 / Only the group chat needs speaker labels and avatars
      const node = msgNode(ev, isGroupChatId(current), runStart);
      if (runStart) node.classList.add("run-start");
      box.append(node);
      lastSource = ev.source;
    }
  }
  flush();

  // 还在干活的 bot：过程行标成"进行中"，后面跟一个打字气泡。
  // 这是长思考期间用户唯一的实时反馈——没有它，界面看起来就像卡住了。
  //
  // Bots still working: the trace line is marked live and a typing bubble follows it. During a long
  // think this is the user's only real-time feedback; without it the window just looks stuck.
  const working = busyInChat();
  flushTrace(working.length > 0);
  for (const name of working) box.append(typingNode(name, isGroupChatId(current)));

  // 待审批就地摆在对话末尾，不用去别处找 / Pending approvals sit at the end of the conversation
  for (const ap of state.approvals || []) {
    if (ap.chat === current) box.append(approvalNode(ap));
  }
  if (stick) box.scrollTop = box.scrollHeight;
}

// ---- 工作台 / Workbench ----

function applySideSplit(frac) {
  const body = $("sideBody");
  const split = $("sideSplit");
  if (!body || !split) return;
  const usable = body.clientHeight - split.offsetHeight;
  if (usable <= 0) return;
  const min = 72;
  const px = Math.min(usable - min, Math.max(min, frac * usable));
  const next = px / usable;
  body.style.setProperty("--side-chat", (next * 100) + "%");
  localStorage.setItem("botbureau_side_split", String(next));
}

function wireSideSplit() {
  const split = $("sideSplit");
  const body = $("sideBody");
  if (!split || !body) return;
  const saved = parseFloat(localStorage.getItem("botbureau_side_split"));
  if (saved > 0 && saved < 1) applySideSplit(saved);
  split.onpointerdown = (e) => {
    e.preventDefault();
    split.classList.add("dragging");
    split.setPointerCapture(e.pointerId);
    const move = (ev) => {
      const r = body.getBoundingClientRect();
      applySideSplit((ev.clientY - r.top) / r.height);
    };
    const up = () => {
      split.classList.remove("dragging");
      split.removeEventListener("pointermove", move);
      split.removeEventListener("pointerup", up);
    };
    split.addEventListener("pointermove", move);
    split.addEventListener("pointerup", up);
  };
}

function renderTasks() {
  const wrap = $("tasks");
  wrap.replaceChildren();
  const tasks = state.tasks || [];
  if (!tasks.length) {
    wrap.append(el("div", "empty", t("Bots claim their share here when they split up a task")));
    return;
  }
  for (const task of tasks) {
    const title = el("div", "title");
    title.append(el("span", "pill " + task.status, task.status), document.createTextNode(" " + task.title));
    const sub = el("div", "sub");
    sub.append(avatar(task.owner, 14), el("span", "", " " + titleOf(task.owner)));
    sub.style.display = "flex";
    sub.style.alignItems = "center";
    sub.style.gap = "5px";
    if (task.note) sub.append(el("span", "", "· " + task.note));
    wrap.append(item({ title, sub }));
  }
}

function renderRoutines() {
  const wrap = $("routines");
  wrap.replaceChildren();
  if (!state.routines.length) {
    wrap.append(el("div", "empty", t('Say "every … do …" to a bot to create one')));
    return;
  }
  for (const r of state.routines) {
    const next = new Date(r.next_run * 1000);
    const when = `${pad2(next.getMonth() + 1)}-${pad2(next.getDate())} ${pad2(next.getHours())}:${pad2(next.getMinutes())}`;
    wrap.append(item({
      title: r.name,
      sub: t("%s · every %s min · next %s", r.bot, r.every_minutes, when),
      code: r.prompt.length > 90 ? r.prompt.slice(0, 90) + "…" : r.prompt,
      actions: [[t("Delete"), "danger", () => api("/api/routines/delete", { name: r.name }).catch((e) => toast(e.message))]],
    }));
  }
}

function checkRow({ name, label: text, avatarSize, meta, checked, dataset, onchange }) {
  const row = el("div", "check-row");
  const label = el("label");
  const cb = document.createElement("input");
  cb.type = "checkbox";
  if (checked) cb.checked = true;
  if (dataset) Object.assign(cb.dataset, dataset);
  cb.value = name;
  if (onchange) cb.onchange = () => onchange(cb);
  label.append(cb);
  if (avatarSize) label.append(avatar(name, avatarSize));
  label.append(el("span", "", text || name));
  if (meta) label.append(el("span", "meta", meta));
  row.append(label);
  return row;
}

function renderGroupMembers(id) {
  const wrap = $("groupMembers");
  if (!wrap) return;
  wrap.replaceChildren();
  if (!state.bots.length) {
    wrap.append(el("div", "empty", t("No bots yet")));
    return;
  }
  for (const b of state.bots) {
    wrap.append(checkRow({
      name: b.name, label: titleOf(b.name), avatarSize: 20, meta: b.role, checked: inGroupOf(id, b.name),
      onchange: (cb) => groupMemberDraft[id][b.name] = cb.checked,
    }));
  }
}

// ---- 侧栏底部 / Sidebar footer ----

function renderEngineRow(online) {
  $("engineDot").style.background = online === false ? "var(--danger)" : "var(--ok)";
  $("engineLabel").textContent = REMOTE
    ? t("Remote engine") + " · " + BACKEND.replace(/^https?:\/\//, "")
    : t("Local engine");
}

function renderFooter() {
  const servers = state.mcp || [];
  const okCount = servers.filter((s) => s.status === "connected").length;
  $("mcpNote").textContent = servers.length ? `${okCount}/${servers.length}` : "";
  renderEngineRow($("offlineBanner").hidden);
}

function renderKeysList() {
  const wrap = $("keysList");
  wrap.replaceChildren();
  const keys = state.keys || [];
  if (!keys.length) {
    wrap.append(el("div", "empty", t("No keys saved yet")));
    return;
  }
  for (const k of keys) {
    wrap.append(item({
      title: k.name, sub: k.masked,
      actions: [[t("Delete"), "danger", () => api("/api/keys/delete", { name: k.name }).catch((err) => toast(err.message))]],
    }));
  }
}

function renderMCPList() {
  const wrap = $("mcpList");
  wrap.replaceChildren();
  const servers = state.mcp || [];
  if (!servers.length) {
    wrap.append(el("div", "empty", t("No plugins added yet")));
    return;
  }
  for (const s of servers) {
    const color = { connected: "var(--ok)", connecting: "var(--warn)", error: "var(--danger)" }[s.status] || "var(--text-3)";
    const kind = s.transport === "http" ? t("remote") : t("local");
    const all = s.all_tools || s.tools || [];
    const nTools = (s.tools || []).length;
    const title = el("div", "title");
    const dot = el("span", "");
    Object.assign(dot.style, { background: color, width: "7px", height: "7px", borderRadius: "50%", display: "inline-block", marginRight: "7px" });
    title.append(dot, document.createTextNode(s.name));
    // 选了子集就把"6 / 90"写出来，不然用户看不出为什么某个工具用不了
    // Show "6 of 90" once a subset is chosen, or there is no way to tell why some tool is unavailable
    const countText = nTools === all.length
      ? t("%s tools", nTools)
      : t("%s of %s tools", nTools, all.length);
    const actions = [
      [t("Reconnect"), "", () => api("/api/mcp/reconnect", { name: s.name }).catch((err) => toast(err.message))],
    ];
    if (all.length > 1) {
      actions.push([t("Choose tools"), "", () => openToolPicker(s)]);
    }
    // 远程连接器给一个授权入口：OAuth 那批（Linear/Notion/GitHub…）没有它就完全接不上
    // Remote connectors get an authorize action: without it the OAuth ones (Linear, Notion, GitHub, ...)
    // cannot be connected at all
    if (s.transport === "http") {
      actions.push([t("Authorize"), "", () => startMCPOAuth(s.name)]);
    }
    wrap.append(item({
      title,
      sub: t("%s · %s", kind, countText),
      code: s.error || (s.tools || []).join(", "),
      actions: actions.concat([
        [t("Delete"), "danger", async () => {
          const ok = await ask({
            title: t("Delete plugin %s", s.name),
            hint: t("Bots assigned to it will be unsubscribed."),
            ok: t("Delete"),
            danger: true,
          });
          if (ok) api("/api/mcp/delete", { name: s.name }).catch((err) => toast(err.message));
        }],
      ]),
    }));
  }
}

// openToolPicker 让用户挑这个插件要暴露哪些工具。大插件（GitHub 官方有九十多个工具）
// 不挑的话，工具列表会把上下文撑爆，模型也更容易选错。
// openToolPicker lets the user choose which of a plugin's tools are exposed. Without it a large plugin
// (the official GitHub one has ninety-odd tools) floods the tool list, eating context and making the
// model likelier to pick the wrong one.
function openToolPicker(server) {
  const all = server.all_tools || [];
  const on = new Set(server.tools || []);
  const box = $("toolPickList");
  box.replaceChildren();
  for (const name of all) {
    box.append(checkRow({ name, checked: on.has(name), dataset: { tool: "1" } }));
  }
  $("toolPickTitle").textContent = t("Tools in %s", server.name);
  $("toolPickErr").textContent = "";
  $("toolPickModal").dataset.server = server.name;
  $("toolPickModal").dataset.total = String(all.length);
  $("toolPickModal").showModal();
}

async function saveToolPick(all) {
  const modal = $("toolPickModal");
  const boxes = [...$("toolPickList").querySelectorAll("input[data-tool]")];
  const picked = all ? boxes.map((cb) => cb.value) : boxes.filter((cb) => cb.checked).map((cb) => cb.value);
  // 全选就存空数组：这样以后插件更新加了新工具会自动跟上，而不是停在当时那一份名单上
  // Selecting everything stores an empty array, so a later plugin update that adds tools is picked up
  // automatically instead of being frozen to the list as it stood
  const tools = picked.length === Number(modal.dataset.total) ? [] : picked;
  try {
    await api("/api/mcp/tools", { name: modal.dataset.server, tools });
    modal.close();
  } catch (err) {
    $("toolPickErr").textContent = err.message;
  }
}

// startMCPOAuth 走完整的 OAuth：后端发现端点并注册客户端，这里把授权页打开，然后轮询结果。
// startMCPOAuth runs the full OAuth flow: the backend discovers the endpoints and registers a client,
// this opens the authorization page and then polls for the outcome.
let mcpOAuthTimer = null;
async function startMCPOAuth(name) {
  $("mcpErr").textContent = t("Preparing authorization for %s…", name);
  try {
    const st = await api("/api/mcp/oauth/start", { name });
    if (st.url) window.open(st.url, "_blank", "noopener");
    $("mcpErr").textContent = t("Approve in the browser, then come back.");
    clearInterval(mcpOAuthTimer);
    mcpOAuthTimer = setInterval(async () => {
      try {
        const cur = await api("/api/mcp/oauth/status?name=" + encodeURIComponent(name));
        if (cur.status === "done") {
          clearInterval(mcpOAuthTimer);
          $("mcpErr").textContent = t("%s is authorized.", name);
        } else if (cur.status === "error") {
          clearInterval(mcpOAuthTimer);
          $("mcpErr").textContent = cur.error || t("Authorization failed");
        }
      } catch {}
    }, 1500);
  } catch (err) {
    $("mcpErr").textContent = err.message;
  }
}

// ---- 技能 / Skills ----

function renderSkillList() {
  const wrap = $("skillList");
  if (!wrap) return;
  wrap.replaceChildren();
  const skills = state.skills || [];
  if (!skills.length) {
    wrap.append(el("div", "empty", t("No skills yet — install a plugin package that ships some, or drop a folder with a SKILL.md into data/skills/")));
    return;
  }
  for (const s of skills) {
    wrap.append(item({
      title: s.name,
      sub: s.description,
      // 来源写出来：同名技能只有一个生效，出问题时得知道生效的是哪一个
      // Spell out the origin: only one skill of a given name is in force, and when something looks wrong
      // you need to know which one that is
      code: s.source === "local" ? t("local · %s", s.dir) : t("from plugin %s", s.source),
    }));
  }
}

// ---- 插件包 / Plugin packages ----

function renderBundleList() {
  const wrap = $("bundleList");
  if (!wrap) return;
  wrap.replaceChildren();
  const bundles = state.plugins || [];
  if (!bundles.length) {
    wrap.append(el("div", "empty", t("No plugin packages installed")));
    return;
  }
  for (const b of bundles) {
    const brought = [];
    if (b.mcp_servers?.length) brought.push(t("%s plugins", b.mcp_servers.length));
    if (b.skills?.length) brought.push(t("%s skills", b.skills.length));
    if (b.agents?.length) brought.push(t("%s teammates", b.agents.length));
    const actions = [];
    // 包里的每个同事模板都给一个"加进团队"：这是 Bot Bureau 独有的一步——
    // 别处的 agent 只能当子代理用，这里它就是一位真正的成员。
    // Each teammate template gets an "add to the team" action — the step unique to Bot Bureau, where an
    // agent elsewhere can only be a subagent and here becomes an actual member.
    for (const a of b.agents || []) {
      actions.push([t("Add %s", a.name), "", () => addAgentAsBot(a)]);
    }
    // 升级是就地调和：你挑好的工具子集、跑过的授权都留着，不像"卸了重装"那样清零
    // Upgrading reconciles in place: the tool subset you picked and the authorization you completed stay,
    // unlike a remove-and-reinstall that zeroes both
    if (b.source) {
      actions.push([t("Update"), "", (btn) => updateBundle(b, btn)]);
    }
    actions.push([t("Remove"), "danger", async () => {
      const ok = await ask({
        title: t("Remove package %s", b.name),
        hint: t("Its plugins and skills go with it. Teammates you already created stay."),
        ok: t("Remove"), danger: true,
      });
      if (ok) api("/api/plugins/remove", { name: b.name }).catch((err) => toast(err.message));
    }]);
    const sub = [b.description, b.version && "v" + b.version, brought.join(" · ")].filter(Boolean).join(" — ");
    wrap.append(item({
      title: b.name,
      sub,
      // 不支持的部分要显眼：装完悄悄少一半功能比直接说出来糟糕得多
      // What is unsupported has to be visible: silently dropping half a package is far worse than saying so
      code: b.ignored?.length ? t("Not supported: ") + b.ignored.join("; ") : "",
      actions,
    }));
  }
}

// addAgentAsBot 把包里的同事模板灌进新建 bot 的表单。不直接建：模型和权限得用户自己定，
// 而且他应该先看见这个角色到底写了什么。
// addAgentAsBot loads a bundled teammate template into the new-bot form. It does not create one outright:
// the model and permissions are the user's call, and they ought to see what the role actually says first.
// openMarketplacePicker 把清单里的插件列出来，一条一个「安装」。
// openMarketplacePicker lists the plugins in a marketplace, one Install per entry.
function openMarketplacePicker(source, market) {
  const wrap = $("marketList");
  wrap.replaceChildren();
  const installed = new Set((state.plugins || []).map((p) => p.name));
  for (const entry of market.plugins || []) {
    const done = installed.has(entry.name);
    wrap.append(item({
      title: entry.name,
      sub: entry.description || "",
      actions: done ? [] : [[t("Install"), "", async (btn) => {
        btn.disabled = true;
        btn.textContent = t("Installing…");
        $("marketErr").textContent = t("Installing %s…", entry.name);
        try {
          const res = await api("/api/plugins/install", { source, plugin: entry.name });
          const p = res.plugin || {};
          $("marketErr").textContent = t("%s installed", p.name || entry.name);
          $("bundleSrc").value = "";
        } catch (err) {
          $("marketErr").textContent = err.message;
          btn.disabled = false;
          btn.textContent = t("Install");
        }
      }]],
    }));
  }
  $("marketTitle").textContent = t("Plugins in %s", market.name);
  $("marketErr").textContent = "";
  $("marketModal").showModal();
}

async function updateBundle(bundle, btn) {
  if (btn) {
    btn.disabled = true;
    btn.textContent = t("Updating…");
  }
  $("bundleErr").textContent = t("Fetching the latest %s…", bundle.name);
  try {
    const res = await api("/api/plugins/update", { name: bundle.name });
    const p = res.plugin || {};
    $("bundleErr").textContent = p.version
      ? t("%s updated to v%s", p.name, p.version)
      : t("%s updated", p.name);
  } catch (err) {
    $("bundleErr").textContent = err.message;
    if (btn) {
      btn.disabled = false;
      btn.textContent = t("Update");
    }
  }
}

function addAgentAsBot(agent) {
  $("mcpModal").close();
  openBotModal("", {
    name: agent.name,
    display_name: agent.name,
    // 角色栏用代理名（code-reviewer → code reviewer）：留空的话后端会填 "assistant"，
    // 那就把包作者本来说清楚了的角色丢了。
    // The role comes from the agent's name (code-reviewer → code reviewer): leaving it empty makes the
    // backend fall back to "assistant", throwing away the role the package author already stated.
    role: agent.name.replace(/[-_]+/g, " "),
    description: agent.description || "",
    prompt: agent.prompt || "",
  });
}

function renderBotMcpChoices(selected = []) {
  const wrap = $("botMcp");
  wrap.replaceChildren();
  const servers = state.mcp || [];
  if (!servers.length) {
    wrap.append(el("div", "empty", t("No plugins yet — add one in the plugins panel")));
    return;
  }
  for (const s of servers) {
    const nTools = (s.tools || []).length;
    wrap.append(checkRow({
      name: s.name, meta: t("%s tools", nTools), dataset: { mcp: "1" },
      checked: selected.includes(s.name),
    }));
  }
}

function switchChat(id) {
  current = id;
  delete unread[id];
  closePop();
  renderChatList();
  renderHeader();
  renderMsgs(true);
  $("input").focus();
}

// ---- 搜索 / Search ----

$("search").addEventListener("input", (e) => {
  filter = e.target.value;
  renderChatList();
});

// ---- 输入 / Input ----

$("composer").addEventListener("submit", (e) => {
  e.preventDefault();
  const input = $("input");
  const text = input.value.trim();
  if (!text) return;
  input.value = "";
  input.style.height = "auto";
  api("/api/send", { chat: current, text }).catch((err) => toast(t("Send failed: ") + err.message));
});
$("input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    $("composer").requestSubmit();
  }
});
// 输入框随内容长高，到上限再滚动 / The field grows with its content, then scrolls
$("input").addEventListener("input", (e) => {
  e.target.style.height = "auto";
  e.target.style.height = Math.min(e.target.scrollHeight, 160) + "px";
});

function setPlusBtn() {
  const btn = $("plusBtn");
  const mention = isGroupChatId(current);
  btn.title = mention ? t("Mention a teammate") : t("Jump to another chat");
  btn.setAttribute("data-i18n-title", mention ? "Mention a teammate" : "Jump to another conversation");
  btn.setAttribute("data-en-title", mention ? "Mention a teammate" : "Jump to another chat");
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("width", "18");
  svg.setAttribute("height", "18");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.8");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  if (mention) {
    const c = document.createElementNS(NS_SVG, "circle");
    c.setAttribute("cx", "12"); c.setAttribute("cy", "12"); c.setAttribute("r", "4");
    const p = document.createElementNS(NS_SVG, "path");
    p.setAttribute("d", "M16 12v1.6a2.4 2.4 0 0 0 4.8 0V12a8.8 8.8 0 1 0-3.5 7");
    svg.append(c, p);
  } else {
    const p = document.createElementNS(NS_SVG, "path");
    p.setAttribute("d", "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2");
    const c = document.createElementNS(NS_SVG, "circle");
    c.setAttribute("cx", "9"); c.setAttribute("cy", "7"); c.setAttribute("r", "4");
    const p2 = document.createElementNS(NS_SVG, "path");
    p2.setAttribute("d", "M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75");
    svg.append(p, c, p2);
  }
  btn.replaceChildren(svg);
}

let pop = null;
function closePop() {
  if (pop) { pop.remove(); pop = null; }
}
document.addEventListener("click", (e) => {
  if (pop && !pop.contains(e.target) && e.target.closest("#plusBtn") === null) closePop();
});

$("plusBtn").onclick = (e) => {
  e.stopPropagation();
  if (pop) { closePop(); return; }
  pop = el("div", "pop");
  const isGroup = isGroupChatId(current);
  const entries = isGroup
    ? groupMembersOf(current).map((n) => ({ id: n, name: n, meta: (botOf(n) || {}).role }))
    : [
        ...groupsList().map((g) => ({ id: g.id, name: g.id, title: groupTitleOf(g.id) })),
        ...state.bots.filter((b) => "dm:" + b.name !== current).map((b) => ({ id: "dm:" + b.name, name: b.name, meta: b.role })),
      ];
  if (!entries.length) pop.append(el("div", "empty", t("No members in the group yet")));
  for (const en of entries) {
    const b = el("button", "");
    b.type = "button";
    if (isGroup) {
      b.append(avatar(en.name, 20), el("span", "", titleOf(en.name)));
      if (en.meta) b.append(el("span", "meta", en.meta));
    } else if (!en.id.startsWith("dm:")) {
      b.append(groupAvatarFor(en.id, 20), el("span", "", en.title));
    } else {
      b.append(avatar(en.name, 20), el("span", "", titleOf(en.name)));
      if (en.meta) b.append(el("span", "meta", en.meta));
    }
    b.onclick = () => {
      closePop();
      if (isGroup) {
        const input = $("input");
        input.value = `@${en.name} ` + input.value;
        input.focus();
      } else {
        switchChat(en.id);
      }
    };
    pop.append(b);
  }
  document.body.append(pop);
  const r = $("plusBtn").getBoundingClientRect();
  pop.style.left = r.left + "px";
  pop.style.bottom = (window.innerHeight - r.top + 8) + "px";
};

// ---- 设置：API Keys / Settings: API Keys ----

$("connectRemoteBtn").onclick = async () => {
  if (!window.botBureauNative) return;
  $("remoteErr").textContent = t("Connecting…");
  const r = await window.botBureauNative.connectTo($("raddr").value);
  // 成功时主进程会重载窗口；失败才会走到这里
  // On success the main process reloads the window; we only reach here on failure
  if (r && !r.ok) $("remoteErr").textContent = r.error;
};
$("backLocalBtn").onclick = async () => {
  if (!window.botBureauNative) return;
  $("remoteErr").textContent = t("Starting the local engine…");
  const r = await window.botBureauNative.connectLocal();
  if (r && !r.ok) $("remoteErr").textContent = r.error;
};

function renderTG() {
  const s = state.telegram || {};
  const btn = $("tgToggleBtn");
  btn.textContent = s.enabled ? t("Turn off") : t("Turn on");
  let text = "";
  if (!s.has_token) {
    text = t("No token configured (save TELEGRAM_BOT_TOKEN on the Keys tab)");
  } else if (s.enabled && s.running) {
    const bind = s.bind === "group" || !s.bind ? t("Group chat") : t("DM with %s", s.bind);
    text = t("Connected as @%s", s.bot || "?")
      + (s.owner
        ? t(" · bound to %s · routed to %s (/bind to switch)", s.owner, bind)
        : t(" · waiting for /start to bind"));
  } else if (s.enabled) text = s.error || t("Not running");
  else text = t("Off");
  $("tgStatus").textContent = text;
}

$("tgToggleBtn").onclick = async () => {
  const cur = (state.telegram || {}).enabled;
  try {
    state.telegram = await api("/api/telegram", { enabled: !cur });
    renderTG();
  } catch (err) {
    $("tgStatus").textContent = err.message;
  }
};

function renderPairInfo() {
  const pair = $("pairInfo");
  if (REMOTE) {
    pair.textContent = t("Connected as a client to a remote engine (%s) — all data lives on that device.", BACKEND);
  } else if (TOKEN) {
    pair.replaceChildren(
      document.createTextNode(t("Multi-device sync: other devices on this LAN discover and can connect to this machine when they start Bot Bureau (bots and data stay here). Pairing code:") + " "),
      codeChip(TOKEN),
    );
  } else {
    pair.textContent = t("This machine is local-only (BOTBUREAU_LOCAL_ONLY=1) and not discoverable on the LAN.");
  }
}

let settingsTab = "general";

function showSettingsTab(tab) {
  settingsTab = tab;
  document.querySelectorAll("#keysModal [data-pane]").forEach((p) => { p.hidden = p.dataset.pane !== tab; });
  document.querySelectorAll("#keysModal .settings-nav button").forEach((b) => {
    b.classList.toggle("on", b.dataset.tab === tab);
  });
  // 保存按钮只在「密钥」分栏有意义：其余分栏都是选中即存
  // The save button only means anything on the Keys pane; every other pane saves on selection
  $("keysSaveBtn").hidden = tab !== "keys";
}

$("botPermSel").onchange = renderBotPermNote;
$("botEffortSel").onchange = renderBotEffortNote;

let xaiPollTimer = null;
let chatgptPollTimer = null;

// 订阅登录整行：状态点 + 一句话 + 按钮。和新建 Bot 弹窗里那个是同一套写法，
// 两处看起来一样，用户不用学第二遍。
//
// One row per subscription: status dot, one line of text, buttons. Same shape as the block in the
// new-bot dialog, so the two look alike and nothing has to be learned twice.
function renderOAuthBlock(state_, ids, signedIn, signIn) {
  const login = $(ids.login);
  if (!login) return;
  const s = state_ || {};
  // 对缺元素免疫：这个函数被两处不同的标记复用，少一个 id 不该把整个设置弹窗打不开
  // Tolerant of missing elements: this is reused by two different markups, and one absent id must not
  // take the whole settings dialog down with it
  const set = (id, fn) => { const n = $(id); if (n) fn(n); };
  const dot = $(ids.dot);
  const status = $(ids.status);
  const hint = $(ids.hint);
  set(ids.logout, (n) => { n.hidden = !s.connected; });

  if (dot) {
    dot.style.background = s.connected ? "var(--ok)" : s.pending ? "var(--warn)" : "var(--text-3)";
  }
  if (s.connected && !s.pending) {
    if (status) status.textContent = signedIn;
    if (hint) hint.hidden = true;
    login.textContent = t("Sign in again");
    return;
  }
  login.textContent = signIn;
  if (s.pending) {
    if (status) {
      status.replaceChildren();
      if (s.user_code) {
        status.append(document.createTextNode(t("Enter this code in your browser:") + " "), codeChip(s.user_code));
      } else {
        status.textContent = t("Waiting for approval…");
      }
    }
    if (hint) {
      hint.hidden = !s.url;
      if (s.url) hint.textContent = s.url;
    }
    return;
  }
  if (status) status.textContent = s.status === "error" && s.error ? s.error : t("Not signed in");
  if (hint) hint.hidden = true;
}

function renderXaiOAuth() {
  renderOAuthBlock(state.xai, { login: "xaiLoginBtn", logout: "xaiLogoutBtn", status: "xaiOAuthStatus", hint: "xaiOAuthHint", dot: "xaiDot" },
    t("Signed in to SuperGrok"), t("Sign in to SuperGrok"));
}

function renderChatGPTOAuth() {
  renderOAuthBlock(state.chatgpt, { login: "chatgptLoginBtn", logout: "chatgptLogoutBtn", status: "chatgptOAuthStatus", hint: "chatgptOAuthHint", dot: "chatgptDot" },
    t("Signed in to ChatGPT"), t("Sign in to ChatGPT Plus/Pro"));
}

function startDeviceLogin(kind) {
  const start = kind === "chatgpt" ? "/api/chatgpt/oauth/start" : "/api/xai/oauth/start";
  const statusPath = kind === "chatgpt" ? "/api/chatgpt/oauth/status" : "/api/xai/oauth/status";
  const statusEl = kind === "chatgpt" ? "chatgptOAuthStatus" : "xaiOAuthStatus";
  const paint = kind === "chatgpt" ? renderChatGPTOAuth : renderXaiOAuth;
  return async () => {
    try {
      state[kind] = await api(start, {});
      paint();
      if (state[kind] && state[kind].url) window.open(state[kind].url, "_blank", "noopener");
      clearInterval(kind === "chatgpt" ? chatgptPollTimer : xaiPollTimer);
      const timer = setInterval(async () => {
        try {
          state[kind] = await api(statusPath);
          paint();
          if (state[kind] && (state[kind].connected || state[kind].status === "error") && !state[kind].pending) {
            clearInterval(timer);
          }
        } catch {}
      }, 1500);
      if (kind === "chatgpt") chatgptPollTimer = timer;
      else xaiPollTimer = timer;
    } catch (err) {
      $(statusEl).textContent = err.message;
    }
  };
}

const startXaiLogin = startDeviceLogin("xai");
const startChatGPTLogin = startDeviceLogin("chatgpt");

function openSettings(tab, prefillKey) {
  $("keysForm").reset();
  $("keysErr").textContent = "";
  $("remoteErr").textContent = "";
  $("langSel").value = localePref();
  $("backLocalBtn").hidden = !REMOTE;
  renderTG();
  renderPairInfo();
  renderKeysList();
  renderXaiOAuth();
  renderChatGPTOAuth();
  renderPermLevels();
  showSettingsTab(tab || "general");
  if (prefillKey) {
    const kn = $("keysForm").elements.namedItem("kname");
    if (kn) kn.value = prefillKey;
  }
  $("keysModal").showModal();
  if (prefillKey) {
    const kv = $("keysForm").elements.namedItem("kvalue");
    if (kv) kv.focus();
  }
}

document.querySelectorAll("#keysModal .settings-nav button").forEach((b) => {
  b.onclick = () => showSettingsTab(b.dataset.tab);
});
$("xaiLoginBtn").onclick = startXaiLogin;
$("xaiLogoutBtn").onclick = async () => {
  try {
    state.xai = await api("/api/xai/oauth/logout", {});
    renderXaiOAuth();
  } catch (err) {
    $("xaiOAuthStatus").textContent = err.message;
  }
};
$("chatgptLoginBtn").onclick = startChatGPTLogin;
$("chatgptLogoutBtn").onclick = async () => {
  try {
    state.chatgpt = await api("/api/chatgpt/oauth/logout", {});
    renderChatGPTOAuth();
  } catch (err) {
    $("chatgptOAuthStatus").textContent = err.message;
  }
};
$("settingsBtn").onclick = () => openSettings("general");
$("keysForm").addEventListener("submit", (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  if (settingsTab !== "keys") { $("keysModal").close(); return; }
  const fd = new FormData($("keysForm"));
  const name = String(fd.get("kname") || "").trim();
  const value = String(fd.get("kvalue") || "").trim();
  if (!name || !value) { $("keysErr").textContent = t("Both name and key are required"); return; }
  api("/api/keys", { name, value })
    .then(() => {
      const kn = $("keysForm").elements.namedItem("kname");
      const kv = $("keysForm").elements.namedItem("kvalue");
      if (kn) kn.value = "";
      if (kv) kv.value = "";
      $("keysErr").textContent = t("Saved");
    })
    .catch((err) => { $("keysErr").textContent = err.message; });
});

// ---- 插件（MCP）/ Plugins (MCP) ----

// 目录条目的一句话标注：装之前需要什么、以及慢在哪。写在按钮旁边，
// 免得用户点下去以后才发现要去申请密钥、或者以为卡死了。
// One line per catalog entry saying what it needs and where it will be slow — next to the button, so
// nobody clicks first and only then discovers a key is required, or takes a long download for a hang.
function catalogNote(entry) {
  const bits = [entry.url ? t("remote") : t("local")];
  if (entry.need?.kind === "key") bits.push(t("needs a key"));
  if (entry.need?.kind === "path") bits.push(t("needs a directory"));
  if (entry.oauth) bits.push(t("opens a browser to authorize"));
  if (entry.slow) bits.push(t("first install downloads a lot"));
  return bits.join(" · ");
}

// 正在安装的条目。装一个插件可能要几十秒，这期间来一次无关的状态刷新会把界面重画，
// 记在这里才不会把"安装中…"退回成"安装"。
// Entries currently installing. Installing can take tens of seconds, and an unrelated state refresh
// repaints the panel in the meantime; tracking them here keeps "Installing…" from reverting to
// "Install".
const mcpInstalling = new Set();

function renderMCPCatalog() {
  const wrap = $("mcpCatalog");
  wrap.replaceChildren();
  const installed = new Set((state.mcp || []).map((s) => s.name));
  for (const entry of MCP_CATALOG) {
    const title = el("div", "title", entry.label);
    if (installed.has(entry.name)) title.append(el("span", "tag-done", t("Installed")));
    const busy = mcpInstalling.has(entry.name);
    wrap.append(item({
      title,
      sub: t(entry.desc) + " — " + catalogNote(entry),
      actions: installed.has(entry.name) ? [] : [[
        busy ? (entry.oauth ? t("Authorizing…") : t("Installing…")) : t("Install"),
        busy ? "busy" : "",
        (btn) => { if (!busy) installFromCatalog(entry, btn); },
      ]],
    }));
  }
}

// installFromCatalog 一路问到底再装：缺什么当场要，要到了就直接连，
// 中途任何一步取消都干净收场（不会留下半个装好的插件）。
// installFromCatalog asks for whatever is missing and then installs: anything it needs is requested on
// the spot, and cancelling at any step leaves nothing behind (never a half-installed plugin).
async function installFromCatalog(entry, btn) {
  const body = { name: entry.name };
  const need = entry.need;

  if (need?.kind === "key") {
    // 已经存过的密钥不再问一遍 / do not ask again for a key that is already saved
    const saved = (state.keys || []).some((k) => k.name === need.key);
    if (!saved) {
      const value = await ask({
        title: t("%s needs a key", entry.label),
        hint: t(need.hint),
        field: true, fieldLabel: need.key, placeholder: t("Paste the key"),
        ok: t("Save and install"),
      });
      if (value === null || !String(value).trim()) return;
      try {
        await api("/api/keys", { name: need.key, value: String(value).trim() });
      } catch (err) {
        $("mcpErr").textContent = err.message;
        return;
      }
    }
  }

  let path = "";
  if (need?.kind === "path") {
    const value = await ask({
      title: t("Which directory should %s expose?", entry.label),
      hint: t("The bots can read and write anything under it. Writes still need your approval."),
      field: true, fieldLabel: t(need.label), placeholder: need.placeholder,
      ok: t("Install"),
    });
    if (value === null || !String(value).trim()) return;
    path = String(value).trim();
  }

  if (entry.url) {
    body.url = entry.url;
    if (need?.as === "bearer") body.bearer_key = need.key;
  } else {
    body.command = entry.command;
    body.args = path ? entry.args + " " + path : entry.args;
    if (need?.as === "env") body.env = { [need.key]: "$" + need.key };
  }

  // 装的过程可能要几十秒（现下 npm 包 / 等浏览器授权），按钮就地变成进度提示，
  // 免得用户以为没反应又点一次。
  // Installing can take tens of seconds (downloading an npm package, waiting on a browser
  // authorization), so the button turns into a progress note in place rather than looking unresponsive
  // and inviting a second click.
  mcpInstalling.add(entry.name);
  if (btn) {
    btn.disabled = true;
    btn.textContent = entry.oauth ? t("Authorizing…") : t("Installing…");
  }
  $("mcpErr").textContent = entry.oauth
    ? t("A browser window will open for you to authorize %s.", entry.label)
    : t("Installing %s — the first time may take a minute while it downloads.", entry.label);
  try {
    await api("/api/mcp/add", body);
    $("mcpErr").textContent = t("%s installed. Check it for a bot under that bot's settings.", entry.label);
  } catch (err) {
    $("mcpErr").textContent = err.message;
  } finally {
    mcpInstalling.delete(entry.name);
    renderMCPCatalog();
  }
}

$("mcpBtn").onclick = () => {
  $("mcpForm").reset();
  $("mcpForm").dataset.env = "";
  $("mcpErr").textContent = "";
  $("mcpStdioRows").hidden = false;
  $("mcpHttpRows").hidden = true;
  $("mcpManual").open = false;
  $("bundleErr").textContent = "";
  renderMCPCatalog();
  renderMCPList();
  renderSkillList();
  renderBundleList();
  $("mcpModal").showModal();
};
$("mcpTypeSel").onchange = (e) => {
  const http = e.target.value === "http";
  $("mcpStdioRows").hidden = http;
  $("mcpHttpRows").hidden = !http;
};
$("mcpForm").addEventListener("submit", (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const fd = new FormData($("mcpForm"));
  const g = (k) => String(fd.get(k) || "").trim();
  const body = { name: g("mname") };
  if (g("mtype") === "http") {
    body.url = g("murl");
    body.bearer_key = g("mbearer");
  } else {
    body.command = g("mcommand");
    body.args = g("margs");
    if ($("mcpForm").dataset.env) {
      try { body.env = JSON.parse($("mcpForm").dataset.env); } catch {}
    }
  }
  $("mcpErr").textContent = t("Adding… (the first run may download the plugin package)");
  api("/api/mcp/add", body)
    .then(() => { $("mcpForm").reset(); $("mcpErr").textContent = t("Added"); })
    .catch((err) => { $("mcpErr").textContent = err.message; });
});

$("skillRescanBtn").onclick = () => {
  api("/api/skills/rescan", {})
    .then(() => toast(t("Skills rescanned")))
    .catch((err) => toast(err.message));
};

$("bundleInstallBtn").onclick = async () => {
  const src = $("bundleSrc").value.trim();
  if (!src) { $("bundleErr").textContent = t("Enter a git URL or a folder path"); return; }
  const btn = $("bundleInstallBtn");
  btn.disabled = true;
  btn.textContent = t("Installing…");
  $("bundleErr").textContent = t("Fetching the package…");
  try {
    const res = await api("/api/plugins/install", { source: src });
    // 这个地址给的是一份市场清单而不是单个插件——抽样下来约一半的真实仓库是这种形态，
    // 所以这不是错误路径，是常规路径之一。
    // The address gave a marketplace listing rather than a single plugin — about half of the sampled real
    // repositories are shaped this way, so this is one of the normal paths, not an error path.
    if (res.marketplace) {
      $("bundleErr").textContent = "";
      openMarketplacePicker(src, res.marketplace);
      return;
    }
    $("bundleSrc").value = "";
    const p = res.plugin || {};
    // 装完直接说清楚吃进了什么、漏掉了什么，别让用户自己去猜
    // Say plainly what was taken in and what was left out, rather than leaving the user to work it out
    const got = [];
    if (p.mcp_servers?.length) got.push(t("%s plugins", p.mcp_servers.length));
    if (p.skills?.length) got.push(t("%s skills", p.skills.length));
    if (p.agents?.length) got.push(t("%s teammates", p.agents.length));
    $("bundleErr").textContent = got.length
      ? t("%s installed: %s", p.name, got.join(" · "))
      : t("%s installed, but it had nothing Bot Bureau can use", p.name);
  } catch (err) {
    $("bundleErr").textContent = err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = t("Install package");
  }
};

$("toolPickSave").onclick = () => saveToolPick(false);
$("toolPickAll").onclick = () => saveToolPick(true);

// ---- 任务看板清理 / Task board cleanup ----

$("clearDoneBtn").onclick = () => {
  api("/api/tasks/clear_done", {}).catch((err) => toast(err.message));
};

// ---- 新建 / 编辑 Bot、群聊设置 / Create & edit bots, group settings ----

const fld = (form, name) => form.elements.namedItem(name);
let editingBot = "";                              // 非空表示在编辑而不是新建 / non-empty means edit, not create
const avatarDraft = { bot: "", group: "" };       // 弹窗里当前选中的头像 / avatar picked in the open dialog

// 头像编辑区：一排预设底色 + 上传的图，选中的画一圈白边
// The avatar editor: a row of preset fills plus an uploaded image; the chosen one gets a white ring
function paintAvatarEditor(which, fallbackName) {
  const spec = avatarDraft[which];
  const box = $(which === "bot" ? "botAvPreview" : "groupAvPreview");
  box.replaceChildren();
  if (which === "group" && !spec) {
    box.append(groupAvatarFor(editingGroup, 64));
  } else {
    const wrap = el("span", "av");
    wrap.style.width = wrap.style.height = "64px";
    wrap.append(faceNode(spec, fallbackName, 64));
    box.append(wrap);
  }
  const sw = $(which === "bot" ? "botSwatches" : "groupSwatches");
  sw.replaceChildren(...FACES.map((c) => {
    const b = el("button", "swatch" + (spec === c ? " on" : ""));
    b.type = "button";
    b.style.background = c;
    b.title = c;
    b.onclick = () => { avatarDraft[which] = c; paintAvatarEditor(which, fallbackName); };
    return b;
  }));
}

// 上传的图先裁成正方形再缩到 96px：头像是圆的，长图直接压会变形，原图也太大存不进配置
// Uploaded images are center-cropped square and scaled to 96px: the avatar is round, so a
// non-square source would distort, and the original is far too large to keep in the config
const AVATAR_PX = 96;
async function shrinkImage(file) {
  const dataURL = await new Promise((res, rej) => {
    const fr = new FileReader();
    fr.onload = () => res(fr.result);
    fr.onerror = () => rej(new Error(t("Could not read the image")));
    fr.readAsDataURL(file);
  });
  const img = new Image();
  await new Promise((res, rej) => {
    img.onload = res;
    img.onerror = () => rej(new Error(t("That is not a decodable image")));
    img.src = dataURL;
  });
  const side = Math.min(img.naturalWidth, img.naturalHeight);
  if (!side) throw new Error(t("The image has no dimensions"));
  const cv = document.createElement("canvas");
  cv.width = cv.height = AVATAR_PX;
  cv.getContext("2d").drawImage(
    img, (img.naturalWidth - side) / 2, (img.naturalHeight - side) / 2, side, side,
    0, 0, AVATAR_PX, AVATAR_PX);
  const webp = cv.toDataURL("image/webp", 0.85);
  return webp.startsWith("data:image/webp") ? webp : cv.toDataURL("image/jpeg", 0.85);
}

function wireAvatarPicker(which, errId, fallback) {
  const file = $(which === "bot" ? "botAvFile" : "groupAvFile");
  $(which === "bot" ? "botAvUpload" : "groupAvUpload").onclick = () => file.click();
  $(which === "bot" ? "botAvReset" : "groupAvReset").onclick = () => {
    avatarDraft[which] = "";
    paintAvatarEditor(which, fallback());
  };
  file.onchange = async () => {
    const f = file.files && file.files[0];
    file.value = "";
    if (!f) return;
    try {
      avatarDraft[which] = await shrinkImage(f);
      paintAvatarEditor(which, fallback());
      $(errId).textContent = "";
    } catch (err) {
      $(errId).textContent = err.message;
    }
  };
}
wireAvatarPicker("bot", "botErr", () => editingBot || fld($("botForm"), "name").value || "bot");
wireAvatarPicker("group", "groupErr", () => editingGroup || "group");

// ---- 连接模型：服务商 → 接入方式 → 模型 ----
//
// 三步一套表单，新建/编辑 bot 和首次引导共用。三条设计前提：
//   · 服务商目录由引擎给（/api/providers），客户端不再自带一份迟早过期的型号表；
//   · 模型名一律现拉（/api/models），拉不到就如实说，绝不编一个填上去；
//   · 订阅登录和填 key 都在这里就地做完，不用先去设置页存 key 再绕回来。
//
// ---- Connecting a model: vendor → auth → model ----
//
// One three-step form shared by the new/edit bot dialog and first-run onboarding. Three premises:
//   · the vendor catalog comes from the engine (/api/providers), so the client carries no model list to go stale;
//   · model names are always fetched live (/api/models) — on failure we say so rather than invent one;
//   · signing in or pasting a key happens right here, not via a detour through the settings dialog.

// 验证码 + 复制按钮。
//
// 设备码和配对码都是"看着它、去另一台设备上敲一遍"的东西，手抄最容易出错——
// 尤其是 5BEC-3D33 这种混了字母数字的。所以凡是要用户搬运的码，旁边就该有复制。
// 用等宽字体显示，O/0、I/1 才分得清。
//
// A code with a copy button.
//
// Device codes and pairing codes both exist to be read here and typed somewhere else, which is exactly
// where transcription goes wrong — particularly with mixed alphanumerics like 5BEC-3D33. Any code the
// user has to carry gets a copy button next to it, shown in a monospace face so O/0 and I/1 stay apart.
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // 剪贴板 API 在非安全上下文里会被拒；退回到老办法
    // The clipboard API is refused outside a secure context; fall back to the old trick
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.append(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

function codeChip(code) {
  const wrap = el("span", "code-chip");
  wrap.append(el("code", "", code));
  const btn = el("button", "text-btn", t("Copy"));
  btn.type = "button";
  btn.onclick = async (e) => {
    e.preventDefault();
    e.stopPropagation();
    const ok = await copyText(code);
    btn.textContent = ok ? t("Copied") : t("Copy failed");
    setTimeout(() => { btn.textContent = t("Copy"); }, 1600);
  };
  wrap.append(btn);
  return wrap;
}

let permLevels = [];
let effortLevels = [];

// 档位随服务商变（Anthropic 是思考预算，OpenAI 多一档 minimal，xAI 只认 low/high），
// 所以每次换服务商都要重新问引擎，不能只在启动时取一次。
//
// The tiers vary by provider (Anthropic takes a thinking budget, OpenAI has the extra minimal tier,
// xAI accepts only low and high), so the engine is asked again on every vendor change rather than
// once at startup.
async function loadEffortLevels(providerID) {
  try {
    effortLevels = (await api("/api/efforts?provider=" + encodeURIComponent(providerID || ""))).levels || [];
  } catch (err) {
    console.log("effort levels unavailable: " + err.message);
    effortLevels = [];
  }
}

// 思考强度：一个下拉 + 一行说明。放在模型下面，因为它是"这个模型怎么用"的一部分。
// Thinking effort: one dropdown and a note. It sits under the model because it is part of how that
// model gets used.
function fillBotEffortSel(value) {
  const sel = $("botEffortSel");
  sel.replaceChildren(...effortLevels.map((e) => {
    const o = document.createElement("option");
    o.value = e.id;
    o.textContent = e.label;
    return o;
  }));
  // 换了服务商后原来选的那档可能已经没有了（openai 的 medium → xai 就没有），
  // 这时退回"服务商默认"，而不是留一个下拉里根本不存在的值。
  //
  // After a vendor change the previous tier may be gone (medium on openai, absent on xai); fall back to
  // "vendor default" rather than leaving a value the dropdown does not contain.
  const keep = effortLevels.some((e) => e.id === (value || ""));
  sel.value = keep ? value || "" : "";
  renderBotEffortNote();
}

// 档位表要重新问引擎（它按服务商给），拿回来再填下拉。
// The tier table is refetched from the engine (which serves it per provider) before filling the dropdown.
async function refreshBotEffort(providerID, value) {
  await loadEffortLevels(providerID);
  fillBotEffortSel(value);
}

function renderBotEffortNote() {
  const sel = $("botEffortSel");
  const e = effortLevels.find((x) => x.id === sel.value);
  $("botEffortNote").textContent = e ? e.note : "";
}

async function loadPermLevels() {
  try {
    const r = await api("/api/permissions");
    permLevels = r.levels || [];
  } catch (err) {
    console.log("permission levels unavailable: " + err.message);
  }
}
const permOpt = (id) => permLevels.find((p) => p.id === id) || null;

// 设置里的全局档位：一档一行，选中即存。用整行按钮而不是下拉，
// 因为每一档的差别全在那句说明里，藏进下拉就等于没写。
//
// The global tier in Settings: one row per tier, saved on click. Rows rather than a dropdown, because
// the whole difference between tiers lives in the one-line description — hiding it in a dropdown is
// the same as not writing it.
function renderPermLevels() {
  const box = $("permLevels");
  if (!box) return;
  const cur = (state.settings || {}).permission || "ask";
  box.replaceChildren(...permLevels.map((p) => {
    const row = el("button", "perm-row" + (p.id === cur ? " on" : "") + (p.id === "full" ? " danger" : ""));
    row.type = "button";
    const txt = el("div", "txt");
    txt.append(el("div", "name", p.label), el("div", "desc", p.note));
    row.append(el("span", "tick"), txt);
    row.onclick = async () => {
      $("permErr").textContent = "";
      try {
        state.settings = await api("/api/settings", { permission: p.id });
        renderPermLevels();
        renderBotPermNote();
      } catch (err) {
        $("permErr").textContent = err.message;
      }
    };
    return row;
  }));
}

// bot 弹窗里的档位：多一个"跟随全局"，并把全局当前是什么写出来
// The per-bot tier adds a "follow the global setting" option and spells out what the global one currently is
function fillBotPermSel(value) {
  const sel = $("botPermSel");
  const globalID = (state.settings || {}).permission || "ask";
  const g = permOpt(globalID);
  const follow = document.createElement("option");
  follow.value = "";
  follow.textContent = t("Follow the global setting (%s)", g ? g.label : globalID);
  sel.replaceChildren(follow, ...permLevels.map((p) => {
    const o = document.createElement("option");
    o.value = p.id;
    o.textContent = p.label;
    return o;
  }));
  sel.value = value || "";
  renderBotPermNote();
}

function renderBotPermNote() {
  const note = $("botPermNote");
  if (!note) return;
  const sel = $("botPermSel");
  const id = sel.value || (state.settings || {}).permission || "ask";
  const p = permOpt(id);
  note.textContent = p ? p.note : "";
  note.classList.toggle("err", id === "full");
}

let providerCatalog = [];

async function loadProviderCatalog() {
  try {
    providerCatalog = (await api("/api/providers")).providers || [];
  } catch (err) {
    console.log("provider catalog unavailable: " + err.message);
  }
}
const providerOpt = (id) => providerCatalog.find((p) => p.id === id) || null;

function authLabel(mode) {
  return {
    chatgpt: t("ChatGPT subscription"),
    xai: t("SuperGrok subscription"),
    key: "API Key",
    none: t("No credential"),
  }[mode] || mode;
}

const subState = (mode) => (mode === "chatgpt" ? state.chatgpt : mode === "xai" ? state.xai : null) || {};
const subConnected = (mode) => !!subState(mode).connected;
function xaiConnected() { return subConnected("xai"); }
function chatgptConnected() { return subConnected("chatgpt"); }

// 设备码登录：开浏览器 + 轮询到出结果。两处界面共用，所以状态通过回调抛出去。
// Device-code login: open the browser and poll until it settles. Shared by two surfaces, so it reports via a callback.
async function deviceLogin(kind, onUpdate) {
  const start = kind === "chatgpt" ? "/api/chatgpt/oauth/start" : "/api/xai/oauth/start";
  const statusPath = kind === "chatgpt" ? "/api/chatgpt/oauth/status" : "/api/xai/oauth/status";
  state[kind] = await api(start, {});
  onUpdate();
  if (state[kind] && state[kind].url) window.open(state[kind].url, "_blank", "noopener");
  const timer = setInterval(async () => {
    try {
      state[kind] = await api(statusPath);
      onUpdate();
      if (!state[kind].pending) clearInterval(timer);
    } catch { /* 下一拍再试 / try again on the next tick */ }
  }, 1500);
  return timer;
}

function field(labelText, control) {
  const l = document.createElement("label");
  l.append(el("span", "", labelText), control);
  return l;
}

// 返回一个挂在 root 上的选择器：setConfig 回填、getConfig 取值。
// Returns a picker mounted on root: setConfig repopulates it, getConfig reads it back.
// onVendor（可选）在用户换服务商时回调。思考强度的档位是跟着服务商走的，
// 选择器自己不管那个，只负责把变化说出去。
//
// onVendor (optional) fires when the user changes vendor. The reasoning-effort tiers follow the
// provider; the picker does not own them and only announces the change.
function createModelPicker(root, onVendor) {
  let cur = { provider_id: "", auth: "", base_url: "", api_key_env: "", model: "" };
  let models = [];
  let manual = false;
  let loading = false;
  let modelErr = "";
  let pollTimer = null;

  const provSel = document.createElement("select");
  const noteEl = el("p", "hint mp-note");
  const authRow = el("div", "seg");
  const authWrap = field(t("How to connect"), authRow);
  const subBox = el("div", "mp-sub");
  const keyInput = document.createElement("input");
  keyInput.type = "password";
  keyInput.placeholder = t("Paste the API key");
  const keyWrap = field("API Key", keyInput);
  const baseInput = document.createElement("input");
  const baseWrap = field("Base URL", baseInput);
  const keyEnvInput = document.createElement("input");
  const keyEnvWrap = field(t("API key name"), keyEnvInput);
  const modelSel = document.createElement("select");
  const modelInput = document.createElement("input");
  modelInput.placeholder = t("Model name");
  const refreshBtn = el("button", "mp-refresh", t("Refresh"));
  refreshBtn.type = "button";
  const modelBox = el("div", "mp-model");
  modelBox.append(modelSel, modelInput, refreshBtn);
  const modelWrap = field(t("Model"), modelBox);
  const modelNote = el("p", "hint mp-note");

  root.replaceChildren(field(t("Vendor"), provSel), noteEl, authWrap, subBox,
    keyWrap, baseWrap, keyEnvWrap, modelWrap, modelNote);

  const opt = () => providerOpt(cur.provider_id);

  provSel.onchange = () => {
    const p = providerOpt(provSel.value);
    cur.provider_id = provSel.value;
    cur.auth = p ? p.auth[0] : "key";
    cur.base_url = p ? p.base_url : "";
    cur.api_key_env = p ? p.key_env : "";
    cur.model = "";
    models = [];
    manual = false;
    modelErr = "";
    paint();
    refresh();
    if (onVendor) onVendor(cur.provider_id);
  };
  refreshBtn.onclick = () => refresh();
  modelSel.onchange = () => { cur.model = modelSel.value; };
  modelInput.oninput = () => { cur.model = modelInput.value.trim(); };
  baseInput.oninput = () => { cur.base_url = baseInput.value.trim(); };
  keyEnvInput.oninput = () => { cur.api_key_env = keyEnvInput.value.trim(); };

  function paint() {
    const p = opt();
    provSel.replaceChildren(...providerCatalog.map((x) => {
      const o = document.createElement("option");
      o.value = x.id;
      o.textContent = x.label;
      return o;
    }));
    if (p) provSel.value = p.id;
    noteEl.textContent = (p && p.note) || "";
    noteEl.hidden = !noteEl.textContent;

    const modes = (p && p.auth) || ["key"];
    authWrap.hidden = modes.length < 2;
    authRow.replaceChildren(...modes.map((m) => {
      const b = el("button", "seg-btn" + (m === cur.auth ? " on" : ""), authLabel(m));
      b.type = "button";
      b.onclick = () => { cur.auth = m; models = []; modelErr = ""; paint(); refresh(); };
      return b;
    }));

    const isSub = cur.auth === "chatgpt" || cur.auth === "xai";
    subBox.hidden = !isSub;
    if (isSub) paintSub();

    keyWrap.hidden = cur.auth !== "key";
    if (cur.auth === "key") {
      const have = (state.keys || []).some((k) => k.name === cur.api_key_env);
      keyInput.placeholder = have
        ? t("%s is saved; leave blank to keep it", cur.api_key_env)
        : t("Paste %s", cur.api_key_env);
    }
    baseWrap.hidden = !(p && p.custom);
    keyEnvWrap.hidden = !(p && p.custom);
    baseInput.value = cur.base_url || "";
    keyEnvInput.value = cur.api_key_env || "";

    modelWrap.hidden = !!(p && p.provider === "fake");
    modelSel.hidden = manual;
    modelInput.hidden = !manual;
    refreshBtn.disabled = loading;
    refreshBtn.textContent = loading ? t("Loading…") : t("Refresh");
    modelSel.replaceChildren(...models.map((m) => {
      const o = document.createElement("option");
      o.value = m;
      o.textContent = m;
      return o;
    }));
    // 空下拉不能是个哑巴：写清楚现在缺什么，用户才知道下一步该干嘛
    // An empty dropdown must not go silent: say what is missing so the next step is obvious
    if (!models.length && !cur.model) {
      const o = document.createElement("option");
      o.value = "";
      o.disabled = true;
      o.selected = true;
      o.textContent = loading
        ? t("Loading…")
        : cur.auth === "key"
          ? t("Enter the key above, then Refresh")
          : t("Sign in above, then Refresh");
      modelSel.append(o);
    }
    if (cur.model && !models.includes(cur.model) && !manual) {
      const o = document.createElement("option");
      o.value = o.textContent = cur.model;
      modelSel.prepend(o);
    }
    if (cur.model) modelSel.value = cur.model;
    else if (models.length) cur.model = modelSel.value = models[0];
    modelInput.value = cur.model || "";

    modelNote.replaceChildren();
    modelNote.hidden = modelWrap.hidden;
    if (!modelWrap.hidden) {
      if (modelErr) modelNote.append(document.createTextNode(modelErr + " "));
      const toggle = el("button", "text-btn", manual ? t("Pick from the list") : t("Type it in"));
      toggle.type = "button";
      toggle.onclick = () => { manual = !manual; paint(); };
      modelNote.append(toggle);
    }
  }

  function paintSub() {
    const kind = cur.auth;
    const s = subState(kind);
    subBox.replaceChildren();
    const dot = el("span", "status-dot");
    dot.style.background = s.connected ? "var(--ok)" : s.pending ? "var(--warn)" : "var(--text-3)";
    let text = t("Not signed in");
    if (s.connected) text = t("Signed in");
    else if (s.pending) text = "";
    else if (s.status === "error") text = s.error || t("Sign-in failed");
    const textEl = el("span", "mp-sub-text", text);
    if (s.pending && s.user_code) {
      textEl.replaceChildren(document.createTextNode(t("Enter this code in your browser:") + " "), codeChip(s.user_code));
    }
    subBox.append(dot, textEl);
    const btn = el("button", "", s.connected ? t("Sign out") : t("Sign in"));
    btn.type = "button";
    btn.onclick = async () => {
      try {
        if (s.connected) {
          state[kind] = await api(kind === "chatgpt" ? "/api/chatgpt/oauth/logout" : "/api/xai/oauth/logout", {});
          paint();
          return;
        }
        clearInterval(pollTimer);
        pollTimer = await deviceLogin(kind, () => {
          paint();
          // 登录成功的那一刻立刻去拉模型，用户不用再点一次刷新
          // The moment sign-in lands, fetch models so the user need not hit refresh
          if (subConnected(kind) && !models.length) refresh();
        });
      } catch (err) {
        modelErr = err.message;
        paint();
      }
    };
    subBox.append(btn);
  }

  function pendingKey() {
    const v = keyInput.value.trim();
    return v && cur.auth === "key" ? { name: cur.api_key_env, value: v } : null;
  }

  async function refresh() {
    const p = opt();
    if (!p || p.provider === "fake") { models = []; modelErr = ""; paint(); return; }
    loading = true;
    modelErr = "";
    paint();
    try {
      // 先把刚敲进来的 key 存了再问。
      //
      // 列表是引擎拿凭据去服务商那儿现拉的，而这里只发 key 的名字——用户刚输入的值还躺在
      // 输入框里没进钥匙串，引擎当然找不到，于是"填完 key 点刷新，什么都不出来"。
      // 输入了 key 又点刷新，意思就是"用这个 key 去查"，存下来正是他要的。
      //
      // Save the freshly typed key before asking.
      //
      // The list is fetched live by the engine using a credential, and only the key's *name* travels
      // in this request — a value just typed still sits in the field, unknown to the key store, so the
      // engine finds nothing and "enter a key, hit refresh, nothing appears" is the result. Typing a
      // key and then asking for the list means "look it up with this key", so storing it is the intent.
      const pending = pendingKey();
      if (pending) {
        await api("/api/keys", pending);
        keyInput.value = "";
        if (typeof refetchState === "function") await refetchState().catch(() => {});
      }
      const r = await api("/api/models", {
        provider_id: cur.provider_id, base_url: cur.base_url,
        key_env: cur.api_key_env, auth: cur.auth,
      });
      models = r.models || [];
      modelErr = r.error || "";
      // 拉不到不自动切手填。
      //
      // 选模型本来就该是"从这家有什么里面挑"，手打型号名是退路不是常态——自动切过去
      // 等于把"我暂时问不到"变成了"请你自己想一个"，而多数人并不知道该填什么。
      // 拉不到就让下拉框里明说原因，刷新键还在那儿；真想手打，点一下就切。
      //
      // A failed fetch does not switch to manual entry.
      //
      // Choosing a model means picking from what the vendor actually offers; typing an id is the
      // fallback, not the norm. Switching automatically turns "I could not ask just now" into "think
      // of one yourself", which most people cannot. On failure the dropdown says why, the refresh
      // button is still there, and typing is one click away for anyone who wants it.
      if (models.length && (!cur.model || !models.includes(cur.model))) cur.model = models[0];
    } catch (err) {
      models = [];
      modelErr = err.message;
    }
    loading = false;
    paint();
  }

  return {
    setConfig(c) {
      clearInterval(pollTimer);
      // 先把服务商定下来，其余字段全部从它派生——分开兜底会让 id 和 key 名对不上
      // Settle the vendor first and derive everything from it; independent fallbacks let the id and key name drift apart
      const p = providerOpt(c.provider_id) || guessProvider(c) || providerCatalog[0] || null;
      cur = {
        provider_id: p ? p.id : "",
        auth: c.auth || (p ? p.auth[0] : "key"),
        base_url: c.base_url || (p ? p.base_url : ""),
        api_key_env: c.api_key_env || (p ? p.key_env : ""),
        model: c.model || "",
      };
      models = [];
      manual = false;
      modelErr = "";
      loading = false;
      paint();
      refresh();
    },
    getConfig() {
      const p = opt();
      return {
        provider: p ? p.provider : "",
        provider_id: cur.provider_id,
        auth: cur.auth,
        model: cur.model,
        base_url: p && p.provider === "fake" ? "" : cur.base_url,
        api_key_env: p && p.provider === "fake" ? "" : cur.api_key_env,
      };
    },
    // 用户就地填的 key：交给调用方在保存前先存进钥匙串
    // A key typed in place: the caller saves it to the key store before saving the bot
    pendingKey,
    clearKeyInput() { keyInput.value = ""; },
    repaint: paint,
  };
}

// 老配置里没有 provider_id，按 base_url 反推一次，让编辑弹窗能正确回填。
// Pre-existing configs have no provider_id; infer one from the base URL so the edit dialog repopulates correctly.
function guessProvider(c) {
  const prov = (c.provider || "").toLowerCase();
  if (!prov) return null;
  if (prov === "fake") return providerOpt("fake");
  const base = (c.base_url || "").toLowerCase();
  if (prov === "anthropic") return providerOpt("anthropic");
  for (const p of providerCatalog) {
    if (p.custom || !p.base_url) continue;
    const host = p.base_url.replace(/^https?:\/\//, "").split("/")[0].toLowerCase();
    if (host && base.includes(host)) return p;
  }
  return base ? providerOpt("custom") : providerOpt("openai");
}

const botPicker = createModelPicker($("botModelPicker"), (pid) => refreshBotEffort(pid, ""));

// 侧栏只留一个"＋"：建 bot 和建群都从这里进。
// 两个并排的加号分不清谁是谁，图标又几乎一样——点开给两个带说明的选项更清楚。
//
// One "＋" in the sidebar covers both creating a bot and creating a group.
// Two adjacent plus buttons with near-identical icons say nothing about which is which; one menu
// with two labelled options does.
$("botForm").elements.namedItem("display_name").addEventListener("input", paintBotID);

$("newBtn").onclick = (e) => {
  e.stopPropagation();
  if (pop) { closePop(); return; }
  pop = el("div", "pop");
  const entry = (iconPath, label, desc, fn) => {
    const b = el("button", "pop-wide");
    b.type = "button";
    const svg = document.createElementNS(NS_SVG, "svg");
    svg.setAttribute("width", "17"); svg.setAttribute("height", "17");
    svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.6");
    svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round");
    const path = document.createElementNS(NS_SVG, "path");
    path.setAttribute("d", iconPath);
    svg.append(path);
    const txt = el("div", "");
    txt.append(el("div", "name", label), el("div", "desc", desc));
    b.append(svg, txt);
    b.onclick = () => { closePop(); fn(); };
    return b;
  };
  pop.append(
    entry(ICON_PLUS, t("New bot"), t("Hire a teammate with its own model and workspace"), () => openBotModal("")),
    entry(ICON_PEOPLE, t("New team"), t("Put several bots on one piece of work together"), () => createGroup()),
  );
  document.body.append(pop);
  const r = $("newBtn").getBoundingClientRect();
  pop.style.left = r.left - 6 + "px";
  pop.style.top = r.bottom + 6 + "px";
};

// 名字和 @提及 id 合成一个字段。
//
// 以前是两个输入框（"显示名"和"@提及 id"），第一次建 bot 的人得先弄明白它俩什么关系
// 才能填第一个字。现在只填名字，id 自动派生并在下面一行小字里显示出来——绝大多数情况
// 不用管它，想改再点"改"。id 仍然是那把稳定的钥匙（工作目录、群成员、任务归属都认它），
// 只是不再要求用户先想清楚这件事。
//
// The name and the @mention id collapse into one field.
//
// There used to be two inputs — "display name" and "@mention id" — so anyone creating their first bot
// had to work out how the two relate before typing anything. Now only the name is asked for; the id is
// derived and shown on a small line underneath, ignorable in almost every case and editable behind a
// "change" link. The id is still the stable key (workspaces, membership and task ownership all use it);
// the user simply is not made to think about that first.
function slugify(name) {
  const s = (name || "").toLowerCase().trim()
    .replace(/[\s_]+/g, "-")
    .replace(/[^a-z0-9-]/g, "")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
  return s.slice(0, 24);
}

// 名字里一个 ASCII 字符都没有（比如全中文）时派生不出 id，退到 bot-2、bot-3…
// With no ASCII in the name (all-CJK, say) nothing can be derived, so fall back to bot-2, bot-3, …
// 内部 id = 名字的 slug + 五位随机串。
//
// 随机那五位是必需的，不是装饰：slug 只留 a-z0-9-，所以"吴敏""催命大师"这类名字整个被删空，
// 全都落到同一个兜底值上，第二位同事起就成了 bot-2、bot-3——一串既不好认、又跟名字毫无关系的
// 序号。id 现在不露在界面上，唯一性比可读性重要得多，随机串两头都占：拉丁名字仍然看得出是谁
// （wren-k3f9a），中文名字至少各不相同。
//
// The internal id is the name's slug plus five random characters.
//
// Those five are load-bearing rather than decorative: the slug keeps only a-z0-9-, so a name written
// in Chinese empties out entirely and every such bot lands on the same fallback — bot-2, bot-3 from
// the second teammate on, a sequence that is neither recognisable nor related to the name. The id no
// longer surfaces in the UI, where uniqueness matters far more than readability, and a random suffix
// gets both: a Latin name stays recognisable (wren-k3f9a) and a Chinese one is at least distinct.
function randomSuffix(n) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
  const bytes = new Uint8Array(n);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join("");
}

function freeBotID(base) {
  const taken = new Set(state.bots.map((b) => b.name));
  // 前缀留 18 位，加上分隔符和五位随机串正好压在 id 的 24 字符上限内
  // The prefix is capped at 18 so the separator and five random characters stay inside the 24-character id limit
  const prefix = (base || "bot").slice(0, 18);
  for (let i = 0; i < 20; i++) {
    const id = prefix + "-" + randomSuffix(5);
    if (!taken.has(id)) return id;
  }
  return prefix + "-" + randomSuffix(5);
}

// 新建时从名字派生一个内部 id，编辑时保持原样。全程不出现在界面上：
// 用户只该记住一个名字，群里喊的就是它——引擎那边 id 和显示名两条都能点到人。
//
// Derive an internal id from the name when creating, and leave it alone when editing. It never
// surfaces in the UI: the user should have to remember one name, and that is the one called in a
// group — the engine matches both the id and the display name.
function paintBotID() {
  if (editingBot) return; // id 是工作目录的键，创建后不能改 / the id keys the workspace; fixed after creation
  const form = $("botForm");
  fld(form, "name").value = freeBotID(slugify(fld(form, "display_name").value));
}

// openBotModal 的 prefill 用于「从插件包导入一位同事」：新建表单先填好，再交给用户改。
// openBotModal's prefill serves "import a teammate from a plugin package": the new-bot form arrives
// filled in, and the user takes it from there.
function openBotModal(name, prefill) {
  const form = $("botForm");
  form.reset();
  $("botErr").textContent = "";
  editingBot = name || "";
  const c = name ? cfgOf(name) : (prefill || {});
  $("botModalTitle").textContent = name
    ? t("Edit %s", titleOf(name))
    : t("New Bot");
  $("botSubmit").textContent = name ? t("Save") : t("Create");
  $("botEditNote").hidden = !name;
  fld(form, "name").value = c.name || "";
  // 编辑时名字框里放当前显示名，没设过显示名就放 id——用户看到的就是列表里那个名字
  // While editing, the name field carries the current display name, or the id when none was set —
  // whatever the user sees in the list
  fld(form, "display_name").value = c.display_name || c.name || "";
  paintBotID();
  fld(form, "role").value = c.role || "";
  fld(form, "description").value = c.description || "";
  fld(form, "prompt").value = c.prompt || "";
  // 导入来的角色说明默认展开：用户该先看清楚自己要加的是什么，再点创建
  // Imported role instructions start expanded: the user should see exactly what they are adding before
  // hitting create
  $("botPromptRow").open = !!c.prompt;
  botPicker.clearKeyInput();
  botPicker.setConfig(c);
  fillBotPermSel(c.permission || "");
  refreshBotEffort(c.provider_id || "", c.effort || "");
  renderBotMcpChoices(c.mcp || []);
  avatarDraft.bot = c.avatar || "";
  paintAvatarEditor("bot", name || "bot");
  $("botModal").showModal();
}

$("botForm").addEventListener("submit", async (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const form = $("botForm");
  const cfg = { avatar: avatarDraft.bot, ...botPicker.getConfig() };
  // 空字段也要一起提交：编辑时清空某项就该真的清掉，不能被 "只传非空" 悄悄保留
  // Empty fields are submitted too: clearing one while editing must actually clear it
  for (const key of ["name", "display_name", "role", "description", "prompt"]) {
    cfg[key] = fld(form, key).value.trim();
  }
  if (!editingBot) {
    if (!cfg.display_name && !cfg.name) {
      $("botErr").textContent = t("Give it a name first");
      return;
    }
    if (!cfg.name) cfg.name = freeBotID(slugify(cfg.display_name));
  }
  // 名字和 id 一样时不必单独存显示名：列表本来就显示 id
  // No separate display name when it matches the id: the list shows the id anyway
  if (cfg.display_name === cfg.name) cfg.display_name = "";
  cfg.permission = $("botPermSel").value;
  cfg.effort = $("botEffortSel").value; // 空串 = 跟随全局 / empty means follow the global setting
  cfg.mcp = [...$("botMcp").querySelectorAll("input[data-mcp]:checked")].map((cb) => cb.value);
  const editing = !!editingBot;
  if (editing) cfg.name = editingBot;
  try {
    const pending = botPicker.pendingKey();
    if (pending) {
      await api("/api/keys", pending);
      botPicker.clearKeyInput();
    }
    await api(editing ? "/api/bots/update" : "/api/bots", cfg);
    $("botModal").close();
  } catch (err) {
    $("botErr").textContent = err.message;
  }
});

let editingGroup = "group";
let groupMemberDraft = {};

function openGroupModal(id) {
  editingGroup = id || "group";
  const isDefault = editingGroup === "group";
  const g = isDefault
    ? { title: settingsOf().group_title || "", avatar: settingsOf().group_avatar || "", members: state.group_members || [] }
    : (groupOf(editingGroup) || { title: "", avatar: "", members: [] });
  const form = $("groupForm");
  form.reset();
  $("groupErr").textContent = "";
  $("groupDeleteBtn").hidden = isDefault;
  fld(form, "group_title").value = g.title || "";
  avatarDraft.group = g.avatar || "";
  paintAvatarEditor("group", editingGroup);
  groupMemberDraft[editingGroup] = {};
  for (const n of g.members) groupMemberDraft[editingGroup][n] = true;
  renderGroupMembers(editingGroup);
  $("groupModal").showModal();
}
$("groupForm").addEventListener("submit", async (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const title = fld($("groupForm"), "group_title").value.trim();
  const avatar = avatarDraft.group;
  const members = state.bots.map((b) => b.name).filter((n) => groupMemberDraft[editingGroup][n]);
  try {
    if (editingGroup === "group") {
      await api("/api/settings", { group_title: title, group_avatar: avatar });
      for (const b of state.bots) {
        await api("/api/group/set", { group: "group", name: b.name, in: !!groupMemberDraft[editingGroup][b.name] });
      }
    } else {
      await api("/api/groups/update", { id: editingGroup, title, avatar, members });
    }
    $("groupModal").close();
  } catch (err) {
    $("groupErr").textContent = err.message;
  }
});
$("groupDeleteBtn").onclick = async () => {
  const ok = await ask({
    title: t("Delete this group"),
    hint: t("Messages in this group are kept."),
    ok: t("Delete"),
    danger: true,
  });
  if (!ok) return;
  try {
    await api("/api/groups/delete", { id: editingGroup });
    $("groupModal").close();
    if (current === editingGroup) switchChat("group");
  } catch (err) {
    $("groupErr").textContent = err.message;
  }
};

// 建群：先落一个空群拿到 id，再直接打开它的设置让用户起名、拉人。
// 比先弹表单再创建少一步，中途放弃的空群也能在设置里直接删掉。
//
// Creating a group: persist an empty one to get its id, then open its settings so the user can name it
// and add members. One step fewer than a create-form first, and an abandoned empty group can be
// deleted right there in the same dialog.
async function createGroup() {
  const g = await api("/api/groups", { title: "", avatar: "", members: [] }).catch((err) => toast(err.message));
  if (g && g.id) openGroupModal(g.id);
}

// ---- 配对码（连接远程引擎时输入一次）/ Pairing code (entered once when connecting to a remote engine) ----

$("tokenForm").addEventListener("submit", (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const v = String(new FormData($("tokenForm")).get("tvalue") || "").trim();
  if (!v) return;
  TOKEN = v;
  localStorage.setItem("botbureau_token:" + BACKEND, v);
  $("tokenModal").close();
  boot();
});

// ---- 启动 / Boot ----

// 语言切换：立即刷新界面，并把偏好同步给引擎（影响后端消息与 bot 提示词语言）
// Language switch: refresh the UI immediately and sync the preference to the engine
// (it drives backend messages and the bots' prompt language)
$("themeSel").onchange = (e) => setThemePref(e.target.value);
$("langSel").onchange = async (e) => {
  const pref = e.target.value;
  setLocalePref(pref);
  applyStatic();
  renderAll();
  renderTG();
  renderPairInfo();
  try {
    await api("/api/settings", { locale: pref });
    // 服务商目录的标签和说明由引擎按当前语言生成，切完语言要重拉一遍
    // The catalog's labels and notes are produced by the engine in its locale, so refetch after switching
    await loadProviderCatalog();
    await loadPermLevels();
    renderPermLevels();
    botPicker.repaint();
    if (onboardPicker) onboardPicker.repaint();
  } catch (err) {
    console.log("sync locale to engine failed: " + err.message);
  }
};

// ---- 首次引导：先讲清楚"你是老板"，再带一遍能干什么，最后请第一位同事 ----
//
// 新装打开是空的：没有 bot，没有群。空界面本身讲不出这个产品是什么，所以引导要先立人称
// ——用户是事务所的老板，bot 是雇来的同事——再逐项指出创建和管理的入口。
// 最后一步才落到"建第一个 bot"，并且可以跳过：不该逼着还没想好的人先配一个模型。
//
// ---- First-run onboarding: establish "you are the boss", tour what is here, then hire the first teammate ----
//
// A fresh install opens empty: no bots, no groups. An empty window cannot explain what the product is,
// so onboarding first fixes the frame — the user runs the bureau, the bots are the staff they hire —
// and then points at where things are created and managed. Only the last step lands on "create your
// first bot", and it can be skipped: nobody undecided should be forced to configure a model first.

let onboardStep = 0;
let onboardPicker = null;

// 每步一张示意图。用现成的头像/图标语汇画，界面上认得出对应的东西在哪。
// One drawing per step, built from the avatar and icon vocabulary already on screen so the user can
// recognize what it points at.
function onboardArt(kind) {
  const wrap = el("div", "");
  wrap.style.display = "flex";
  wrap.style.alignItems = "center";
  wrap.style.gap = "10px";
  if (kind === "boss") {
    for (const n of ["chief", "scout", "coder"]) wrap.append(avatar(n, 46));
  } else if (kind === "tour") {
    wrap.append(avatar("one", 40), groupAvatarFor("group", 46), avatar("two", 40));
  } else {
    wrap.append(avatar("newcomer", 62));
  }
  return wrap;
}

function tourRow(iconPath, name, desc) {
  const row = el("div", "row");
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("class", "glyph");
  svg.setAttribute("width", "17"); svg.setAttribute("height", "17");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.6");
  svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round");
  const path = document.createElementNS(NS_SVG, "path");
  path.setAttribute("d", iconPath);
  svg.append(path);
  const txt = el("div", "");
  txt.append(el("div", "name", name), el("div", "desc", desc));
  row.append(svg, txt);
  return row;
}

const ICON_PLUS = "M12 5v14M5 12h14";
const ICON_PEOPLE = "M16 19v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 17.5V19M10 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Zm10 8v-1.5a3.5 3.5 0 0 0-2.6-3.4M15.5 4.2a3.5 3.5 0 0 1 0 6.6";
const ICON_GEAR = "M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4Zm7.4-.2a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.8 1.2V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-2.8-1.2l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.6 1.6 0 0 0 3.1 14H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.2-2.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.6 1.6 0 0 0 10 3.1V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 2.8 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0 1.2 2.8H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1Z";

function onboardSteps() {
  return [
    {
      art: "boss",
      title: t("You run this Bot Bureau"),
      lede: t("The bots are your staff: each one keeps its own workspace, its own memory and its own model, and they work while you are away. You decide who to hire and what they are allowed to do on their own."),
    },
    {
      art: "tour",
      title: t("What you can do here"),
      lede: t("Everything below is reachable at any time — nothing here is one-way."),
      rows: [
        [ICON_PLUS, t("Create a bot"), t("The ＋ at the top of the sidebar. Give it a role and a persona, pick a model, and it joins the team.")],
        [ICON_PEOPLE, t("Build a team"), t("Put several bots on one piece of work. Call a name and that one takes it; the rest read along as context.")],
        [ICON_GEAR, t("Manage them"), t("Hover a conversation and click the pencil to change a bot's name, avatar, model or permissions — a group's too.")],
      ],
    },
    {
      art: "hire",
      title: t("Hire your first teammate"),
      lede: t("Pick a vendor and a model, and this bot is ready to work. You can skip and do it later — the ＋ in the sidebar is always there."),
      hire: true,
    },
  ];
}

function paintOnboard() {
  const steps = onboardSteps();
  const step = steps[onboardStep];
  $("onboardTitle").textContent = step.title;
  $("onboardLede").textContent = step.lede;
  $("onboardArt").replaceChildren(onboardArt(step.art));
  $("onboardErr").textContent = "";

  const body = $("onboardBody");
  body.replaceChildren();
  if (step.rows) {
    const tour = el("div", "onboard-tour");
    for (const [icon, name, desc] of step.rows) tour.append(tourRow(icon, name, desc));
    body.append(tour);
  }
  if (step.hire) {
    const holder = el("div", "");
    holder.style.textAlign = "left";
    body.append(holder);
    onboardPicker = createModelPicker(holder);
    onboardPicker.setConfig({});
  }

  $("onboardDots").replaceChildren(...steps.map((_, i) => {
    const d = document.createElement("i");
    if (i === onboardStep) d.className = "on";
    return d;
  }));
  $("onboardBack").hidden = onboardStep === 0;
  $("onboardNext").textContent = step.hire ? t("Create") : t("Next");
  $("onboardSkip").textContent = step.hire ? t("Skip for now") : t("Skip");
}

function maybeOnboard() {
  if (localStorage.getItem("botbureau_onboarded") === "1") return;
  // 已经有 bot 了说明不是全新安装（升级上来的），不打扰
  // Existing bots mean this is not a fresh install (an upgrade), so stay out of the way
  if (state.bots.length) {
    localStorage.setItem("botbureau_onboarded", "1");
    return;
  }
  onboardStep = 0;
  paintOnboard();
  $("onboardModal").showModal();
}

function finishOnboard() {
  localStorage.setItem("botbureau_onboarded", "1");
  $("onboardModal").close();
}

$("onboardSkip").onclick = finishOnboard;
$("onboardBack").onclick = () => { if (onboardStep > 0) { onboardStep--; paintOnboard(); } };
$("onboardNext").onclick = async () => {
  const steps = onboardSteps();
  if (!steps[onboardStep].hire) {
    onboardStep++;
    paintOnboard();
    return;
  }
  const picked = onboardPicker.getConfig();
  if (!picked.provider) {
    $("onboardErr").textContent = t("Pick a vendor first");
    return;
  }
  if (picked.provider !== "fake" && !picked.model) {
    $("onboardErr").textContent = t("No model selected yet");
    return;
  }
  try {
    const pending = onboardPicker.pendingKey();
    if (pending) {
      await api("/api/keys", pending);
      onboardPicker.clearKeyInput();
    }
    // 第一位同事就叫 assistant：什么都能干，用户之后自己改名字和人设
    // The first teammate is simply "assistant": a generalist the user can rename and reshape later
    await api("/api/bots", {
      name: "assistant",
      role: t("Assistant"),
      description: t("A generalist teammate: research, writing, code and chores."),
      ...picked,
    });
    finishOnboard();
  } catch (err) {
    $("onboardErr").textContent = err.message;
  }
};

// 外观：跟随系统 / 浅色 / 深色。显式选择写在 <html data-theme> 上，压过 prefers-color-scheme。
// 存本地而不是引擎：同一个引擎可能被好几台设备连着，屏幕亮度是每台设备自己的事。
//
// Appearance: follow the system, light, or dark. An explicit choice is stamped on <html data-theme>
// and beats prefers-color-scheme. It lives in localStorage rather than the engine: several devices
// may share one engine, and how bright a screen is stays that device's own business.
const THEME_KEY = "botbureau_theme";

function themePref() {
  const v = localStorage.getItem(THEME_KEY);
  return v === "light" || v === "dark" ? v : "auto";
}

function applyTheme() {
  const pref = themePref();
  if (pref === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", pref);
}

function setThemePref(pref) {
  if (pref === "auto") localStorage.removeItem(THEME_KEY);
  else localStorage.setItem(THEME_KEY, pref);
  applyTheme();
}

// 局域网上发现别的设备：在设置按钮正上方冒一条提示，而不是拦在启动路上。
//
// 多设备是个惊喜功能，不是入场券——一打开就弹窗逼人选、选完再要配对码，只会让"我就想用本机"
// 的人做两道题。所以这里只提示，点了才走配对；关掉就记住，同一台设备不再烦第二次。
//
// Another device on the LAN: a hint sprouts directly above the settings button rather than standing
// in the way of startup.
//
// Multi-device is a bonus, not a turnstile. A dialog on open followed by a pairing-code prompt makes
// anyone who just wants the local setup answer two questions first. This only offers; pairing happens
// on click. Dismissing it is remembered, so the same device never asks twice.
const PAIR_DISMISSED = "botbureau_pair_dismissed";

function dismissedEngines() {
  try {
    return JSON.parse(localStorage.getItem(PAIR_DISMISSED) || "[]");
  } catch {
    return [];
  }
}

function showPairHint(engines) {
  const done = dismissedEngines();
  const e = (engines || []).find((x) => x && x.url && !done.includes(x.url));
  if (!e) return;
  const box = $("pairHint");
  box.replaceChildren();
  box.hidden = false;

  const head = el("div", "pair-head");
  head.append(el("span", "dot"), el("span", "", t("Found %s on your network", e.name || e.url)));
  const desc = el("div", "pair-desc", t("Pair with it to share one team across devices — bots, chats and approvals all stay on that machine."));
  const acts = el("div", "pair-acts");

  const yes = el("button", "primary", t("Pair"));
  yes.type = "button";
  yes.onclick = async () => {
    if (!window.botBureauNative) return;
    box.hidden = true;
    // 走和设置里同一条连接路径：连上之后主进程会重载窗口，第一次会问配对码
    // Same connect path as Settings: the main process reloads the window on success, and the first
    // time asks for the pairing code
    const r = await window.botBureauNative.connectTo(e.url);
    if (r && !r.ok) {
      toast(r.error);
      box.hidden = false;
    }
  };

  const no = el("button", "", t("Not now"));
  no.type = "button";
  no.onclick = () => {
    box.hidden = true;
    localStorage.setItem(PAIR_DISMISSED, JSON.stringify([...done, e.url]));
  };

  acts.append(yes, no);
  box.append(head, desc, acts);
}

function boot() {
  if (!/Mac/i.test(navigator.userAgent)) document.documentElement.classList.add("win-controls");
  applyTheme();
  applyStatic();
  if (window.botBureauNative && window.botBureauNative.onEnginesFound) {
    window.botBureauNative.onEnginesFound(showPairHint);
  }
  $("langSel").value = localePref();
  $("themeSel").value = themePref();
  wireSideSplit();
  // 服务商目录要先到位，模型选择器才画得出下拉框
  // The vendor catalog has to land first or the model picker has nothing to render
  Promise.all([loadProviderCatalog(), loadPermLevels()])
    .then(refetchState)
    .then(() => {
      connectSSE();
      maybeOnboard();
      console.log(`renderer ready: bots=${state.bots.length} default=${state.default_bot} remote=${REMOTE}`);
    })
    .catch((err) => {
      // 只有连远程引擎时，"输配对码"才是一个用户能执行的动作。
      // 本机引擎的令牌是主进程从 data/token 直接读出来递给渲染器的，这里再回 401
      // 说明是内部状态出了问题（比如同一个数据目录上跑了两个引擎），不是用户少输了什么——
      // 对着自己的机器要配对码，只会让人一头雾水地去翻设置里那串码。
      // Entering a pairing code is only something the user can act on when the engine is remote.
      // For the local engine the token is read straight out of data/token by the main process and handed
      // to the renderer, so a 401 here means the internal state is wrong (two engines sharing one data
      // directory, say), not that the user forgot to type something — and asking someone to pair with
      // their own machine just sends them hunting through Settings for a code they should never need.
      if (err.status === 401 && REMOTE) {
        $("tokenErr").textContent = TOKEN ? t("Wrong pairing code — try again") : "";
        $("tokenModal").showModal();
        return;
      }
      console.error("init failed: " + err.message); // 控制台统一用英文 / console output is English throughout
      document.body.textContent = t("Failed to connect to the backend: ") + err.message;
    });
}
boot();
