"use strict";

// Settings, OAuth, and plugin catalog dialogs.
// Loaded in the order declared by renderer/index.html.

let settingsTab = "general";

function showSettingsTab(tab) {
  settingsTab = tab;
  document.querySelectorAll("#keysModal [data-pane]").forEach((p) => { p.hidden = p.dataset.pane !== tab; });
  document.querySelectorAll("#keysModal .settings-nav button").forEach((b) => {
    b.classList.toggle("on", b.dataset.tab === tab);
  });

  // The save button only means anything on the Keys pane; every other pane saves on selection
  $("keysSaveBtn").hidden = tab !== "keys";
  // archives mean a disk scan, so fetch only on arrival
  if (tab === "alumni") renderDeparted();
}

$("botPermSel").onchange = renderBotPermNote;
$("botEffortSel").onchange = () => {
  effortValue = $("botEffortSel").value;
  renderBotEffortNote();
};

let xaiPollTimer = null;
let chatgptPollTimer = null;

// One row per subscription: status dot, one line of text, buttons. Same shape as the block in the
// new-bot dialog, so the two look alike and nothing has to be learned twice.
function renderOAuthBlock(state_, ids, signedIn, signIn) {
  const login = $(ids.login);
  if (!login) return;
  const s = state_ || {};

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

// One line per catalog entry saying what it needs and where it will be slow — next to the button, so
// nobody clicks first and only then discovers a key is required, or takes a long download for a hang.
function catalogNote(entry) {
  const bits = [entry.url ? t("remote") : t("local")];
  if (entry.need?.kind === "key") bits.push(t("needs a key"));
  if (entry.need?.kind === "path") bits.push(t("needs a directory"));
  if (entry.oauth) bits.push(t("Opens a browser for authorization"));
  if (entry.slow) bits.push(t("first install downloads a lot"));
  return bits.join(" · ");
}

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

// installFromCatalog asks for whatever is missing and then installs: anything it needs is requested on
// the spot, and cancelling at any step leaves nothing behind (never a half-installed plugin).
async function installFromCatalog(entry, btn) {
  const body = { name: entry.name };
  const need = entry.need;

  if (need?.kind === "key") {
    // do not ask again for a key that is already saved
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
      hint: t("Bots can read and write everything under this directory; writes still require your approval."),
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
    : t("Installing %s — the first install may take a minute while the package downloads.", entry.label);
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


    // The address gave a marketplace listing rather than a single plugin — about half of the sampled real
    // repositories are shaped this way, so this is one of the normal paths, not an error path.
    if (res.marketplace) {
      $("bundleErr").textContent = "";
      openMarketplacePicker(src, res.marketplace);
      return;
    }
    $("bundleSrc").value = "";
    const p = res.plugin || {};

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

// Task board cleanup ----

$("clearDoneBtn").onclick = () => {
  api("/api/tasks/clear_done", {}).catch((err) => toast(err.message));
};

// Create & edit bots, group settings ----
