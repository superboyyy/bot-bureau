import { beforeEach, describe, expect, it } from "vitest";
import "../renderer/i18n.js";

const i18n = window.__botBureauI18n;

describe("renderer i18n", () => {
  beforeEach(() => {
    localStorage.clear();
    i18n.setLocalePref("en");
    document.body.replaceChildren();
  });

  it("falls back to English and interpolates values", () => {
    expect(i18n.t("Unknown %s", "value")).toBe("Unknown value");
    i18n.setLocalePref("zh");
    expect(i18n.t("Hello %s", "Wren")).toBe("你好 Wren");
  });

  it("applies translated text, placeholders, and titles", () => {
    document.body.innerHTML = `
      <span data-i18n="Hello %s">Hello %s</span>
      <input data-i18n-ph="Placeholder" placeholder="Placeholder">
      <button data-i18n-title="Document title" title="Document title"></button>`;
    i18n.setLocalePref("zh");
    i18n.applyStatic(document);
    expect(document.querySelector("span").textContent).toBe("你好 %s");
    expect(document.querySelector("input").placeholder).toBe("占位符");
    expect(document.querySelector("button").title).toBe("文档标题");
    expect(document.documentElement.lang).toBe("zh");
  });

  it("joins sentences according to the active language", () => {
    expect(i18n.sentences("one", "", "two")).toBe("one two");
    i18n.setLocalePref("zh");
    expect(i18n.sentences("一", "", "二")).toBe("一二");
    expect(i18n.localePref()).toBe("zh");
  });
});
