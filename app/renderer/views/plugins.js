"use strict";

// MCP, skill, and installed bundle views.
// Loaded in the order declared by renderer/index.html.

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

// Skills ----

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

      // Spell out the origin: only one skill of a given name is in force, and when something looks wrong
      // you need to know which one that is
      code: s.source === "local" ? t("local · %s", s.dir) : t("from plugin %s", s.source),
    }));
  }
}

// Plugin packages ----

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


    // Each member template gets an "add to the team" action — the step unique to Bot Bureau, where an
    // agent elsewhere can only be a subagent and here becomes an actual member.
    for (const a of b.agents || []) {
      actions.push([t("Add %s", a.name), "", () => addAgentAsBot(a)]);
    }

    // Upgrading reconciles in place: the tool subset you picked and the authorization you completed stay,
    // unlike a remove-and-reinstall that zeroes both
    if (b.source) {
      actions.push([t("Update"), "", (btn) => updateBundle(b, btn)]);
    }
    actions.push([t("Remove"), "danger", async () => {
      const ok = await ask({
        title: t("Remove package %s", b.name),
        hint: t("Its plugins and skills are removed with it. Members you already created stay."),
        ok: t("Remove"), danger: true,
      });
      if (ok) api("/api/plugins/remove", { name: b.name }).catch((err) => toast(err.message));
    }]);
    const sub = [b.description, b.version && "v" + b.version, brought.join(" · ")].filter(Boolean).join(" — ");
    wrap.append(item({
      title: b.name,
      sub,

      // What is unsupported has to be visible: silently dropping half a package is far worse than saying so
      code: b.ignored?.length ? t("Not supported: ") + b.ignored.join("; ") : "",
      actions,
    }));
  }
}

// addAgentAsBot loads a bundled member template into the new-bot form. It does not create one outright:
// the model and permissions are the user's call, and they ought to see what the role actually says first.

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
  chatViewSerial++;
  delete unread[id];
  closePop();
  clearReply();

  // Attachments belong to the conversation. A picture still sitting there after switching away and
  // back would go to someone else on the next Return.
  clearAttachments();
  renderChatList();
  renderHeader();
  renderMsgs(true);





  // Entering a conversation asks once whether anything older exists.

  // Waiting for "scrolled to the top" does not work: content shorter than the viewport has no scrollbar
  // and fires no scroll event at all, so nothing older is fetched and it is never learnt that this is
  // the start — and on screen those two cases look identical.
  loadOlder();
  $("input").focus();
}

// Search ----

$("search").addEventListener("input", (e) => {
  filter = e.target.value;
  renderChatList();
});

// Input ----

$("composer").addEventListener("submit", (e) => {
  e.preventDefault();
  const input = $("input");
  const text = input.value.trim();

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

// Attachments waiting to be sent ----

// Three ways in, all landing here: the paperclip, a paste into the field, a drop onto the window.
// Paste matters most — screenshot, ⌘V, nothing touching disk in between — the shortest path there is to
// showing a bot what is on screen.

// The bytes are read to base64 here rather than kept as File objects until send: reading is
// asynchronous, and a user may well paste an image and hit Return in the same breath. Read up front and
