"use strict";

// DOM helpers and the shared ask dialog.
// Loaded in the order declared by renderer/index.html.

const $ = (id) => document.getElementById(id);
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

let toastTimer = null;
function toast(msg) {
  const n = $("toast");
  n.textContent = msg;
  n.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { n.hidden = true; }, 3600);
}

let askResolver = null;
function finishAsk(v) {
  const r = askResolver;
  askResolver = null;
  if (r) r(v);
}

// Passing check adds a checkbox, and confirming then resolves to { checked } while cancelling stays
// false — truthy still means "the user said yes", so no existing if (ok) call site has to change.
function ask({ title, hint = "", ok, cancel, danger = false, field = false, fieldLabel = "", placeholder = "", check = null }) {
  return new Promise((resolve) => {
    askResolver = resolve;
    $("askTitle").textContent = title;
    $("askHint").textContent = hint;
    $("askHint").hidden = !hint;
    $("askFieldWrap").hidden = !field;
    $("askField").value = "";
    $("askField").placeholder = placeholder;
    $("askFieldLabel").textContent = fieldLabel;
    $("askCheckWrap").hidden = !check;
    $("askCheck").checked = !!(check && check.checked);
    $("askCheckLabel").textContent = check ? check.label : "";
    $("askOk").textContent = ok || t("OK");
    $("askCancel").textContent = cancel || t("Cancel");
    $("askOk").classList.toggle("danger", !!danger);
    $("askModal").showModal();
    if (field) $("askField").focus();
  });
}

$("askForm").addEventListener("submit", (e) => {
  const fieldOn = !$("askFieldWrap").hidden;
  if (e.submitter && e.submitter.value === "cancel") {
    finishAsk(fieldOn ? null : false);
    return;
  }
  e.preventDefault();
  const v = fieldOn ? $("askField").value
    : ($("askCheckWrap").hidden ? true : { checked: $("askCheck").checked });
  finishAsk(v);
  $("askModal").close();
});
$("askModal").addEventListener("close", () => {
  finishAsk(!$("askFieldWrap").hidden ? null : false);
});
