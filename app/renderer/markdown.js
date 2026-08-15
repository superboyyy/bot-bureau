"use strict";

// 把模型输出的 Markdown 渲染成 DOM。
//
// 全程只用 createElement + textContent，一处 innerHTML 都没有：正文是模型生成的，
// 等同于不可信输入，拼字符串迟早会被一段构造出来的内容打穿。
// 只支持一个够用的子集——粗体、斜体、行内代码、链接、标题、引用、列表、围栏代码块。
//
// Renders the models' Markdown into DOM.
//
// Everything goes through createElement + textContent; there is no innerHTML anywhere. The text is
// model-generated and therefore untrusted input, and string-building would eventually be broken by
// crafted content. Only a sufficient subset is supported: bold, italic, inline code, links,
// headings, block quotes, lists and fenced code blocks.

// safeHref 只放行 http/https。javascript: 和 data: 一律退化成纯文本，
// 免得一个链接就能在渲染进程里执行脚本。
// safeHref admits only http/https. javascript: and data: degrade to plain text, so a link can never
// execute script inside the renderer.
function safeHref(url) {
  try {
    const u = new URL(url);
    if (u.protocol === "http:" || u.protocol === "https:") return u.href;
  } catch {}
  return "";
}

// appendLink 建外链。target=_blank 配 rel=noopener noreferrer：
// 少了 noopener，新开的页面能通过 window.opener 反过来操纵本窗口。
// （主进程还会把外链交给系统浏览器，见 main.js 的 setWindowOpenHandler。）
//
// appendLink builds an external link. target=_blank pairs with rel=noopener noreferrer: without
// noopener the opened page can reach back through window.opener and drive this window.
// (The main process also hands external links to the system browser — see setWindowOpenHandler in main.js.)
function appendLink(parent, href, label) {
  const safe = safeHref(href);
  if (!safe) {
    parent.append(document.createTextNode(label));
    return;
  }
  const a = document.createElement("a");
  a.href = safe;
  a.target = "_blank";
  a.rel = "noopener noreferrer";
  a.textContent = label;
  parent.append(a);
}

// mdInline 处理行内标记。一条正则同时匹配所有形式，靠捕获组分辨命中的是哪一种，
// 这样就不必反复扫描同一段文本，也不会出现"先替换粗体、结果把代码块里的星号也吃了"这类串味。
//
// mdInline handles inline markup. A single regex matches every form at once and the capture groups
// say which one hit, so the text is scanned once and there is no cross-talk of the
// "bold pass ate the asterisks inside a code span" kind.
function mdInline(text, parent) {
  const re = /\*\*([^*]+)\*\*|\*([^*\n]+)\*|`([^`\n]+)`|\[([^\]]+)\]\(((?:[^()]|\([^()]*\))*)\)|(https?:\/\/[^\s<]+)/g;
  let last = 0;
  let m;
  while ((m = re.exec(text))) {
    if (m.index > last) parent.append(document.createTextNode(text.slice(last, m.index)));
    if (m[1] !== undefined) {
      const s = document.createElement("strong");
      s.textContent = m[1];
      parent.append(s);
    } else if (m[2] !== undefined) {
      const s = document.createElement("em");
      s.textContent = m[2];
      parent.append(s);
    } else if (m[3] !== undefined) {
      const s = document.createElement("code");
      s.textContent = m[3];
      parent.append(s);
    } else if (m[4] !== undefined) {
      if (safeHref(m[5])) appendLink(parent, m[5], m[4]);
      else parent.append(document.createTextNode(m[0]));
    } else {
      let raw = m[6];
      let trail = "";
      const trimmed = raw.replace(/[),.;!?]+$/, "");
      const punct = raw.slice(trimmed.length);
      if (punct && safeHref(trimmed)) {
        raw = trimmed;
        trail = punct;
      }
      appendLink(parent, raw, raw);
      if (trail) parent.append(document.createTextNode(trail));
    }
    last = re.lastIndex;
  }
  if (last < text.length) parent.append(document.createTextNode(text.slice(last)));
}

function isBlank(line) { return /^\s*$/.test(line); }
function isFence(line) { return /^```/.test(line); }
function isHeading(line) { return /^#{1,3} /.test(line); }
function isQuote(line) { return /^> /.test(line); }
function isUl(line) { return /^\s*[-*] /.test(line); }
function isOl(line) { return /^\s*\d+\. /.test(line); }

// renderMarkdown 逐行走块级结构，返回一个 div.md。
// 围栏代码块最先判定：它内部的内容不能再被当作 Markdown 解析。
//
// renderMarkdown walks the block structure line by line and returns a div.md.
// Fenced code is checked first: its contents must not be parsed as Markdown at all.
function renderMarkdown(text) {
  const root = document.createElement("div");
  root.className = "md";
  const lines = String(text ?? "").replace(/\r\n/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (isFence(line)) {
      const buf = [];
      i++;
      while (i < lines.length && !isFence(lines[i])) { buf.push(lines[i]); i++; }
      if (i < lines.length) i++;
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      code.textContent = buf.join("\n");
      pre.append(code);
      root.append(pre);
      continue;
    }
    if (isBlank(line)) { i++; continue; }
    if (isHeading(line)) {
      const level = line.match(/^#+/)[0].length;
      const h = document.createElement("h" + level);
      mdInline(line.replace(/^#{1,3} /, ""), h);
      root.append(h);
      i++;
      continue;
    }
    if (isQuote(line)) {
      const bq = document.createElement("blockquote");
      const buf = [];
      while (i < lines.length && isQuote(lines[i])) { buf.push(lines[i].slice(2)); i++; }
      buf.forEach((ln, idx) => {
        if (idx) bq.append(document.createElement("br"));
        mdInline(ln, bq);
      });
      root.append(bq);
      continue;
    }
    if (isUl(line) || isOl(line)) {
      const ordered = isOl(line);
      const list = document.createElement(ordered ? "ol" : "ul");
      const itemRe = ordered ? /^\s*\d+\. / : /^\s*[-*] /;
      while (i < lines.length && itemRe.test(lines[i])) {
        const li = document.createElement("li");
        mdInline(lines[i].replace(itemRe, ""), li);
        list.append(li);
        i++;
      }
      root.append(list);
      continue;
    }
    const buf = [];
    while (
      i < lines.length &&
      !isBlank(lines[i]) && !isFence(lines[i]) && !isHeading(lines[i]) &&
      !isQuote(lines[i]) && !isUl(lines[i]) && !isOl(lines[i])
    ) {
      buf.push(lines[i]);
      i++;
    }
    const p = document.createElement("p");
    buf.forEach((ln, idx) => {
      if (idx) p.append(document.createElement("br"));
      mdInline(ln, p);
    });
    root.append(p);
  }
  return root;
}
