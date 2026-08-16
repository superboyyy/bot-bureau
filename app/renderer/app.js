"use strict";

// Theme, discovery hint, and application boot.
// Loaded last by renderer/index.html.

// Appearance: follow the system, light, or dark. An explicit choice is stamped on <html data-theme>
// and beats prefers-color-scheme. It lives in localStorage rather than the engine: several devices
// may share one engine, and how bright a screen is stays that device's own business.
const THEME_KEY = "botbureau_theme";

function themePref() {
  const v = localStorage.getItem(THEME_KEY);
  return v === "light" || v === "dark" ? v : "auto";
}

function applyTheme() {
  const pref = themePref();
  if (pref === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", pref);
}

function setThemePref(pref) {
  if (pref === "auto") localStorage.removeItem(THEME_KEY);
  else localStorage.setItem(THEME_KEY, pref);
  applyTheme();
}

// Another device on the LAN: a hint sprouts directly above the settings button rather than standing
// in the way of startup.

// Multi-device is a bonus, not a turnstile. A dialog on open followed by a pairing-code prompt makes
// anyone who just wants the local setup answer two questions first. This only offers; pairing happens
// on click. Dismissing it is remembered, so the same device never asks twice.
const PAIR_DISMISSED = "botbureau_pair_dismissed";

function dismissedEngines() {
  try {
    return JSON.parse(localStorage.getItem(PAIR_DISMISSED) || "[]");
  } catch {
    return [];
  }
}

function showPairHint(engines) {
  const done = dismissedEngines();
  const e = (engines || []).find((x) => x && x.url && !done.includes(x.url));
  if (!e) return;
  const box = $("pairHint");
  box.replaceChildren();
  box.hidden = false;

  const head = el("div", "pair-head");
  head.append(el("span", "dot"), el("span", "", t("Found %s on your network", e.name || e.url)));
  const desc = el("div", "pair-desc", t("Pair with this engine to share one team across devices — bots, chats, and approvals all stay on that machine."));
  const acts = el("div", "pair-acts");

  const yes = el("button", "primary", t("Pair"));
  yes.type = "button";
  yes.onclick = async () => {
    if (!window.botBureauNative) return;
    box.hidden = true;

    // Same connect path as Settings: the main process reloads the window on success, and the first
    // time asks for the pairing code
    const r = await window.botBureauNative.connectTo(e.url);
    if (r && !r.ok) {
      toast(r.error);
      box.hidden = false;
    }
  };

  const no = el("button", "", t("Not now"));
  no.type = "button";
  no.onclick = () => {
    box.hidden = true;
    localStorage.setItem(PAIR_DISMISSED, JSON.stringify([...done, e.url]));
  };

  acts.append(yes, no);
  box.append(head, desc, acts);
}

function boot() {
  if (!/Mac/i.test(navigator.userAgent)) document.documentElement.classList.add("win-controls");
  applyTheme();
  applyStatic();
  if (window.botBureauNative && window.botBureauNative.onEnginesFound) {
    window.botBureauNative.onEnginesFound(showPairHint);
  }
  $("langSel").value = localePref();
  $("themeSel").value = themePref();
  wireSideSplit();




  // The language goes first: the catalog, the permission tiers and the reasoning-effort labels are all
  // produced by the engine in its own language, so asking earlier brings back the previous one. The
  // sync used to happen only when the user touched the language dropdown, which meant anyone who never
  // touched it — the UI language having been settled at install time — never hit it at all, and read a
  // Chinese UI with the engine's text still in English.
  syncLocaleToEngine()

    // The vendor catalog has to land first or the model picker has nothing to render
    .then(() => Promise.all([loadProviderCatalog(), loadPermLevels()]))
    .then(refetchState)
    .then(() => {
      connectSSE();
      maybeOnboard();
      console.log(`renderer ready: bots=${state.bots.length} default=${state.default_bot} remote=${REMOTE}`);
    })
    .catch((err) => {




      // Entering a pairing code is only something the user can act on when the engine is remote.
      // For the local engine the token is read straight out of data/token by the main process and handed
      // to the renderer, so a 401 here means the internal state is wrong (two engines sharing one data
      // directory, say), not that the user forgot to type something — and asking someone to pair with
      // their own machine just sends them hunting through Settings for a code they should never need.
      if (err.status === 401 && REMOTE) {
        $("tokenErr").textContent = TOKEN ? t("Wrong pairing code — try again") : "";
        $("tokenModal").showModal();
        return;
      }
      console.error("init failed: " + err.message); // console output is English throughout
      document.body.textContent = t("Failed to connect to the backend: ") + err.message;
    });
}
boot();
