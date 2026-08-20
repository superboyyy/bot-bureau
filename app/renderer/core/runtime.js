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

// Plugin catalog: the ones the panel installs in a single click. Each entry either needs nothing or is
// short exactly one thing (need), which is asked for on the spot and then installed — rather than
// sending the user elsewhere to prepare something and come back.
// Ordered by setup cost: nothing-to-configure first, key/authorization ones last.

// need.kind:
// null   — install straight away
// path   — append a directory argument
// key    — save a key first, then reference it
const MCP_CATALOG = [
  {
    name: "deepwiki", label: "DeepWiki",
    desc: "Ask questions about any public GitHub repository — its documentation, architecture, and code.",
    url: "https://mcp.deepwiki.com/mcp",
  },
  {
    name: "context7", label: "Context7",
    desc: "Up-to-date documentation and code examples for thousands of libraries.",
    url: "https://mcp.context7.com/mcp",
  },
  {
    name: "memory", label: "Memory",
    desc: "A knowledge graph where the team can record and search facts later.",
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
    desc: "Issues, pull requests, code search, and file contents on GitHub.",
    url: "https://api.githubcopilot.com/mcp/",
    need: {
      kind: "key", key: "GITHUB_PAT", as: "bearer", label: "GitHub personal access token",
      hint: "Create a token at github.com/settings/personal-access-tokens — it is stored on this machine only.",
    },
  },
  {
    name: "exa", label: "Exa Search",
    desc: "AI-focused web search that returns full page content, not just links.",
    command: "npx", args: "-y exa-mcp-server",
    need: { kind: "key", key: "EXA_API_KEY", as: "env", label: "Exa API key", hint: "Get one from exa.ai." },
  },
  {
    name: "firecrawl", label: "Firecrawl",
    desc: "Turn entire websites into clean Markdown.",
    command: "npx", args: "-y firecrawl-mcp",
    need: { kind: "key", key: "FIRECRAWL_API_KEY", as: "env", label: "Firecrawl API key", hint: "Get one from firecrawl.dev." },
  },

  // Remote OAuth connectors: the engine discovers endpoints, registers a client, and runs PKCE.
  // Install opens a browser for approval; afterward tools appear without a pasted token.
  {
    name: "atlassian", label: "Atlassian",
    desc: "Jira issues and Confluence pages — search, summarize, create, and update.",
    url: "https://mcp.atlassian.com/v1/mcp/authv2",
    oauth: true,
  },
  {
    name: "linear", label: "Linear",
    desc: "Issues, projects, and cycles in Linear.",
    url: "https://mcp.linear.app/mcp",
    oauth: true,
  },
  {
    name: "notion", label: "Notion",
    desc: "Search, read and update pages and databases in Notion.",
    url: "https://mcp.notion.com/mcp",
    oauth: true,
  },
  {
    name: "sentry", label: "Sentry",
    desc: "Search errors, inspect issues, and dig into performance in Sentry.",
    url: "https://mcp.sentry.dev/mcp",
    oauth: true,
  },
];
