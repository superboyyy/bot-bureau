"use strict";

// Resizable workbench layout.
// Loaded in the order declared by renderer/index.html.

function applySideSplit(frac) {
  const body = $("sideBody");
  const split = $("sideSplit");
  if (!body || !split) return;
  const usable = body.clientHeight - split.offsetHeight;
  if (usable <= 0) return;
  const min = 72;
  const px = Math.min(usable - min, Math.max(min, frac * usable));
  const next = px / usable;
  body.style.setProperty("--side-chat", (next * 100) + "%");
  localStorage.setItem("botbureau_side_split", String(next));
}

function wireSideSplit() {
  const split = $("sideSplit");
  const body = $("sideBody");
  if (!split || !body) return;
  const saved = parseFloat(localStorage.getItem("botbureau_side_split"));
  if (saved > 0 && saved < 1) applySideSplit(saved);
  split.onpointerdown = (e) => {
    e.preventDefault();
    split.classList.add("dragging");
    split.setPointerCapture(e.pointerId);
    const move = (ev) => {
      const r = body.getBoundingClientRect();
      applySideSplit((ev.clientY - r.top) / r.height);
    };
    const up = () => {
      split.classList.remove("dragging");
      split.removeEventListener("pointermove", move);
      split.removeEventListener("pointerup", up);
    };
    split.addEventListener("pointermove", move);
    split.addEventListener("pointerup", up);
  };
}
