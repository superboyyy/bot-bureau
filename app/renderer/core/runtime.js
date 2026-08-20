"use strict";

// Runtime configuration and shared renderer state.
// Loaded in the order declared by renderer/index.html.

"use strict";

// Bot Bureau renderer: bootstrap via /api/state + incremental updates over SSE, pure DOM rendering (no framework).

const PARAMS = new URLSearchParams(location.search);
const BACKEND = PARAMS.get("backend") || "http://127.0.0.1:8973";
const REMOTE = PARAMS.get("remote") === "1";

// When the main process turned on macOS vibrancy for the sidebar, the canvas behind it has to step
// aside or its flat fill buries the system blur. This only stamps the switch on <html>; style.css
// decides what stepping aside means. Opened directly in a browser the param is absent and the page
// keeps its opaque canvas, so neither side needs a branch.
if (PARAMS.get("vibrancy") === "1") document.documentElement.setAttribute("data-vibrancy", "");
let TOKEN = PARAMS.get("token") || localStorage.getItem("botbureau_token:" + BACKEND) || "";

let state = { bots: [], approvals: [], routines: [], tasks: [], keys: [], group_members: [], groups: [], pins: [], mcp: [], conversations: [], default_bot: "" };
const isGroupChatId = (id) => id === "group" || /^g_/.test(id);

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
let current = "group"; // "group" or "dm:<bot>"
let chatViewSerial = 0;
const unread = {};
let filter = "";                       // sidebar search term
const expanded = new Set();            // folds the user has opened

// The message currently being replied to ({id, chat, source, text}), or null
let replyTo = null;
let refetchTimer = null;

// Vendors and models are no longer hard-coded here: the catalog comes from the engine's /api/providers
// and model names are fetched live from /api/models. A baked-in table goes stale, and a retired model id
// only blows up when a message is actually sent.

// Plugin catalog: served by the engine (/api/mcp/catalog) so the panel and enable_connector share
// one list. Populated on first plugins-panel open (and after refresh).
let MCP_CATALOG = [];
let mcpCatalogLoaded = false;

async function ensureMCPCatalog() {
  if (mcpCatalogLoaded && MCP_CATALOG.length) return MCP_CATALOG;
  try {
    const res = await api("/api/mcp/catalog");
    MCP_CATALOG = res.catalog || [];
    mcpCatalogLoaded = true;
  } catch (err) {
    console.warn("mcp catalog", err);
  }
  return MCP_CATALOG;
}
