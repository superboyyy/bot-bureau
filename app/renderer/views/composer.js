"use strict";

// Composer attachments and context menu support.
// Loaded in the order declared by renderer/index.html.

// Return has nothing left to do but one POST.
let pending = [];

function renderAttachments() {
  const tray = $("attachTray");
  tray.replaceChildren();
  tray.hidden = pending.length === 0;
  for (const p of pending) {
    const chip = el("div", "chip" + (p.preview ? " has-thumb" : ""));
    if (p.preview) {
      const img = document.createElement("img");
      img.src = p.preview;
      img.alt = "";
      chip.append(img);
    }
    const meta = el("div", "meta");
    meta.append(el("div", "nm", p.name), el("div", "sz", humanSize(p.size)));
    chip.append(meta);
    const x = el("button", "x", "✕");
    x.type = "button";
    x.title = t("Remove");
    x.onclick = () => {
      pending = pending.filter((q) => q !== p);
      renderAttachments();
    };
    chip.append(x);
    tray.append(chip);
  }
}

function clearAttachments() {
  pending = [];
  renderAttachments();
}

function humanSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + " KB";
  return n + " B";
}

// addFiles takes in a batch of files. The limits are the same numbers the backend enforces
// (engine/attach.go); checking here first is only so the user finds out now, rather than after 25MB has
// gone up the wire and come back refused.
const MAX_FILES = 8;
const MAX_ONE = 10 * 1024 * 1024;
const MAX_ALL = 25 * 1024 * 1024;

async function addFiles(list) {
  const files = [...list].filter((f) => f && f.size >= 0);
  for (const f of files) {
    if (pending.length >= MAX_FILES) {
      toast(t("A message can include at most %s files", MAX_FILES));
      break;
    }
    if (f.size > MAX_ONE) {
      toast(t("%s is too large (limit %s)", f.name || t("This file"), humanSize(MAX_ONE)));
      continue;
    }
    if (pending.reduce((n, p) => n + p.size, 0) + f.size > MAX_ALL) {
      toast(t("These files exceed the size limit for a single message"));
      break;
    }
    try {
      pending.push(await readFile(f));
      renderAttachments();
    } catch (err) {
      toast(t("Could not read %s", f.name || ""));
    }
  }
}

function readFile(f) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onerror = () => reject(r.error);
    r.onload = () => {
      const url = String(r.result);
      const data = url.slice(url.indexOf(",") + 1);
      const mime = f.type || "";
      resolve({

        // A pasted screenshot arrives as a File with no name; give it one that reads as something
        name: f.name || (mime.startsWith("image/") ? t("pasted image") + guessExt(mime) : t("pasted file")),
        mime, size: f.size, data,
        preview: mime.startsWith("image/") ? url : "",
      });
    };
    r.readAsDataURL(f);
  });
}

const guessExt = (mime) => ({ "image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp" })[mime] || ".png";

$("attachBtn").onclick = () => $("attachFile").click();
$("attachFile").onchange = (e) => {
  addFiles(e.target.files);
  e.target.value = ""; // picking the same file twice must still fire
};

// Paste: taken over only when the clipboard really carries files, so pasting text still just pastes text
$("input").addEventListener("paste", (e) => {
  const files = [...(e.clipboardData ? e.clipboardData.files : [])];
  if (!files.length) return;
  e.preventDefault();
  addFiles(files);
});

// Drag and drop over the whole window rather than only the field. A visible state while dragging, or
// nobody knows what letting go will do.
let dragDepth = 0;
window.addEventListener("dragenter", (e) => {
  if (![...(e.dataTransfer ? e.dataTransfer.types : [])].includes("Files")) return;
  e.preventDefault();
  if (++dragDepth === 1) document.body.classList.add("dropping");
});
window.addEventListener("dragover", (e) => {
  if ([...(e.dataTransfer ? e.dataTransfer.types : [])].includes("Files")) e.preventDefault();
});
window.addEventListener("dragleave", () => {
  if (--dragDepth <= 0) { dragDepth = 0; document.body.classList.remove("dropping"); }
});
window.addEventListener("drop", (e) => {
  if (!e.dataTransfer || !e.dataTransfer.files.length) return;
  e.preventDefault();
  dragDepth = 0;
  document.body.classList.remove("dropping");
  addFiles(e.dataTransfer.files);
});
$("input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    $("composer").requestSubmit();
  }
});
// The field grows with its content, then scrolls
$("input").addEventListener("input", (e) => {
  e.target.style.height = "auto";
  e.target.style.height = Math.min(e.target.scrollHeight, 160) + "px";
});

function setPlusBtn() {
  const btn = $("plusBtn");
  const mention = isGroupChatId(current);
  btn.title = mention ? t("Mention a member") : t("Jump to another chat");
  btn.setAttribute("data-i18n-title", mention ? "Mention a member" : "Jump to another conversation");
  btn.setAttribute("data-en-title", mention ? "Mention a member" : "Jump to another chat");
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("width", "18");
  svg.setAttribute("height", "18");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.8");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  if (mention) {
    const c = document.createElementNS(NS_SVG, "circle");
    c.setAttribute("cx", "12"); c.setAttribute("cy", "12"); c.setAttribute("r", "4");
    const p = document.createElementNS(NS_SVG, "path");
    p.setAttribute("d", "M16 12v1.6a2.4 2.4 0 0 0 4.8 0V12a8.8 8.8 0 1 0-3.5 7");
    svg.append(c, p);
  } else {
    const p = document.createElementNS(NS_SVG, "path");
    p.setAttribute("d", "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2");
    const c = document.createElementNS(NS_SVG, "circle");
    c.setAttribute("cx", "9"); c.setAttribute("cy", "7"); c.setAttribute("r", "4");
    const p2 = document.createElementNS(NS_SVG, "path");
    p2.setAttribute("d", "M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75");
    svg.append(p, c, p2);
  }
  btn.replaceChildren(svg);
}
