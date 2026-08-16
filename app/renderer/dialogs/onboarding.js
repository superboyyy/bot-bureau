"use strict";

// First-run onboarding.
// Loaded in the order declared by renderer/index.html.

// Boot ----

// syncLocaleToEngine tells the engine which language the UI is speaking right now.

// The language is kept in two places: the UI's choice in browser storage, the engine's in
// data/settings.json, and "follow the system" is resolved once on each side. What goes over is the
// already-resolved zh/en rather than "auto" — the engine resolves that by reading LANG/LC_ALL, which a
// GUI child process launched from an icon does not have, so "auto" only ever becomes English there.
async function syncLocaleToEngine() {
  try {
    await api("/api/settings", { locale: LOCALE });
  } catch (err) {
    console.log("sync locale to engine failed: " + err.message);
  }
}

// Language switch: refresh the UI immediately and sync the preference to the engine
// (it drives backend messages and the bots' prompt language)
$("themeSel").onchange = (e) => setThemePref(e.target.value);
$("langSel").onchange = async (e) => {
  const pref = e.target.value;
  setLocalePref(pref);
  applyStatic();
  renderAll();
  renderTG();
  renderPairInfo();
  await syncLocaleToEngine();
  try {

    // The catalog's labels and notes are produced by the engine in its locale, so refetch after switching
    await loadProviderCatalog();
    await loadPermLevels();
    renderPermLevels();
    botPicker.repaint();
    if (onboardPicker) onboardPicker.repaint();
  } catch (err) {
    console.log("reload engine-supplied labels failed: " + err.message);
  }
};

// ---- First-run onboarding: establish "you are the boss", tour what is here, then hire the first member ----

// A fresh install opens empty: no bots, no groups. An empty window cannot explain what the product is,
// so onboarding first fixes the frame — the user runs the bureau, the bots are the staff they hire —
// and then points at where things are created and managed. Only the last step lands on "create your
// first bot", and it can be skipped: nobody undecided should be forced to configure a model first.

let onboardStep = 0;
let onboardPicker = null;

// One drawing per step, built from the avatar and icon vocabulary already on screen so the user can
// recognize what it points at.
function onboardArt(kind) {
  const wrap = el("div", "");
  wrap.style.display = "flex";
  wrap.style.alignItems = "center";
  wrap.style.gap = "10px";
  if (kind === "boss") {
    for (const n of ["chief", "scout", "coder"]) wrap.append(avatar(n, 46));
  } else if (kind === "tour") {
    wrap.append(avatar("one", 40), groupAvatarFor("group", 46), avatar("two", 40));
  } else {
    wrap.append(avatar("newcomer", 62));
  }
  return wrap;
}

function tourRow(iconPath, name, desc) {
  const row = el("div", "row");
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("class", "glyph");
  svg.setAttribute("width", "17"); svg.setAttribute("height", "17");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.6");
  svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round");
  const path = document.createElementNS(NS_SVG, "path");
  path.setAttribute("d", iconPath);
  svg.append(path);
  const txt = el("div", "");
  txt.append(el("div", "name", name), el("div", "desc", desc));
  row.append(svg, txt);
  return row;
}

const ICON_PLUS = "M12 5v14M5 12h14";
const ICON_PEOPLE = "M16 19v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 17.5V19M10 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Zm10 8v-1.5a3.5 3.5 0 0 0-2.6-3.4M15.5 4.2a3.5 3.5 0 0 1 0 6.6";
const ICON_GEAR = "M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4Zm7.4-.2a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.8 1.2V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-2.8-1.2l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.6 1.6 0 0 0 3.1 14H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.2-2.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.6 1.6 0 0 0 10 3.1V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 2.8 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0 1.2 2.8H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1Z";

function onboardSteps() {
  return [
    {
      art: "boss",
      title: t("You run this Bot Bureau"),
      lede: t("The bots are your staff: each one keeps its own workspace, its own memory and its own model, and they work while you are away. You decide who to hire and what they are allowed to do on their own."),
    },
    {
      art: "tour",
      title: t("What you can do here"),
      lede: t("You can return to any of these features at any time."),
      rows: [
        [ICON_PLUS, t("Create a bot"), t("Click the ＋ at the top of the sidebar. Give the new member a role and persona, choose a model, and it joins the team.")],
        [ICON_PEOPLE, t("Create a group chat"), t("Let several members collaborate on one task. Mention a member by name to assign the work; the others follow along for context.")],
        [ICON_GEAR, t("Manage members"), t("Hover over a conversation and click the pencil to change a member's name, avatar, model, or permissions. You can edit group chats the same way.")],
      ],
    },
    {
      art: "hire",
      title: t("Hire your first member"),
      lede: t("Pick a vendor and a model, and this member is ready to work. You can skip this step and do it later — the ＋ in the sidebar is always available."),
      hire: true,
    },
  ];
}

function paintOnboard() {
  const steps = onboardSteps();
  const step = steps[onboardStep];
  $("onboardTitle").textContent = step.title;
  $("onboardLede").textContent = step.lede;
  $("onboardArt").replaceChildren(onboardArt(step.art));
  $("onboardErr").textContent = "";

  const body = $("onboardBody");
  body.replaceChildren();
  if (step.rows) {
    const tour = el("div", "onboard-tour");
    for (const [icon, name, desc] of step.rows) tour.append(tourRow(icon, name, desc));
    body.append(tour);
  }
  if (step.hire) {
    const holder = el("div", "");
    holder.style.textAlign = "left";
    body.append(holder);
    onboardPicker = createModelPicker(holder);
    onboardPicker.setConfig({});
  }

  $("onboardDots").replaceChildren(...steps.map((_, i) => {
    const d = document.createElement("i");
    if (i === onboardStep) d.className = "on";
    return d;
  }));
  $("onboardBack").hidden = onboardStep === 0;
  $("onboardNext").textContent = step.hire ? t("Create") : t("Next");
  $("onboardSkip").textContent = step.hire ? t("Skip for now") : t("Skip");
}

function maybeOnboard() {
  if (localStorage.getItem("botbureau_onboarded") === "1") return;

  // Existing bots mean this is not a fresh install (an upgrade), so stay out of the way
  if (state.bots.length) {
    localStorage.setItem("botbureau_onboarded", "1");
    return;
  }
  onboardStep = 0;
  paintOnboard();
  $("onboardModal").showModal();
}

function finishOnboard() {
  localStorage.setItem("botbureau_onboarded", "1");
  $("onboardModal").close();
}

$("onboardSkip").onclick = finishOnboard;
$("onboardBack").onclick = () => { if (onboardStep > 0) { onboardStep--; paintOnboard(); } };
$("onboardNext").onclick = async () => {
  const steps = onboardSteps();
  if (!steps[onboardStep].hire) {
    onboardStep++;
    paintOnboard();
    return;
  }
  const picked = onboardPicker.getConfig();
  if (!picked.provider) {
    $("onboardErr").textContent = t("Pick a vendor first");
    return;
  }
  if (picked.provider !== "fake" && !picked.model) {
    $("onboardErr").textContent = t("No model selected yet");
    return;
  }
  try {
    const pending = onboardPicker.pendingKey();
    if (pending) {
      await api("/api/keys", pending);
      onboardPicker.clearKeyInput();
    }

    // The first member is simply "assistant": a generalist the user can rename and reshape later
    await api("/api/bots", {
      name: "assistant",
      role: t("Assistant"),
      description: t("A generalist member for research, writing, coding, and everyday tasks."),
      ...picked,
    });
    finishOnboard();
  } catch (err) {
    $("onboardErr").textContent = err.message;
  }
};
