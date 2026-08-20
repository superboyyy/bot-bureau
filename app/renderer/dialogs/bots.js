"use strict";

// Create and edit bot dialog.
// Loaded in the order declared by renderer/index.html.

const fld = (form, name) => form.elements.namedItem(name);
let editingBot = "";                              // non-empty means edit, not create
const avatarDraft = { bot: "", group: "" };       // avatar picked in the open dialog

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
    img.onerror = () => rej(new Error(t("The image could not be decoded.")));
    img.src = dataURL;
  });
  const side = Math.min(img.naturalWidth, img.naturalHeight);
  if (!side) throw new Error(t("The image is missing dimensions."));
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

// ---- Connecting a model: vendor → auth → model ----

// One three-step form shared by the new/edit bot dialog and first-run onboarding. Three premises:
// · the vendor catalog comes from the engine (/api/providers), so the client carries no model list to go stale;
// · model names are always fetched live (/api/models) — on failure we say so rather than invent one;
// · signing in or pasting a key happens right here, not via a detour through the settings dialog.

// A code with a copy button.

// Device codes and pairing codes both exist to be read here and typed somewhere else, which is exactly
// where transcription goes wrong — particularly with mixed alphanumerics like 5BEC-3D33. Any code the
// user has to carry gets a copy button next to it, shown in a monospace face so O/0 and I/1 stay apart.
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {

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

// What "working directory" actually means: one sentence from the engine, shared by all three tier
// notes (see config.PermScopeNote)
let permScopeNote = "";
let effortLevels = [];
let effortValue = "";
let effortRequest = 0;

function defaultBotEffortLevel() {
  return {
    id: "",
    label: t("Default"),
    note: t("Send no thinking setting at all — the safest choice, and the only one every model accepts"),
  };
}

// Effort values vary by concrete model, so the engine is asked again whenever the selected model
// changes. The request id prevents a slower answer for the previous model from repainting this one.
async function refreshBotEffort(providerID, modelID, value) {
  const request = ++effortRequest;
  let levels = [defaultBotEffortLevel()];
  try {
    const query = "?provider=" + encodeURIComponent(providerID || "") +
      "&model=" + encodeURIComponent(modelID || "");
    const fetched = (await api("/api/efforts" + query)).levels || [];
    if (fetched.length) levels = fetched;
  } catch (err) {
    console.log("effort levels unavailable: " + err.message);
  }
  if (request !== effortRequest) return;
  effortLevels = levels;
  fillBotEffortSel(value);
}

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

  // After a model change the previous tier may be gone; fall back to the model default rather than
  // leaving a value the dropdown does not contain.
  const keep = effortLevels.some((e) => e.id === (value || ""));
  sel.value = keep ? value || "" : "";
  effortValue = sel.value;
  renderBotEffortNote();
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

// This member's working directories: their own (handed out by the engine, not revocable) plus the ones
// the user named in conversation (revocable).

// Printing full paths is deliberate. Once packaged, their own directory sits under
// ~/Library/Application Support — a level Finder hides by default — under a random id that appears
// nowhere else, so without printing it here the promise "no approvals inside the workspace" has nothing
// the user could check it against.

// Remove only, never add. Granting is the act of naming the directory in conversation; an add button
// here would turn something a person has to say out loud into a switch that is easy to flip.
function renderBotRoots(bot) {
  const box = $("botRoots");
  const note = $("botRootsNote");
  if (!box) return;
  note.textContent = permScopeNote;
  if (!bot) {

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
    head.append(el("span", "", t("You pointed to this directory")));
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
  renderFetchHosts();
  renderSandbox();
}

function renderFetchHosts() {
  const ta = $("fetchHosts");
  if (!ta) return;
  const list = (state.settings || {}).fetch_hosts || [];
  if (document.activeElement !== ta) ta.value = list.join("\n");
}

async function saveFetchHosts() {
  const ta = $("fetchHosts");
  const err = $("fetchHostsErr");
  if (!ta) return;
  if (err) err.textContent = "";
  const hosts = ta.value.split(/\n/).map((s) => s.trim()).filter(Boolean);
  try {
    state.settings = await api("/api/settings", { fetch_hosts: hosts });
    renderFetchHosts();
  } catch (e) {
    if (err) err.textContent = e.message;
  }
}

function renderSandbox() {
  const s = ((state.settings || {}).sandbox) || {};
  const setCheck = (id, on) => {
    const n = $(id);
    if (!n || document.activeElement === n) return;
    n.checked = !!on;
  };
  setCheck("sandboxEnabled", s.enabled !== false);
  setCheck("sandboxAutoAllow", !!s.auto_allow_bash);
  setCheck("sandboxAllowUnsandboxed", s.allow_unsandboxed !== false);
  const note = $("sandboxNote");
  if (note) {
    if (!s.available) {
      note.textContent = t("Command isolation is unavailable on this system. Bash runs on the host.");
    } else {
      let msg = t("Command isolation: %s", s.backend || "");
      if (!s.network) msg += " " + t("This backend restricts writes; it does not block network access.");
      note.textContent = msg;
    }
  }
}

async function saveSandbox() {
  const err = $("sandboxErr");
  if (err) err.textContent = "";
  const enabled = $("sandboxEnabled");
  const autoAllow = $("sandboxAutoAllow");
  const allowUn = $("sandboxAllowUnsandboxed");
  if (!enabled || !autoAllow || !allowUn) return;
  try {
    state.settings = await api("/api/settings", {
      sandbox: {
        enabled: enabled.checked,
        auto_allow_bash: autoAllow.checked,
        allow_unsandboxed: allowUn.checked,
      },
    });
    renderSandbox();
  } catch (e) {
    if (err) err.textContent = e.message;
  }
}

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
    } catch { /* try again on the next tick */ }
  }, 1500);
  return timer;
}

function field(labelText, control) {
  const l = document.createElement("label");
  l.append(el("span", "", labelText), control);
  return l;
}

// Returns a picker mounted on root: setConfig repopulates it, getConfig reads it back.

// onVendor/onModel (optional) announce changes; the picker does not own the reasoning-effort selector.
function createModelPicker(root, onVendor, onModel) {
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
    if (onVendor) onVendor(cur.provider_id, cur.model);
  };
  refreshBtn.onclick = () => refresh();
  modelSel.onchange = () => {
    cur.model = modelSel.value;
    if (onModel) onModel(cur.provider_id, cur.model);
  };
  modelInput.oninput = () => { cur.model = modelInput.value.trim(); };
  modelInput.onchange = () => { if (onModel) onModel(cur.provider_id, cur.model); };
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

    // An empty dropdown must not go silent: say what is missing so the next step is obvious
    if (!models.length && !cur.model) {
      const o = document.createElement("option");
      o.value = "";
      o.disabled = true;
      o.selected = true;
      o.textContent = loading
        ? t("Loading…")
        : cur.auth === "key"
          ? t("Enter the key above, then click Refresh")
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
    const beforeModel = cur.model;
    loading = true;
    modelErr = "";
    paint();
    try {






      // Save the freshly typed key before asking.

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






      // A failed fetch does not switch to manual entry.

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
    if (cur.model !== beforeModel && onModel) onModel(cur.provider_id, cur.model);
  }

  return {
    setConfig(c) {
      clearInterval(pollTimer);

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

    // A key typed in place: the caller saves it to the key store before saving the bot
    pendingKey,
    clearKeyInput() { keyInput.value = ""; },
    repaint: paint,
  };
}

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

const botPicker = createModelPicker(
  $("botModelPicker"),
  (pid, model) => {
    effortValue = "";
    refreshBotEffort(pid, model, "");
  },
  (pid, model) => refreshBotEffort(pid, model, effortValue),
);

// One "+" in the sidebar covers both creating a bot and creating a group.
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
    entry(ICON_PLUS, t("New bot"), t("Create a member with its own model and workspace"), () => openBotModal("")),
    entry(ICON_PEOPLE, t("New group chat"), t("Let several members collaborate on one task"), () => createGroup()),
  );
  document.body.append(pop);
  const r = $("newBtn").getBoundingClientRect();
  pop.style.left = r.left - 6 + "px";
  pop.style.top = r.bottom + 6 + "px";
};

// The name and the @mention id collapse into one field.

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

// With no ASCII in the name (all-CJK, say) nothing can be derived, so fall back to bot-2, bot-3, …

// The internal id is the name's slug plus five random characters.

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

  // The prefix is capped at 18 so the separator and five random characters stay inside the 24-character id limit
  const prefix = (base || "bot").slice(0, 18);
  for (let i = 0; i < 20; i++) {
    const id = prefix + "-" + randomSuffix(5);
    if (!taken.has(id)) return id;
  }
  return prefix + "-" + randomSuffix(5);
}

// Derive an internal id from the name when creating, and leave it alone when editing. It never
// surfaces in the UI: the user should have to remember one name, and that is the one called in a
// group — the engine matches both the id and the display name.
function paintBotID() {
  if (editingBot) return; // the id keys the workspace; fixed after creation
  const form = $("botForm");
  fld(form, "name").value = freeBotID(slugify(fld(form, "display_name").value));
}

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

  // While editing, the name field carries the current display name, or the id when none was set —
  // whatever the user sees in the list
  fld(form, "display_name").value = c.display_name || c.name || "";
  paintBotID();
  fld(form, "role").value = c.role || "";
  fld(form, "description").value = c.description || "";
  fld(form, "prompt").value = c.prompt || "";

  // Imported role instructions start expanded: the user should see exactly what they are adding before
  // hitting create
  $("botPromptRow").open = !!c.prompt;
  botPicker.clearKeyInput();
  effortValue = c.effort || "";
  botPicker.setConfig(c);
  fillBotPermSel(c.permission || "");
  renderBotRoots(name ? (state.bots || []).find((b) => b.name === name) : null);
  const picked = botPicker.getConfig();
  refreshBotEffort(picked.provider_id, picked.model, effortValue);
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

  // No separate display name when it matches the id: the list shows the id anyway
  if (cfg.display_name === cfg.name) cfg.display_name = "";
  cfg.permission = $("botPermSel").value;
  cfg.effort = $("botEffortSel").value; // empty means follow the global setting
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
