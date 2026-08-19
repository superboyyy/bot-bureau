"use strict";

// Tasks, routines, keys, groups, and departed members.
// Loaded in the order declared by renderer/index.html.

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

function renderTodos() {
  const head = $("todosHead");
  const wrap = $("todos");
  if (!head || !wrap) return;
  const dm = typeof current === "string" && current.startsWith("dm:");
  head.hidden = !dm;
  wrap.hidden = !dm;
  if (!dm) return;
  wrap.replaceChildren();
  const name = current.slice(3);
  const bot = (state.bots || []).find((b) => b.name === name);
  const todos = (bot && bot.todos) || [];
  if (!todos.length) {
    wrap.append(el("div", "empty", t("This member has no personal list yet")));
    return;
  }
  for (const todo of todos) {
    const title = el("div", "title");
    const cls = todo.status === "done" ? "done" : "pending";
    title.append(el("span", "pill " + cls, todo.status || "pending"), document.createTextNode(" " + todo.content));
    wrap.append(item({ title, sub: todo.id }));
  }
}

// The owner on a routine row is a dropdown that reassigns in place.

// It used to be dead text printing the internal id — a string that appears nowhere else in the UI and
// identifies nobody. Handing the work to someone else meant a sideways trick: have the other member
// save a routine under the very same name and let "same name overwrites" displace the original. That
// asked the user to know an implementation detail and to restate the prompt word for word, which this
// row truncates at 90 characters anyway. Whose job this is was always going to change; it belongs in a
// control you can click.
function routineSub(r, when) {
  const sub = el("div", "sub");
  const sel = document.createElement("select");


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
    wrap.append(el("div", "empty", t('Tell a member "every … do …" to create a routine')));
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

// Sidebar footer ----

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

// Former members ----

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
            hint: t("This member's memory and everything in its workspace will be deleted permanently. This cannot be undone."),
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
      : t("This member has no saved memory.")));
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
