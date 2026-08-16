import { describe, expect, it } from "vitest";
import "../renderer/markdown.js";

const { safeHref, renderMarkdown } = window.__botBureauMarkdown;

describe("markdown renderer", () => {
  it("allows only HTTP and HTTPS links", () => {
    expect(safeHref("https://example.com/a?q=1")).toBe("https://example.com/a?q=1");
    expect(safeHref("http://localhost:3000")).toBe("http://localhost:3000/");
    expect(safeHref("javascript:alert(1)")).toBe("");
    expect(safeHref("data:text/html,<script>alert(1)</script>")).toBe("");
    expect(safeHref("not a URL")).toBe("");
  });

  it("renders supported blocks and keeps fenced code as text", () => {
    const root = renderMarkdown([
      "# Heading",
      "",
      "**bold** *italic* `code` [site](https://example.com)",
      "",
      "> quoted",
      "",
      "- one",
      "- two",
      "",
      "```js",
      "<script>alert(1)</script>",
      "```"
    ].join("\n"));

    expect(root.querySelector("h1").textContent).toBe("Heading");
    expect(root.querySelector("strong").textContent).toBe("bold");
    expect(root.querySelector("em").textContent).toBe("italic");
    expect(root.querySelector("code").textContent).toContain("code");
    expect(root.querySelector("a").getAttribute("href")).toBe("https://example.com/");
    expect(root.querySelector("blockquote").textContent).toBe("quoted");
    expect(root.querySelectorAll("li")).toHaveLength(2);
    expect(root.querySelector("pre code").textContent).toBe("<script>alert(1)</script>");
    expect(root.querySelectorAll("script")).toHaveLength(0);
  });

  it("does not turn unsafe markdown links into anchors", () => {
    const root = renderMarkdown("[bad](javascript:alert(1)) https://example.com/path.");
    expect(root.querySelectorAll("a")).toHaveLength(1);
    expect(root.textContent).toContain("[bad](javascript:alert(1))");
    expect(root.textContent).toContain("https://example.com/path.");
  });
});
