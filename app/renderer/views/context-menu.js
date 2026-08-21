"use strict";

// Context menus, Telegram, and pairing details.
// Loaded in the order declared by renderer/index.html.

let pop = null;
function closePop() {
  if (pop) { pop.remove(); pop = null; }
}
document.addEventListener("click", (e) => {
  if (pop && !pop.contains(e.target) && e.target.closest("#plusBtn") === null) closePop();
});

// Esc backs out one layer at a time: the popover first, and the pending quotation once it is gone
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (pop) closePop();
  else if (replyTo) clearReply();
});

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
  if (isGroup && entries.length) {
    const allBtn = el("button", "");
    allBtn.type = "button";
    allBtn.append(el("span", "", t("Everyone")));
    allBtn.append(el("span", "meta", t("everyone answers")));
    allBtn.onclick = () => {
      closePop();
      const input = $("input");
      input.value = "@all " + input.value;
      input.focus();
    };
    pop.append(allBtn);
  }
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

// Settings: API Keys ----

$("connectRemoteBtn").onclick = async () => {
  if (!window.botBureauNative) return;
  $("remoteErr").textContent = t("Connecting…");
  const r = await window.botBureauNative.connectTo($("raddr").value);

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
