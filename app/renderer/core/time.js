"use strict";

// Date and time formatting helpers.
// Loaded in the order declared by renderer/index.html.

// Time ----

const pad2 = (n) => String(n).padStart(2, "0");
const WEEK = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

function dayLabel(d) {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const days = Math.round((today - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
  if (days === 0) return t("Today");
  if (days === 1) return t("Yesterday");
  if (days < 7) return t(WEEK[d.getDay()]);
  return t("%s/%s", d.getMonth() + 1, d.getDate());
}

// The list shows the coarsest useful unit
function listTime(ts) {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  return dayLabel(d);
}

function sepTime(ts) {
  const d = new Date(ts * 1000);
  return `${dayLabel(d)} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}
