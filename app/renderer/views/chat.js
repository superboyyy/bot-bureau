"use strict";

// Message stream, tools, approvals, and history loading.
// Loaded in the order declared by renderer/index.html.

// Message stream ----

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

// The attachments a message carries: images laid out as images, everything else as a file row.

// The event holds metadata only and the bytes come from /api/file/<id> — base64 inside the event log
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

// fileURL turns an attachment into an address an <img> can take directly.

// /api/file/<id> cannot go into a src as-is: like every other endpoint it sits behind the pairing code,
// and an <img> cannot carry an Authorization header. Putting the code in the query string would make
// the picture appear — and write the code into every log and every history file along with it. So it is
// fetched properly and handed over as a blob address.

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
  void target.offsetWidth; // restart the animation so a second click flashes again
  target.classList.add("flash");
}

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
  if (ap.kind === "plan") {
    box.append(el("div", "head", t("%s submitted a plan", titleOf(ap.bot))));
    box.append(el("div", "code", ap.title || ap.action || ""));
    if (ap.body) box.append(el("pre", "diff", ap.body));
  } else {
    box.append(el("div", "head", t("%s requests permission to run", titleOf(ap.bot))));
    box.append(el("div", "code", ap.action));
    if (ap.diff) {
      const pre = el("pre", "diff", ap.diff);
      box.append(pre);
    }
  }
  const acts = el("div", "acts");
  const yes = el("button", "yes", ap.kind === "plan" ? t("Accept plan") : t("Approve"));
  yes.type = "button";
  yes.onclick = () => api("/api/approve", { id: ap.id, approved: true }).catch((e) => toast(e.message));
  const no = el("button", "no", ap.kind === "plan" ? t("Reject plan") : t("Reject"));
  no.type = "button";
  no.onclick = async () => {
    const reason = await ask({
      title: ap.kind === "plan" ? t("Reject this plan") : t("Reject this action"),
      hint: t("You can explain why — the member will see your explanation."),
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

  // "Approve and remember this directory." This is the one moment when handing a directory to
  // a member is unambiguous: the user says "bot-bureau" and the engine cannot tell where that is, while
  // the card in front of them spells the full path out. The button prints it verbatim, so the directory
  // they grant is the directory they can see.

  // It sits below the two buttons and is deliberately quieter than either: this click approves not just
  // the command in view but everything that directory will see afterwards. Making it big or primary
  // would be using visual weight to decide on the user's behalf.
  if (ap.dir && ap.kind !== "plan") {
    const grant = el("button", "grant");
    grant.type = "button";
    grant.title = t("This member will read, write and run commands in this directory without asking. Remove it in its settings.");
    grant.append(el("span", "lbl", t("Approve and remember")), el("span", "code", ap.dir));
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

const SEP_GAP = 15 * 60; // a gap this long gets a time separator

function renderMsgs(forceBottom) {
  const box = $("msgs");
  if (!hasAnyChat()) {
    box.replaceChildren();
    const blank = el("div", "blank");
    blank.append(
      el("div", "blank-title", t("Nobody works here yet")),
      el("div", "blank-desc", t("Click ＋ in the sidebar to create your first member, then start chatting here.")),
    );
    box.append(blank);
    return;
  }

  // Follow new messages only when already pinned to the bottom — never yank a user who is reading history
  const stick = forceBottom || box.scrollHeight - box.scrollTop - box.clientHeight < 80;




  // A redraw tears the list down and rebuilds it, taking the scroll position to zero with it. Pinned to
  // the bottom that goes unnoticed, since the end follows along anyway; while scrolling back it throws
  // the user to the bottom every time a member emits one more line of tool output — with a team that is
  // actually working, history cannot be read at all. So when not pinned, the position goes back where
  // it was.
  const keepTop = box.scrollTop;
  box.replaceChildren();
  let lastTs = 0;
  let lastSource = null;
  let buffer = [];   // buffered activity from elsewhere
  let bufferKey = "";
  let trace = [];    // buffered tool events, folded into one line
  let traceKey = "";
  let prevMsg = 0;

  const flushTrace = (live) => {
    if (!trace.length) return;
    box.append(traceNode(traceKey, trace, !!live));
    trace = [];
    lastSource = null;
    prevMsg = 0;
  };



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



      // A bot's quotation is attached automatically, and directly beneath what it quotes it says
      // nothing that the layout does not already say. It earns its place once a run of steps, someone
      // else's message, or a stretch of time has come between. A quotation the user picked always
      // stays: that was a deliberate act — in a group it also decides who the message is for — and it
      // must not look like it failed to register.
      const quoted = ev.source === "user" || ev.reply_to !== prevMsg;
      // Only the group chat needs speaker labels and avatars
      const node = msgNode(ev, isGroupChatId(current), runStart, quoted);
      if (runStart) node.classList.add("run-start");
      box.append(node);
      lastSource = ev.source;
      prevMsg = ev.id;
    }
  }
  flush();




  // Bots still working: the trace line is marked live and a typing bubble follows it. During a long
  // think this is the user's only real-time feedback; without it the window just looks stuck.
  const working = busyInChat();
  flushTrace(working.length > 0);
  for (const name of working) box.append(typingNode(name, isGroupChatId(current)));

  // Pending approvals sit at the end of the conversation
  for (const ap of state.approvals || []) {
    if (ap.chat === current) box.append(approvalNode(ap));
  }
  if (stick) {
    box.scrollTop = box.scrollHeight;
  } else {
    box.scrollTop = keepTop;
  }
}

// Scrolling back ----

// History only disappears when the user deletes the conversation, so scrolling up has to reach the
// start. /api/state carries the newest batch and older pages are fetched on demand (/api/history) and
// spliced on the front.

// There was no such route before: the UI had the 400 entries in state and nothing beyond, even with the
// messages still sitting on disk.
const reachedStart = {};   // conversation → the start has been reached
const loadingOlder = new Set();

// oldestShown is the id of the earliest entry in hand for a conversation: where the next page starts.
function oldestShown(chat) {
  let min = 0;
  for (const ev of events) {
    if (ev.chat === chat && (!min || ev.id < min)) min = ev.id;
  }
  return min;
}

async function loadOlder() {
  const chat = current;
  const viewSerial = chatViewSerial;
  if (!chat || loadingOlder.has(chat) || reachedStart[chat]) return;
  loadingOlder.add(chat);
  const box = $("msgs");

  // Anchor first: after splicing, put the viewport back on the line it was on, or the user is thrown
  const beforeH = box.scrollHeight;
  const beforeTop = box.scrollTop;
  try {
    const r = await api("/api/history?chat=" + encodeURIComponent(chat) +
      "&before=" + (oldestShown(chat) || 0) + "&limit=200");
    const older = r.events || [];
    if (!r.more) reachedStart[chat] = true;
    if (older.length) {
      const known = new Set(events.map((e) => String(e.id)));
      const fresh = older.filter((e) => !known.has(String(e.id)));
      if (fresh.length) {
        mergeEventBatch(fresh);
        // The request may have outlived a chat switch. The events are still useful when the user comes
        // back, but the old request must not redraw or reposition the new conversation's pane.
        if (current === chat && chatViewSerial === viewSerial) {
          renderMsgs();
          box.scrollTop = box.scrollHeight - beforeH + beforeTop;
        }
      }
    } else {
      reachedStart[chat] = true;
      if (current === chat && chatViewSerial === viewSerial) renderMsgs();
    }
  } catch (err) {

    // A failed fetch does not mark the start as reached: scrolling up again tries once more
    console.log("history unavailable: " + err.message);
  } finally {
    loadingOlder.delete(chat);
  }
}

$("msgs").addEventListener("scroll", (e) => {
  if (e.target.scrollTop < 240) loadOlder();
});

// Workbench ----
