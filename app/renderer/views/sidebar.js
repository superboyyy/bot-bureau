"use strict";

// Conversation list, header, and sidebar rendering.
// Loaded in the order declared by renderer/index.html.

function renderAll(forceBottom = true) {
  renderChatList();
  renderHeader();
  renderMsgs(forceBottom);
  renderTasks();
  renderRoutines();
  renderKeysList();
  renderMCPList();
  renderMCPCatalog();
  renderSkillList();
  renderBundleList();
  renderFooter();
}

// One card item: title, subtitle, mono body, actions
function item({ title, sub, code, actions }) {
  const node = el("div", "item");
  if (title) node.append(title instanceof Node ? title : el("div", "title", title));
  if (sub) node.append(sub instanceof Node ? sub : el("div", "sub", sub));
  if (code) node.append(el("div", "code", code));
  if (actions && actions.length) {
    const bar = el("div", "actions");
    for (const [label, cls, fn] of actions) {
      const b = el("button", cls, label);

      // The button is handed to the callback: a long action (installing a plugin) turns itself into a
      // progress label in place
      b.onclick = (e) => { e.preventDefault(); fn(b); };
      bar.append(b);
    }
    node.append(bar);
  }
  return node;
}

// Conversation list ----

// Last activity per conversation comes from the sidebar index, not from the chat pane's loaded pages.
function lastByChat() {
  const map = {};
  for (const ev of state.conversations || []) {
    if (ev.chat) map[ev.chat] = ev;
  }
  return map;
}

// SSE carries the full event for the open chat. Keep a small projection in the independent sidebar
// index as well, so a newly arrived message updates the list without making it depend on chat history.
function rememberConversation(ev) {
  if (!ev.chat || !["msg", "tool", "system"].includes(ev.kind)) return;
  const summary = {
    id: ev.id, ts: ev.ts, kind: ev.kind, chat: ev.chat,
    source: ev.source, text: ev.text || "", has_files: !!(ev.files && ev.files.length),
  };
  const list = state.conversations || [];
  const index = list.findIndex((item) => item.chat === ev.chat);
  if (index >= 0) {
    if (Number(list[index].id || 0) > Number(ev.id || 0)) return;
    list[index] = summary;
  } else {
    list.push(summary);
  }
  state.conversations = list;
}

const pendingIn = (chat) => (state.approvals || []).filter((a) => a.chat === chat).length;

function previewOf(ev) {
  if (!ev) return "";
  if (ev.kind === "tool") return `${titleOf(ev.source)} · ${ev.text}`;
  if (ev.kind === "msg" && isGroupChatId(ev.chat) && ev.source !== "user") return `${titleOf(ev.source)}: ${ev.text}`;
  return ev.text || "";
}

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






  // Settings and delete move into a right-click menu instead of buttons living in the row. Those
  // buttons only appeared on hover, and they appeared exactly on top of the preview text and the
  // timestamp — so much so that making room for them meant fading the time and the unread badge to
  // opacity:0 while hovering. Which is to say: reading a row was impossible precisely while pointing at
  // it. Text in a menu also says what each action does better than two small glyphs ever did.
  // Nothing is lost in discoverability: clicking the chat header opens the same settings (renderHeader).
  const menu = [
    [pinned ? t("Unpin") : t("Pin to top"), "", () => setPinned(id, !pinned)],
    [t("New conversation"), "", async () => {
      const ok = await ask({
        title: t("Start a new conversation?"),
        hint: t("The model starts fresh. Messages already in this chat stay on screen, and MEMORY.md is kept."),
        ok: t("New conversation"),
      });
      if (!ok) return;
      api("/api/session/reset", { chat: id }).catch((err) => toast(err.message));
    }],
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




      // The user decides then and there whether the files go, instead of being told after the fact that
      // they are somewhere under data/ — an id is a random string that never surfaces, so even in that
      // folder nobody could tell which directory was whose. "Kept" only means something alongside where
      // it can be seen afterwards.
      const routines = (state.routines || []).filter((r) => r.bot === bot).length;
      const hint = sentences(
        t("This member's memory and work files are kept; open Settings › Former members to read or delete them."),
        routines === 1 ? t("The routine assigned to this member stops too.") : "",
        routines > 1 ? t("The %s routines assigned to this member stop too.", routines) : "",
      );
      const res = await ask({
        title: t("Remove %s", name),
        hint,
        ok: t("Remove"),
        danger: true,
        check: { label: t("Also delete this member's memory and work files"), checked: false },
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
      : t("%s members · mention a member by name to assign work", n)));
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
  // Clicking the avatar or name opens this conversation's settings
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
    ? t("Message the team… mention a member by name to assign work")
    : t("Message %s", titleOf(current.slice(3)));
}

// Which bots are working in the current conversation. Shared by the header's stop button and the
// message stream's typing bubbles.
function busyInChat() {
  return isGroupChatId(current)
    ? state.bots.filter((b) => b.busy && inGroupOf(current, b.name)).map((b) => b.name)
    : state.bots.filter((b) => b.busy && current === "dm:" + b.name).map((b) => b.name);
}
