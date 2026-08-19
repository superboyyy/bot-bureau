"use strict";

// HTML window controls for Windows and Linux. macOS keeps the native traffic lights.
// Loaded in the order declared by renderer/index.html.

function htmlWindowChrome() {
  const v = new URLSearchParams(location.search).get("chrome");
  if (v === "html") return true;
  if (v === "native") return false;
  return typeof navigator !== "undefined" && !/Mac/i.test(navigator.userAgent || "");
}

function chromeText(en) {
  const fn = (window.__botBureauI18n && window.__botBureauI18n.t) || window.t;
  return typeof fn === "function" ? fn(en) : en;
}

function resolvedAppearance() {
  const pref = typeof window.themePref === "function" ? window.themePref() : "auto";
  if (pref === "light" || pref === "dark") return pref;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function syncWindowAppearance() {
  const native = window.botBureauNative;
  if (native && typeof native.setAppearance === "function") native.setAppearance(resolvedAppearance());
}

function setChromeMaximized(on) {
  const box = document.getElementById("winChrome");
  if (!box) return;
  box.classList.toggle("maximized", !!on);
  const btn = document.getElementById("winMax");
  if (!btn) return;
  const label = chromeText(on ? "Restore" : "Maximize");
  btn.title = label;
  btn.setAttribute("aria-label", label);
  btn.setAttribute("data-i18n-title", on ? "Restore" : "Maximize");
}

function wireWindowChrome() {
  const box = document.getElementById("winChrome");
  if (!box) return;
  if (!htmlWindowChrome()) {
    box.hidden = true;
    document.documentElement.classList.remove("win-controls");
    return;
  }
  document.documentElement.classList.add("win-controls");
  box.hidden = false;
  const api = window.botBureauNative && window.botBureauNative.windowControls;
  const min = document.getElementById("winMin");
  const max = document.getElementById("winMax");
  const close = document.getElementById("winClose");
  if (min) min.onclick = () => { if (api) api.minimize(); };
  if (max) max.onclick = () => { if (api) api.maximize(); };
  if (close) close.onclick = () => { if (api) api.close(); };
  if (api && typeof api.onMaximized === "function") api.onMaximized(setChromeMaximized);
  if (api && typeof api.isMaximized === "function") {
    Promise.resolve(api.isMaximized()).then(setChromeMaximized);
  }
}

if (typeof window !== "undefined") {
  window.__botBureauChrome = {
    htmlWindowChrome, resolvedAppearance, syncWindowAppearance,
    setChromeMaximized, wireWindowChrome,
  };
}
