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

let state = { bots: [], approvals: [], routines: [], tasks: [], keys: [], group_members: [], groups: [], pins: [], mcp: [], default_bot: "" };
const isGroupChatId = (id) => id === "group" || /^g_/.test(id);
// 置顶是会话的属性，群聊和私聊同一套判断（引擎里存的就是会话 id）
// Pinning is a property of the conversation, one notion for group chats and DMs alike (the engine
// stores conversation ids)
const isPinned = (id) => (state.pins || []).includes(id);
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
// 正在引用回复的那条消息（{id, chat, source, text}），没有就是 null
// The message currently being replied to ({id, chat, source, text}), or null
let replyTo = null;
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

// check 传进来就多一个复选框，此时确认返回的是 { checked }，取消照旧是 false——
// 两种情况都还是"真值 = 用户点了确认"，所以老的 if (ok) 调用一个都不用改。
// Passing check adds a checkbox, and confirming then resolves to { checked } while cancelling stays
// false — truthy still means "the user said yes", so no existing if (ok) call site has to change.
function ask({ title, hint = "", ok, cancel, danger = false, field = false, fieldLabel = "", placeholder = "", check = null }) {
  return new Promise((resolve) => {
    askResolver = resolve;
    $("askTitle").textContent = title;
    $("askHint").textContent = hint;
    $("askHint").hidden = !hint;
    $("askFieldWrap").hidden = !field;
    $("askField").value = "";
    $("askField").placeholder = placeholder;
    $("askFieldLabel").textContent = fieldLabel;
    $("askCheckWrap").hidden = !check;
    $("askCheck").checked = !!(check && check.checked);
    $("askCheckLabel").textContent = check ? check.label : "";
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
  const v = fieldOn ? $("askField").value
    : ($("askCheckWrap").hidden ? true : { checked: $("askCheck").checked });
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

// 置顶标记：一枚图钉，跟着名字走。
// 光靠"排在上面"说不清它为什么在上面——列表本来就按最后动静排序，一个刚聊过的会话待在
// 顶上再正常不过。有了这枚钉子，"它钉在这儿"和"它刚有动静"才分得开。
//
// The pin marker: a pushpin that travels with the name.
// Position alone cannot explain itself — the list already sorts by last activity, so a conversation
// that was just used sits on top perfectly naturally. The pushpin is what separates "this one is
// nailed here" from "this one just said something".
function pinGlyph() {
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("class", "pin");
  svg.setAttribute("width", "11"); svg.setAttribute("height", "11");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "currentColor");
  const p = document.createElementNS(NS_SVG, "path");
  p.setAttribute("d", "M16 9V4h1a1 1 0 0 0 0-2H7a1 1 0 0 0 0 2h1v5c0 1.66-1.34 3-3 3v2h5.97v7l1 1 1-1v-7H19v-2c-1.66 0-3-1.34-3-3z");
  svg.append(p);
  return svg;
}

// 置顶/取消：先信引擎的回话，再就地重排一次。
// 引擎那边同时会广播一条 refresh，别的设备靠它跟上；本机不等那一圈回来——手上这一下点完
// 就该看见列表动，而不是隔着一次拉取才动。
//
// Pin or unpin: take the engine's answer and re-sort on the spot.
// The engine also broadcasts a refresh so other devices follow along; this one does not wait for that
// round trip — the list should move the moment it is clicked, not one fetch later.
async function setPinned(id, pinned) {
  try {
    const r = await api("/api/pins", { chat: id, pinned });
    state.pins = r.pins || [];
    renderChatList();
  } catch (err) {
    toast(err.message);
  }
}

function convRow({ id, av, name, ev, badge, deletable, groupDel }) {
  const pinned = isPinned(id);
  const row = el("div", "conv" + (current === id ? " active" : ""));
  const body = el("div", "body");
  const line = el("div", "line");
  line.append(el("span", "name", name));
  if (pinned) line.append(pinGlyph());
  const time = listTime(ev && ev.ts);
  if (time) line.append(el("span", "time", time));
  const empty = ev ? previewOf(ev) : (id.startsWith("dm:") && !modelSet(id.slice(3))
    ? t("No model selected")
    : t("No messages yet"));
  // 未读数挂在第二行的预览旁边，不挂在整行的右端。挂在行上时它是 .conv 的一个 flex 兄弟，
  // 一出现就把 .body 整体压窄，于是同一个会话的时间戳会随着"有没有新消息"左右跳一下——
  // 而时间是这一行里唯一需要跨行对齐着扫读的东西，它不该因为别处的状态变化而移位。
  // 第二行的预览本来就是会被截断的文字，让出这点宽度不产生任何视觉抖动。
  //
  // The unread count sits next to the preview on the second line, not at the right edge of the whole
  // row. As a flex sibling of .body it narrowed the entire body the moment it appeared, so a
  // conversation's timestamp jumped left and back as messages went unread — and the timestamp is the
  // one thing here read by scanning down the column, so it must not move because something else
  // changed state. The preview on line two is already truncated text; giving up that width shifts
  // nothing visible.
  const foot = el("div", "foot");
  foot.append(el("div", "prev", empty));
  if (badge) foot.append(el("span", "badge", String(badge)));
  body.append(line, foot);
  row.append(av, body);
  // 设置和删除走右键菜单，不在行里放按钮。
  // 行内按钮悬停才出现，出现的位置正好压在会话预览和时间上——为了给它们腾地方，
  // 之前还得在悬停时把时间和未读数整个 opacity:0 掉。也就是说，想看清一行的内容，
  // 恰恰不能把鼠标放上去。菜单里给的是文字，比两个小图标也更说得清做什么。
  // 发现性没丢：标题栏点一下同样打开设置（见 renderHeader）。
  //
  // Settings and delete move into a right-click menu instead of buttons living in the row. Those
  // buttons only appeared on hover, and they appeared exactly on top of the preview text and the
  // timestamp — so much so that making room for them meant fading the time and the unread badge to
  // opacity:0 while hovering. Which is to say: reading a row was impossible precisely while pointing at
  // it. Text in a menu also says what each action does better than two small glyphs ever did.
  // Nothing is lost in discoverability: clicking the chat header opens the same settings (renderHeader).
  const menu = [
    [pinned ? t("Unpin") : t("Pin to top"), "", () => setPinned(id, !pinned)],
    [t("Settings"), "", () => (isGroupChatId(id) ? openGroupModal(id) : openBotModal(id.slice(3)))],
  ];
  if (groupDel) {
    menu.push([t("Delete this group"), "danger", async () => {
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
    }]);
  }
  if (deletable) {
    menu.push([t("Remove this bot"), "danger", async () => {
      const bot = id.slice(3);
      // 资料删不删让用户当场决定，而不是替他决定完再告诉他文件在 data/ 下——
      // id 是随机串又从不露面，用户翻到那个文件夹也认不出哪个目录是这位成员的，
      // 所以"留着"必须同时说清楚之后在哪能看见它。
      //
      // The user decides then and there whether the files go, instead of being told after the fact that
      // they are somewhere under data/ — an id is a random string that never surfaces, so even in that
      // folder nobody could tell which directory was whose. "Kept" only means something alongside where
      // it can be seen afterwards.
      const routines = (state.routines || []).filter((r) => r.bot === bot).length;
      const hint = sentences(
        t("Their memory and work files are kept; Settings › Former members is where you can read or delete them."),
        routines === 1 ? t("The routine assigned to them stops too.") : "",
        routines > 1 ? t("The %s routines assigned to them stop too.", routines) : "",
      );
      const res = await ask({
        title: t("Remove %s", name),
        hint,
        ok: t("Remove"),
        danger: true,
        check: { label: t("Delete their memory and work files as well"), checked: false },
      });
      if (!res) return;
      api("/api/bots/delete", { name: bot, purge: !!res.checked })
        .then((r) => { if (r && r.warning) toast(r.warning); })
        .catch((err) => toast(err.message));
    }]);
  }
  row.oncontextmenu = (e) => contextMenu(e, menu);
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
  // 置顶的整体浮到最上面；两段内部还是同一条老规矩：有动静的排前面，按时间倒序，
  // 没聊过的保持配置顺序垫底。置顶区不按"钉的时间"排——用户钉了几个会话是想留住这几行，
  // 不是想给它们再定一套次序，而那几行照旧谁刚说话谁在上，读起来和列表其余部分一致。
  //
  // Pinned conversations float to the top as a block; within each block the old rule still holds —
  // activity first, newest on top, untouched ones keeping config order at the bottom. The pinned block
  // is not ordered by when each was pinned: pinning a few conversations is about keeping those rows,
  // not about giving them a second ordering, and whoever spoke last still leads — the same way the
  // rest of the list reads.
  shown.sort((a, b) =>
    (Number(isPinned(b.id)) - Number(isPinned(a.id))) ||
    (((b.ev && b.ev.ts) || 0) - ((a.ev && a.ev.ts) || 0)));
  nav.replaceChildren(...shown.map(convRow));
  if (!shown.length) {
    nav.append(el("div", "empty", rows.length
      ? t("No matching conversations")
      : t("No members yet — press ＋ to hire your first one")));
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
function msgNode(ev, gutter, showWho, quoted = true) {
  const node = el("div", "msg" + (ev.source === "user" ? " user" : ""));
  node.dataset.eid = ev.id;
  const body = el("div", "msg-body");
  if (showWho && ev.source !== "user") body.append(el("div", "who", titleOf(ev.source)));
  // 附件在气泡上面：先看见发了什么，再读随之说了什么。空正文的消息（只丢了张图）
  // 这样也还是一条完整的消息，而不是一个孤零零的空气泡。
  // Attachments sit above the bubble: what was sent is seen first, what was said about it second. A
  // message with no text — just a dropped image — then still reads as a whole message rather than a
  // stray empty bubble.
  if (ev.files && ev.files.length) body.append(filesNode(ev.files));
  if (String(ev.text || "").trim() || !(ev.files && ev.files.length)) body.append(bubbleNode(ev.text));
  if (ev.reply_to && quoted) body.append(quoteNode(ev));
  if (gutter && ev.source !== "user") {
    node.append(showWho ? avatar(ev.source, MSG_AV) : el("span", "av-slot"));
  }
  node.append(body);
  node.oncontextmenu = (e) => contextMenu(e, [[t("Reply"), "", () => startReply(ev)]]);
  return node;
}

const speakerName = (src) => (src === "user" ? t("You") : titleOf(src));

// 消息里带的附件。图片直接铺出来，别的按文件条列出来。
//
// 事件里只有元数据，正文从 /api/file/<id> 取——base64 塞进 events.json，几张截图就能
// 把整份聊天记录撑爆。取图这条请求也要带配对码，所以走 fileURL 而不是直接拼路径。
//
// The attachments a message carries: images laid out as images, everything else as a file row.
//
// The event holds metadata only and the bytes come from /api/file/<id> — base64 inside events.json
// would blow the whole chat log apart after a few screenshots. That request needs the pairing code too,
// hence fileURL rather than a hand-built path.
function filesNode(files) {
  const box = el("div", "files");
  for (const f of files) {
    if (String(f.mime || "").startsWith("image/")) {
      const img = document.createElement("img");
      img.className = "shot";
      img.alt = f.name || "";
      img.title = f.name || "";
      fileURL(f.id).then((url) => { if (url) img.src = url; });
      img.onclick = () => lightbox(img.src, f.name || "");
      box.append(img);
      continue;
    }
    const row = el("div", "file-row");
    row.title = f.name || "";
    row.append(el("span", "ic", "📄"), el("span", "nm", f.name || ""), el("span", "sz", humanSize(f.bytes || 0)));
    box.append(row);
  }
  return box;
}

// fileURL 把一份附件取成一个能直接喂给 <img> 的地址。
//
// 不能直接把 /api/file/<id> 写进 src：这个接口和其余接口一样在配对码后面，而 <img> 带不了
// Authorization 头。把配对码塞进查询串倒是能让图显示出来——同时也就把它写进了每一条日志、
// 每一份历史记录。所以老老实实 fetch 一次，转成 blob 地址。
//
// 按 id 缓存。内容寻址的文件永远不会变，同一张图在历史里翻过去翻回来也只取一次。
//
// fileURL turns an attachment into an address an <img> can take directly.
//
// /api/file/<id> cannot go into a src as-is: like every other endpoint it sits behind the pairing code,
// and an <img> cannot carry an Authorization header. Putting the code in the query string would make
// the picture appear — and write the code into every log and every history file along with it. So it is
// fetched properly and handed over as a blob address.
//
// Cached by id. These files are content-addressed and never change, so scrolling past the same image
// twice fetches it once.
const fileCache = new Map();
function fileURL(id) {
  if (fileCache.has(id)) return fileCache.get(id);
  const headers = TOKEN ? { Authorization: "Bearer " + TOKEN } : {};
  const p = fetch(BACKEND + "/api/file/" + encodeURIComponent(id), { headers })
    .then((r) => (r.ok ? r.blob() : null))
    .then((b) => (b ? URL.createObjectURL(b) : ""))
    .catch(() => "");
  fileCache.set(id, p);
  return p;
}

// 点开一张图：铺满窗口看原尺寸，点哪儿都关掉。
// Clicking an image: full window at its own size, and clicking anywhere closes it.
function lightbox(src, name) {
  if (!src) return;
  const box = el("div", "lightbox");
  const img = document.createElement("img");
  img.src = src;
  img.alt = name;
  box.append(img);
  const close = () => { box.remove(); document.removeEventListener("keydown", esc); };
  const esc = (e) => { if (e.key === "Escape") close(); };
  box.onclick = close;
  document.addEventListener("keydown", esc);
  document.body.append(box);
}

// 气泡下面挂的那个引文小框：谁说的 + 一行原话，点一下跳回原处。
// 挂在下面而不是压在气泡顶上——先读这条消息说了什么，再看它在回哪一句；
// 反过来的话，每条带引用的消息都要先跨过一段旧话才读得到新话。
// 里面存的是发出时抄下的副本，所以原消息滚出事件流之后引文照样读得到，只是跳不过去了。
//
// The little quotation box hanging under the bubble: who said it, one line of what they said, and a
// click jumps back. It hangs below rather than sitting on top of the bubble, so what this message says
// is read first and what it answers second; the other way round, every quoting message makes the
// reader step over an old line to reach the new one.
// The text inside is a copy taken when the message was sent, so it stays readable once the original
// has scrolled out of the event stream — only the jump stops working.
function quoteNode(ev) {
  const box = el("button", "quote");
  box.type = "button";
  box.append(el("span", "q-who", speakerName(ev.reply_src)), el("span", "q-text", ev.reply_text || ""));
  box.onclick = (e) => { e.stopPropagation(); jumpTo(ev.reply_to); };
  return box;
}

function jumpTo(id) {
  const target = $("msgs").querySelector(`.msg[data-eid="${id}"]`);
  if (!target) return;
  target.scrollIntoView({ block: "center", behavior: "smooth" });
  target.classList.remove("flash");
  void target.offsetWidth; // 重启动画：同一条被连点两次也要再闪一下 / restart the animation so a second click flashes again
  target.classList.add("flash");
}

// 引用某条消息来回复。引文只在本会话内有效，所以换会话时会清掉（见 switchChat）。
// Reply to a specific message. A quotation only means anything inside its own conversation, so
// switching away clears it (see switchChat).
function startReply(ev) {
  replyTo = { id: ev.id, chat: current, source: ev.source, text: ev.text };
  renderReplyBar();
  $("input").focus();
}

function clearReply() {
  replyTo = null;
  renderReplyBar();
}

function renderReplyBar() {
  const bar = $("replyBar");
  bar.hidden = !replyTo;
  if (!replyTo) return;
  bar.querySelector(".rb-who").textContent = t("Replying to %s", speakerName(replyTo.source));
  bar.querySelector(".rb-text").textContent = replyTo.text;
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
  // 「批准，并以后不再问这个目录」。这是把一个目录交给某位成员唯一说得准的时机：
  // 用户嘴上说的是「bot-bureau」，引擎猜不出那是哪儿；而这张卡上写着的就是完整路径。
  // 按钮把它原样印出来，用户点的就是他看见的那个目录。
  //
  // 它排在两个按钮下面、而且刻意做得比它们轻：这一下批的不只是眼前这条命令，
  // 而是那个目录里往后的所有命令。放大、做成主按钮，就是拿视觉分量替用户拿主意。
  //
  // "Approve, and stop asking about this directory." This is the one moment when handing a directory to
  // a member is unambiguous: the user says "bot-bureau" and the engine cannot tell where that is, while
  // the card in front of them spells the full path out. The button prints it verbatim, so the directory
  // they grant is the directory they can see.
  //
  // It sits below the two buttons and is deliberately quieter than either: this click approves not just
  // the command in view but everything that directory will see afterwards. Making it big or primary
  // would be using visual weight to decide on the user's behalf.
  if (ap.dir) {
    const grant = el("button", "grant");
    grant.type = "button";
    grant.title = t("This member will read, write and run commands in this directory without asking. Remove it in its settings.");
    grant.append(el("span", "lbl", t("Approve, and stop asking about")), el("span", "code", ap.dir));
    grant.onclick = () => {
      grant.disabled = true;
      api("/api/approve", { id: ap.id, approved: true, grant: true }).catch((e) => {
        grant.disabled = false;
        toast(e.message);
      });
    };
    box.append(grant);
  }
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
      el("div", "blank-desc", t("Press ＋ in the sidebar to hire your first member, then talk to it here.")),
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
  let prevMsg = 0;   // 紧挨在上面的那条消息（中间隔了别的东西就归零）/ the message directly above (zeroed once anything else comes between)

  const flushTrace = (live) => {
    if (!trace.length) return;
    box.append(traceNode(traceKey, trace, !!live));
    trace = [];
    lastSource = null;
    prevMsg = 0;
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
    prevMsg = 0;
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
      prevMsg = 0;
    }
    if (ev.ts) lastTs = ev.ts;

    if (ev.kind === "system") {
      flushTrace(false);
      box.append(el("div", "sys", ev.text));
      lastSource = null;
      prevMsg = 0;
    } else if (ev.kind === "tool") {
      if (!trace.length) traceKey = "trace:" + current + ":" + ev.id;
      trace.push(ev);
    } else {
      flushTrace(false);
      const runStart = ev.source !== lastSource;
      // bot 的引文是自动挂上去的，紧贴着被引的那条时它什么也没说明，画出来只是噪音；
      // 中间隔了过程行、别人的发言或一段时间，它才开始有用。用户自己点的引用一律保留——
      // 那是个明确的动作（在群里还决定了这句话交给谁），不该看着像没生效。
      // A bot's quotation is attached automatically, and directly beneath what it quotes it says
      // nothing that the layout does not already say. It earns its place once a run of steps, someone
      // else's message, or a stretch of time has come between. A quotation the user picked always
      // stays: that was a deliberate act — in a group it also decides who the message is for — and it
      // must not look like it failed to register.
      const quoted = ev.source === "user" || ev.reply_to !== prevMsg;
      // 群聊里才需要标发言人和头像；私聊双方明确 / Only the group chat needs speaker labels and avatars
      const node = msgNode(ev, isGroupChatId(current), runStart, quoted);
      if (runStart) node.classList.add("run-start");
      box.append(node);
      lastSource = ev.source;
      prevMsg = ev.id;
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
    // 负责人独占一行，备注另起一行。两者同处一行时它们是同一个 flex 容器里的兄弟，
    // 而 .item .sub 的 overflow-wrap:anywhere 会把每个 flex 项的最小宽度降到一个字，
    // 于是一句长备注就能把名字挤成竖着排的两三个字——看板上最该认出来的正是"这活归谁"。
    //
    // The owner gets a line of its own and the note starts a new one. Side by side they were siblings
    // in one flex container, and overflow-wrap:anywhere on .item .sub drops every flex item's minimum
    // width to a single character — so one long note squeezed the name down to two or three stacked
    // glyphs, when whose job it is is the thing the board exists to show.
    const sub = el("div", "sub");
    const who = el("div", "owner");
    who.append(avatar(task.owner, 14), el("span", "who-name", titleOf(task.owner)));
    sub.append(who);
    if (task.note) sub.append(el("div", "note", task.note));
    wrap.append(item({ title, sub }));
  }
}

// 例程行上的负责人是个下拉框，就地改派。
//
// 在这之前负责人是一行死文字，而且印的是内部 id——那串东西界面上别处都不出现，用户认不出是谁。
// 想换个人做只有一条歪路：让另一位成员按同一个名字重存一遍，靠"同名覆盖"把原来那条顶掉。
// 那既要求用户知道这个实现细节，又要求他把 prompt 一字不差地重述一遍，而这里超过 90 字就截断了，
// 想抄都抄不全。既然"这件活归谁"本来就是随时会变的，那它就该是一个能点的控件。
//
// The owner on a routine row is a dropdown that reassigns in place.
//
// It used to be dead text printing the internal id — a string that appears nowhere else in the UI and
// identifies nobody. Handing the work to someone else meant a sideways trick: have the other member
// save a routine under the very same name and let "same name overwrites" displace the original. That
// asked the user to know an implementation detail and to restate the prompt word for word, which this
// row truncates at 90 characters anyway. Whose job this is was always going to change; it belongs in a
// control you can click.
function routineSub(r, when) {
  const sub = el("div", "sub");
  const sel = document.createElement("select");
  // 负责人已经不在团队里的老数据：把它原样列出来并选中，否则下拉会显示成别人，
  // 看着像这条例程好好地归某人管，其实它到点只会被跳过
  // Legacy rows whose owner has left: list that id and keep it selected, or the dropdown would show
  // somebody else and imply the routine is in hand when it only ever gets skipped
  if (!state.bots.some((b) => b.name === r.bot)) {
    const gone = document.createElement("option");
    gone.value = r.bot;
    gone.textContent = t("%s (no longer here)", r.bot);
    sel.append(gone);
  }
  for (const b of state.bots) {
    const o = document.createElement("option");
    o.value = b.name;
    o.textContent = titleOf(b.name);
    o.selected = b.name === r.bot;
    sel.append(o);
  }
  sel.onchange = () => {
    api("/api/routines/update", { name: r.name, bot: sel.value }).catch((e) => {
      toast(e.message);
      sel.value = r.bot;
    });
  };
  sub.append(sel, document.createTextNode(" · " + t("every %s min · next %s", r.every_minutes, when)));
  return sub;
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
      sub: routineSub(r, when),
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

// ---- 已离职成员 / Former members ----
//
// 移除成员时选了保留，工作目录就改名成一份离职档案留在 data/workspaces 下。这一栏是它唯一的出口。
// 没有这一栏，"保留"就只是往 data/ 里堆垃圾：id 是随机串、界面上从不出现，用户就算翻到那个
// 文件夹，也认不出哪个目录是谁的、哪些人还在职。这里按显示名把它们一一列出来，能翻看，也能删掉。
//
// A member removed with their files kept leaves an archive under data/workspaces, and this pane is
// its only way out. Without it "kept" would just be junk accumulating in data/: ids are random strings
// that never appear in the UI, so even inside that folder nobody could tell which directory belonged to
// whom, or who still works here. This lists them by the name the user knew, to read or to delete.
const fmtDay = (ts) => {
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
};

const fmtBytes = (n) => {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
};

async function renderDeparted() {
  const wrap = $("departedList");
  wrap.replaceChildren(el("div", "empty", t("Loading…")));
  let list = [];
  try {
    list = (await api("/api/bots/departed")).departed || [];
  } catch (err) {
    wrap.replaceChildren(el("div", "empty", err.message));
    return;
  }
  wrap.replaceChildren();
  if (!list.length) {
    wrap.append(el("div", "empty", t("Nobody has left yet")));
    return;
  }
  for (const d of list) {
    const bits = [t("left %s", fmtDay(d.removed_at))];
    if (d.role) bits.push(d.role);
    bits.push(t("%s files · %s", d.files + (d.truncated ? "+" : ""), fmtBytes(d.bytes)));
    if (!d.has_memory) bits.push(t("no memory"));
    const node = item({
      title: d.display_name || d.id,
      sub: bits.join(" · "),
      actions: [
        [t("View"), "", (btn) => toggleDeparted(node, d, btn)],
        [t("Delete files"), "danger", async () => {
          const ok = await ask({
            title: t("Delete %s's files", d.display_name || d.id),
            hint: t("Their memory and everything in their workspace goes for good. This cannot be undone."),
            ok: t("Delete"),
            danger: true,
          });
          if (!ok) return;
          try {
            await api("/api/bots/departed/delete", { dir: d.dir });
          } catch (err) {
            toast(err.message);
          }
          renderDeparted();
        }],
      ],
    });
    wrap.append(node);
  }
}

async function toggleDeparted(node, d, btn) {
  const open = node.querySelector(".departed-detail");
  if (open) {
    open.remove();
    btn.textContent = t("View");
    return;
  }
  btn.textContent = t("Loading…");
  try {
    const res = await api("/api/bots/departed/detail", { dir: d.dir });
    const box = el("div", "departed-detail");
    box.append(el("div", "code", res.memory
      ? res.memory + (res.truncated ? "\n…" : "")
      : t("This one never wrote anything down.")));
    const files = res.files || [];
    if (files.length) {
      box.append(el("div", "sub", files.map((f) => f.name + (f.dir ? "/" : "")).join("  ")));
    }
    node.append(box);
    btn.textContent = t("Hide");
  } catch (err) {
    toast(err.message);
    btn.textContent = t("View");
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
    if (b.agents?.length) brought.push(t("%s members", b.agents.length));
    const actions = [];
    // 包里的每个成员模板都给一个"加进团队"：这是 Bot Bureau 独有的一步——
    // 别处的 agent 只能当子代理用，这里它就是一位真正的成员。
    // Each member template gets an "add to the team" action — the step unique to Bot Bureau, where an
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
        hint: t("Its plugins and skills go with it. Members you already created stay."),
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

// addAgentAsBot 把包里的成员模板灌进新建 bot 的表单。不直接建：模型和权限得用户自己定，
// 而且他应该先看见这个角色到底写了什么。
// addAgentAsBot loads a bundled member template into the new-bot form. It does not create one outright:
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
  clearReply();
  // 附件跟着会话走。切走再切回来还挂在框上的那张图，下一次回车就会发给另一个人。
  // Attachments belong to the conversation. A picture still sitting there after switching away and
  // back would go to someone else on the next Return.
  clearAttachments();
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
  // 只有附件、一个字没写也发得出去——把一张图丢进来本身就是要说的话
  // Attachments with not a word typed still send: dropping in a picture is itself the thing being said
  if (!text && !pending.length) return;
  const files = pending.map((p) => ({ name: p.name, mime: p.mime, data: p.data }));
  input.value = "";
  input.style.height = "auto";
  const reply_to = replyTo && replyTo.chat === current ? replyTo.id : 0;
  clearReply();
  clearAttachments();
  api("/api/send", { chat: current, text, reply_to, files })
    .catch((err) => toast(t("Send failed: ") + err.message));
});
$("replyCancel").onclick = clearReply;

// ---- 待发送的附件 / Attachments waiting to be sent ----
//
// 三条进来的路都通到同一处：点回形针、往输入框粘、往窗口里拖。粘贴是其中最要紧的一条——
// 截图之后 ⌘V，中间不落一次盘，这是给机器人看一眼屏幕最短的路。
//
// 正文在这里就读成 base64 存着，而不是留着 File 对象等发送时再读：读文件是异步的，
// 而用户完全可能贴完一张图立刻按回车。先读好，按下回车时就只剩一次 POST。
//
// Three ways in, all landing here: the paperclip, a paste into the field, a drop onto the window.
// Paste matters most — screenshot, ⌘V, nothing touching disk in between — the shortest path there is to
// showing a bot what is on screen.
//
// The bytes are read to base64 here rather than kept as File objects until send: reading is
// asynchronous, and a user may well paste an image and hit Return in the same breath. Read up front and
// Return has nothing left to do but one POST.
let pending = [];

function renderAttachments() {
  const tray = $("attachTray");
  tray.replaceChildren();
  tray.hidden = pending.length === 0;
  for (const p of pending) {
    const chip = el("div", "chip" + (p.preview ? " has-thumb" : ""));
    if (p.preview) {
      const img = document.createElement("img");
      img.src = p.preview;
      img.alt = "";
      chip.append(img);
    }
    const meta = el("div", "meta");
    meta.append(el("div", "nm", p.name), el("div", "sz", humanSize(p.size)));
    chip.append(meta);
    const x = el("button", "x", "✕");
    x.type = "button";
    x.title = t("Remove");
    x.onclick = () => {
      pending = pending.filter((q) => q !== p);
      renderAttachments();
    };
    chip.append(x);
    tray.append(chip);
  }
}

function clearAttachments() {
  pending = [];
  renderAttachments();
}

function humanSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + " KB";
  return n + " B";
}

// addFiles 读进来一批文件。上限和后端那份是同一套数字（engine/attach.go），
// 在这里先拦一道纯粹是为了让用户当场知道，而不是把 25MB 传上去再被退回来。
//
// addFiles takes in a batch of files. The limits are the same numbers the backend enforces
// (engine/attach.go); checking here first is only so the user finds out now, rather than after 25MB has
// gone up the wire and come back refused.
const MAX_FILES = 8;
const MAX_ONE = 10 * 1024 * 1024;
const MAX_ALL = 25 * 1024 * 1024;

async function addFiles(list) {
  const files = [...list].filter((f) => f && f.size >= 0);
  for (const f of files) {
    if (pending.length >= MAX_FILES) {
      toast(t("At most %s files can go with one message", MAX_FILES));
      break;
    }
    if (f.size > MAX_ONE) {
      toast(t("%s is too large (limit %s)", f.name || t("This file"), humanSize(MAX_ONE)));
      continue;
    }
    if (pending.reduce((n, p) => n + p.size, 0) + f.size > MAX_ALL) {
      toast(t("Those files come to more than one message can carry"));
      break;
    }
    try {
      pending.push(await readFile(f));
      renderAttachments();
    } catch (err) {
      toast(t("Could not read %s", f.name || ""));
    }
  }
}

function readFile(f) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onerror = () => reject(r.error);
    r.onload = () => {
      const url = String(r.result);
      const data = url.slice(url.indexOf(",") + 1);
      const mime = f.type || "";
      resolve({
        // 截图粘进来时 File 是没有名字的，给它一个能认出来的
        // A pasted screenshot arrives as a File with no name; give it one that reads as something
        name: f.name || (mime.startsWith("image/") ? t("pasted image") + guessExt(mime) : t("pasted file")),
        mime, size: f.size, data,
        preview: mime.startsWith("image/") ? url : "",
      });
    };
    r.readAsDataURL(f);
  });
}

const guessExt = (mime) => ({ "image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp" })[mime] || ".png";

$("attachBtn").onclick = () => $("attachFile").click();
$("attachFile").onchange = (e) => {
  addFiles(e.target.files);
  e.target.value = ""; // 同一个文件连选两次也要能触发 / picking the same file twice must still fire
};

// 粘贴：只在剪贴板真的带了文件时才接管，否则照常粘文字
// Paste: taken over only when the clipboard really carries files, so pasting text still just pastes text
$("input").addEventListener("paste", (e) => {
  const files = [...(e.clipboardData ? e.clipboardData.files : [])];
  if (!files.length) return;
  e.preventDefault();
  addFiles(files);
});

// 拖放：整个窗口都收，不用非得对准输入框。拖进来时给一层提示，否则用户不知道松手会怎样。
// Drag and drop over the whole window rather than only the field. A visible state while dragging, or
// nobody knows what letting go will do.
let dragDepth = 0;
window.addEventListener("dragenter", (e) => {
  if (![...(e.dataTransfer ? e.dataTransfer.types : [])].includes("Files")) return;
  e.preventDefault();
  if (++dragDepth === 1) document.body.classList.add("dropping");
});
window.addEventListener("dragover", (e) => {
  if ([...(e.dataTransfer ? e.dataTransfer.types : [])].includes("Files")) e.preventDefault();
});
window.addEventListener("dragleave", () => {
  if (--dragDepth <= 0) { dragDepth = 0; document.body.classList.remove("dropping"); }
});
window.addEventListener("drop", (e) => {
  if (!e.dataTransfer || !e.dataTransfer.files.length) return;
  e.preventDefault();
  dragDepth = 0;
  document.body.classList.remove("dropping");
  addFiles(e.dataTransfer.files);
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
  btn.title = mention ? t("Mention a member") : t("Jump to another chat");
  btn.setAttribute("data-i18n-title", mention ? "Mention a member" : "Jump to another conversation");
  btn.setAttribute("data-en-title", mention ? "Mention a member" : "Jump to another chat");
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
// Esc 一次退一层：先关浮层，没有浮层就撤掉正在引用的那条
// Esc backs out one layer at a time: the popover first, and the pending quotation once it is gone
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (pop) closePop();
  else if (replyTo) clearReply();
});

// 光标处弹出的操作菜单。外观和"点别处就关"直接复用 .pop 那一套，只有定位不同。
// items 是 [标签, 类名, 回调] 的数组。
// A menu that opens at the cursor. Appearance and click-away dismissal come straight from the .pop
// machinery; only the positioning differs. items is an array of [label, className, onclick].
function contextMenu(ev, items) {
  ev.preventDefault();
  closePop();
  pop = el("div", "pop");
  for (const [label, cls, onclick] of items) {
    const b = el("button", cls || "");
    b.type = "button";
    b.append(document.createTextNode(label));
    b.onclick = () => { closePop(); onclick(); };
    pop.append(b);
  }
  document.body.append(pop);
  // 先塞进 DOM 再量尺寸，然后夹回可视区——靠近右下角右键时，菜单不能有一半在窗口外
  // Measure only once it is in the DOM, then clamp: right-clicking near the bottom-right corner must
  // not leave half the menu outside the window
  const r = pop.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(ev.clientX, window.innerWidth - r.width - 8)) + "px";
  pop.style.top = Math.max(8, Math.min(ev.clientY, window.innerHeight - r.height - 8)) + "px";
}

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
  // 离职档案要扫磁盘，切到这一栏时才去取 / archives mean a disk scan, so fetch only on arrival
  if (tab === "alumni") renderDeparted();
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
    if (p.agents?.length) got.push(t("%s members", p.agents.length));
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
// 「工作目录」到底指哪儿：引擎给的一句话，三档说明共用（见 config.PermScopeNote）
// What "working directory" actually means: one sentence from the engine, shared by all three tier
// notes (see config.PermScopeNote)
let permScopeNote = "";
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
    permScopeNote = r.scope_note || "";
  } catch (err) {
    console.log("permission levels unavailable: " + err.message);
  }
}
const permOpt = (id) => permLevels.find((p) => p.id === id) || null;

// 这位成员的工作目录：自己那个（引擎给的，撤不掉），加上用户在对话里指定过的（可以撤）。
//
// 印出完整路径是有意的。打包之后自己那个目录落在 ~/Library/Application Support 底下，
// Finder 默认不显示那一层，目录名还是个从不露面的随机 id——不在这儿写出来，
// "工作目录内免审批"这句承诺就没有任何可核对的对象。
//
// 只给"移除"，不给"添加"。授权的动作是你在对话里说出那个目录；这里再放一个添加按钮，
// 就把一件本该由人主动说出口的事，变成了一个随手能点开的开关。
//
// This member's working directories: their own (handed out by the engine, not revocable) plus the ones
// the user named in conversation (revocable).
//
// Printing full paths is deliberate. Once packaged, their own directory sits under
// ~/Library/Application Support — a level Finder hides by default — under a random id that appears
// nowhere else, so without printing it here the promise "no approvals inside the workspace" has nothing
// the user could check it against.
//
// Remove only, never add. Granting is the act of naming the directory in conversation; an add button
// here would turn something a person has to say out loud into a switch that is easy to flip.
function renderBotRoots(bot) {
  const box = $("botRoots");
  const note = $("botRootsNote");
  if (!box) return;
  note.textContent = permScopeNote;
  if (!bot) {
    // 还没建出来的成员：工作目录要等它存在了才有路径可印
    // A member who does not exist yet: there is no path to print until they do
    box.replaceChildren(el("div", "empty", t("Created once you save this member.")));
    return;
  }
  const rows = [el("div", "item root-row")];
  rows[0].append(
    el("div", "title", t("Its own folder")),
    el("div", "code", bot.workspace || ""),
  );
  for (const dir of bot.roots || []) {
    const row = el("div", "item root-row");
    const head = el("div", "title");
    head.append(el("span", "", t("You pointed it here")));
    const del = el("button", "text-btn danger", t("Remove"));
    del.type = "button";
    del.onclick = async () => {
      del.disabled = true;
      try {
        await api("/api/bots/roots/remove", { name: bot.name, dir });
        const fresh = (state.bots || []).find((b) => b.name === bot.name);
        if (fresh) fresh.roots = (fresh.roots || []).filter((d) => d !== dir);
        renderBotRoots(fresh || bot);
      } catch (err) {
        del.disabled = false;
        toast(err.message);
      }
    };
    head.append(del);
    row.append(head, el("div", "code", dir));
    rows.push(row);
  }
  box.replaceChildren(...rows);
}

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
    entry(ICON_PLUS, t("New bot"), t("Hire a member with its own model and workspace"), () => openBotModal("")),
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
// 全都落到同一个兜底值上，第二位成员起就成了 bot-2、bot-3——一串既不好认、又跟名字毫无关系的
// 序号。id 现在不露在界面上，唯一性比可读性重要得多，随机串两头都占：拉丁名字仍然看得出是谁
// （wren-k3f9a），中文名字至少各不相同。
//
// The internal id is the name's slug plus five random characters.
//
// Those five are load-bearing rather than decorative: the slug keeps only a-z0-9-, so a name written
// in Chinese empties out entirely and every such bot lands on the same fallback — bot-2, bot-3 from
// the second member on, a sequence that is neither recognisable nor related to the name. The id no
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

// openBotModal 的 prefill 用于「从插件包导入一位成员」：新建表单先填好，再交给用户改。
// openBotModal's prefill serves "import a member from a plugin package": the new-bot form arrives
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
  renderBotRoots(name ? (state.bots || []).find((b) => b.name === name) : null);
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
      // 只提交改动过的成员：拉一个人进来不该让其他人也走一遍进出群的流程
      // Submit only the members that changed: adding one bot must not run everyone else through a join
      const was = new Set(state.group_members || []);
      for (const b of state.bots) {
        const now = !!groupMemberDraft[editingGroup][b.name];
        if (now !== was.has(b.name)) await api("/api/group/set", { group: "group", name: b.name, in: now });
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

// syncLocaleToEngine 把界面此刻说的语言告诉引擎。
//
// 语言是分两处存的：界面的选择在浏览器本地，引擎的在 data/settings.json，"跟随系统"这一档
// 两边各自解析一次。递过去的是已经解析好的 zh/en 而不是 "auto"——引擎那次解析查的是
// LANG/LC_ALL，而从图标启动的 GUI 子进程一个都没有，"auto" 到了它那里只会变成英文。
//
// syncLocaleToEngine tells the engine which language the UI is speaking right now.
//
// The language is kept in two places: the UI's choice in browser storage, the engine's in
// data/settings.json, and "follow the system" is resolved once on each side. What goes over is the
// already-resolved zh/en rather than "auto" — the engine resolves that by reading LANG/LC_ALL, which a
// GUI child process launched from an icon does not have, so "auto" only ever becomes English there.
async function syncLocaleToEngine() {
  try {
    await api("/api/settings", { locale: LOCALE });
  } catch (err) {
    console.log("sync locale to engine failed: " + err.message);
  }
}

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
  await syncLocaleToEngine();
  try {
    // 服务商目录的标签和说明由引擎按当前语言生成，切完语言要重拉一遍
    // The catalog's labels and notes are produced by the engine in its locale, so refetch after switching
    await loadProviderCatalog();
    await loadPermLevels();
    renderPermLevels();
    botPicker.repaint();
    if (onboardPicker) onboardPicker.repaint();
  } catch (err) {
    console.log("reload engine-supplied labels failed: " + err.message);
  }
};

// ---- 首次引导：先讲清楚"你是老板"，再带一遍能干什么，最后请第一位成员 ----
//
// 新装打开是空的：没有 bot，没有群。空界面本身讲不出这个产品是什么，所以引导要先立人称
// ——用户是事务所的老板，bot 是雇来的成员——再逐项指出创建和管理的入口。
// 最后一步才落到"建第一个 bot"，并且可以跳过：不该逼着还没想好的人先配一个模型。
//
// ---- First-run onboarding: establish "you are the boss", tour what is here, then hire the first member ----
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
      title: t("Hire your first member"),
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
    // 第一位成员就叫 assistant：什么都能干，用户之后自己改名字和人设
    // The first member is simply "assistant": a generalist the user can rename and reshape later
    await api("/api/bots", {
      name: "assistant",
      role: t("Assistant"),
      description: t("A generalist member: research, writing, code and chores."),
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
  // 语言要排在最前面：目录、权限档位、思考强度这些标签都是引擎按它自己的语言现生成的，
  // 先问就会拿回上一种语言的文案。以前只在用户动语言下拉时才同步一次，于是从没动过它的人
  // （界面语言是装的时候就定下的）永远撞不上那次同步，界面中文、引擎发的文案英文。
  //
  // The language goes first: the catalog, the permission tiers and the reasoning-effort labels are all
  // produced by the engine in its own language, so asking earlier brings back the previous one. The
  // sync used to happen only when the user touched the language dropdown, which meant anyone who never
  // touched it — the UI language having been settled at install time — never hit it at all, and read a
  // Chinese UI with the engine's text still in English.
  syncLocaleToEngine()
    // 服务商目录要先到位，模型选择器才画得出下拉框
    // The vendor catalog has to land first or the model picker has nothing to render
    .then(() => Promise.all([loadProviderCatalog(), loadPermLevels()]))
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
