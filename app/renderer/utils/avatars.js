"use strict";

// Avatar and identity rendering helpers.
// Loaded in the order declared by renderer/index.html.

// Avatars ----

// A round face with two eyes: the fill distinguishes identity while the silhouette stays uniform.
// On the near-black canvas these pale circles are the only bright elements, so every fill is
// high-luminance and low-saturation — no jarring blocks of color.
const FACES = ["#dcdce1", "#c9b9a6", "#a8bccf", "#b2c9a6", "#cfadbc", "#a3c9c1", "#bcb2d9"];
const NS_SVG = "http://www.w3.org/2000/svg";

function hashOf(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h;
}
const faceColor = (name) => FACES[hashOf(name) % FACES.length];

function faceSVG(fill, size) {
  const svg = document.createElementNS(NS_SVG, "svg");
  svg.setAttribute("class", "face");
  svg.setAttribute("width", size);
  svg.setAttribute("height", size);
  svg.setAttribute("viewBox", "0 0 40 40");
  const disc = document.createElementNS(NS_SVG, "circle");
  disc.setAttribute("cx", "20"); disc.setAttribute("cy", "20"); disc.setAttribute("r", "20");
  disc.setAttribute("fill", fill);
  svg.append(disc);
  for (const cx of [14.4, 25.6]) {
    const eye = document.createElementNS(NS_SVG, "ellipse");
    eye.setAttribute("cx", cx); eye.setAttribute("cy", "18.2");
    eye.setAttribute("rx", "2.7"); eye.setAttribute("ry", "3.5");
    eye.setAttribute("fill", "#141416");
    svg.append(eye);
  }
  return svg;
}

// An uploaded avatar is a data URI, used as the circle directly
function faceIMG(src, size) {
  const img = document.createElement("img");
  img.className = "face";
  img.src = src;
  img.width = size;
  img.height = size;
  img.alt = "";
  return img;
}

// Avatar resolution: empty hashes a fill from the name, #rrggbb is a fill, data: is a user-supplied image
function faceNode(spec, fallbackName, size) {
  if (spec && spec.startsWith("data:")) return faceIMG(spec, size);
  return faceSVG(spec || faceColor(fallbackName), size);
}

const botOf = (name) => state.bots.find((b) => b.name === name);
const cfgOf = (name) => (botOf(name) || {}).config || {};

// The name shown in the UI: a custom display name wins, otherwise the @mention id
const titleOf = (name) => cfgOf(name).display_name || name;
const modelSet = (name) => !!(cfgOf(name).provider);
const settingsOf = () => state.settings || {};

// Avatar size in the message stream, matching the header's
const MSG_AV = 26;

function avatar(name, size = 40, busy = false) {
  const wrap = el("span", "av" + (busy ? " busy" : ""));
  wrap.style.width = size + "px";
  wrap.style.height = size + "px";
  wrap.append(faceNode(cfgOf(name).avatar, name, size));
  return wrap;
}

// Group avatar: unless set, the first two members' faces are stacked — instantly distinct from a DM
function groupAvatarFor(id, size = 40) {
  const spec = groupFaceOf(id);
  if (spec) {
    const wrap = el("span", "av");
    wrap.style.width = size + "px";
    wrap.style.height = size + "px";
    wrap.append(faceNode(spec, id, size));
    return wrap;
  }
  const wrap = el("span", "av stack");
  wrap.style.width = size + "px";
  wrap.style.height = size + "px";
  const members = (groupMembersOf(id) || []).slice(0, 2);
  while (members.length < 2) members.push("bot-bureau-" + members.length);
  const small = Math.round(size * 0.68);
  wrap.append(faceSVG(faceColor(members[0]), small), faceSVG(faceColor(members[1]), small));
  return wrap;
}
