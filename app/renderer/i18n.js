"use strict";
// 界面国际化：源码里的用户可见文案一律写英文原文，其它语言放在 locales/*.js 里按原文查表。
// 静态文案用 data-i18n / data-i18n-ph / data-i18n-title 属性标注，值同样是英文原文。
// 查不到就原样返回英文，漏翻译不会变成空白界面。
// 语言取值：设置里的显式选择（localStorage）> 系统语言（Electron locale / navigator.language）> en。
//
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

// t 按当前语言返回文案；查不到就返回英文原文。
// 带值的文案用 %s 占位而不是模板串拼好再查——插值后的字符串每次都不一样，永远查不中表。
//
// t returns the text for the current locale, falling back to the English source.
// Text with values uses %s placeholders rather than a pre-interpolated template: an interpolated
// string differs every time and would never match a table entry.
function t(en, ...args) {
  const table = i18nTable();
  let out = (table && table[en]) || en;
  for (const a of args) out = out.replace("%s", String(a));
  return out;
}

// sentences 把几句话接成一段。分句的间隔由语言决定：英文句号后要空一格，中文不空——
// 拼出来的「……删除。 指派给 ta 的……」那个半角空格看着就像打错了。
// 整段只写成一条译文也行，但那样每多一种句子组合就得把同一段长句再抄一遍。
//
// sentences joins a few sentences into one paragraph, with the gap decided by the language: English
// wants a space after the full stop and Chinese wants none — the half-width space in
// "……删除。 指派给 ta 的……" reads as a typo. Writing each combination out as its own entry would
// work too, at the cost of copying the same long sentence again for every extra variant.
function sentences(...parts) {
  return parts.filter(Boolean).join(LOCALE === "zh" ? "" : " ");
}

function setLocalePref(pref) {
  if (pref === "auto") localStorage.removeItem(I18N_KEY);
  else localStorage.setItem(I18N_KEY, pref);
  LOCALE = pref === "auto" ? systemLocale() : pref;
}

// applyStatic 按当前语言刷新带 data-i18n* 的静态文案（含 placeholder 与 title）。
// applyStatic refreshes static text carrying data-i18n* for the current locale (placeholder and title included).
function applyStatic(root = document) {
  root.querySelectorAll("[data-i18n]").forEach((el) => { el.textContent = t(el.dataset.i18n); });
  root.querySelectorAll("[data-i18n-ph]").forEach((el) => { el.placeholder = t(el.dataset.i18nPh); });
  root.querySelectorAll("[data-i18n-title]").forEach((el) => { el.title = t(el.dataset.i18nTitle); });
  document.documentElement.lang = LOCALE;
}
