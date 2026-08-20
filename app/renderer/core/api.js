"use strict";

// Backend requests, state refresh, and SSE connection.
// Loaded in the order declared by renderer/index.html.

// Data ----

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
  mergeEventBatch(s.events || []);
  if (events.length) lastId = Math.max(lastId, events[events.length - 1].id);
  // A state refresh updates approvals, settings and the sidebar index. It must not throw away pages
  // that the user already loaded in the message pane, nor yank a reader back to the bottom.
  renderAll(false);
}

// The backend's state payload is only the recent event window. Keep it as a refresh source while
// retaining older pages fetched from /api/history. Event ids make the merge idempotent across SSE,
// reconnects and overlapping history requests.
function mergeEventBatch(batch) {
  if (!batch || !batch.length) return;
  const byID = new Map(events.map((ev) => [String(ev.id), ev]));
  for (const ev of batch) byID.set(String(ev.id), ev);
  events = [...byID.values()].sort((a, b) => Number(a.id) - Number(b.id));
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

// The stream authenticates with a short-lived ticket rather than the pairing code itself.

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
    mergeEventBatch([ev]);
    rememberConversation(ev);
    if (ev.kind === "msg" || ev.kind === "tool") {

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
      // Bot enable_connector (OAuth): open the browser Authorize flow without visiting Plugins.
      if (ev.mcp_oauth && typeof startMCPOAuth === "function") {
        startMCPOAuth(String(ev.mcp_oauth));
      }
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

// Rendering ----
