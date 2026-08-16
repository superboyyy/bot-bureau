"use strict";

// UI i18n: user-facing text is written in English in the source; other languages live in locales/*.js
// keyed by that English text. Static text is tagged with data-i18n / data-i18n-ph / data-i18n-title,
// whose values are likewise the English source. A missing entry falls back to English, so an
// untranslated string never renders as a blank.
// Resolution order: explicit setting (localStorage) > system language (Electron locale / navigator.language) > en.

const I18N_KEY = "botbureau_locale";

function systemLocale() {
  const sys = (new URLSearchParams(location.search).get("locale") || navigator.language || "en").toLowerCase();
  return sys.startsWith("zh") ? "zh" : "en";
}

// "auto" | "zh" | "en"
function localePref() {
  const v = localStorage.getItem(I18N_KEY);
  return v === "zh" || v === "en" ? v : "auto";
}

let LOCALE = localePref() === "auto" ? systemLocale() : localePref();

function i18nTable() {
  return (window.__i18n && window.__i18n[LOCALE]) || null;
}

// t returns the text for the current locale, falling back to the English source.
// Text with values uses %s placeholders rather than a pre-interpolated template: an interpolated
// string differs every time and would never match a table entry.
function t(en, ...args) {
  const table = i18nTable();
  let out = (table && table[en]) || en;
  for (const a of args) out = out.replace("%s", String(a));
  return out;
}

// sentences joins a few sentences into one paragraph, with the gap decided by the language: English
// wants a space after the full stop and Chinese wants none — the half-width space in

// work too, at the cost of copying the same long sentence again for every extra variant.
function sentences(...parts) {
  return parts.filter(Boolean).join(LOCALE === "zh" ? "" : " ");
}

function setLocalePref(pref) {
  if (pref === "auto") localStorage.removeItem(I18N_KEY);
  else localStorage.setItem(I18N_KEY, pref);
  LOCALE = pref === "auto" ? systemLocale() : pref;
}

// applyStatic refreshes static text carrying data-i18n* for the current locale (placeholder and title included).
function applyStatic(root = document) {
  root.querySelectorAll("[data-i18n]").forEach((el) => { el.textContent = t(el.dataset.i18n); });
  root.querySelectorAll("[data-i18n-ph]").forEach((el) => { el.placeholder = t(el.dataset.i18nPh); });
  root.querySelectorAll("[data-i18n-title]").forEach((el) => { el.title = t(el.dataset.i18nTitle); });
  document.documentElement.lang = LOCALE;
}

// The renderer still loads this file as a classic script. Expose only the small public surface used by
// tests, keeping the production page's existing global functions and script order intact.
if (typeof window !== "undefined") {
  window.__botBureauI18n = { t, sentences, localePref, setLocalePref, applyStatic, systemLocale };
}
