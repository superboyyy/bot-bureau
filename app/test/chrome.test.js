import { beforeEach, describe, expect, it } from "vitest";
import "../renderer/i18n.js";
import "../renderer/views/chrome.js";

const chrome = window.__botBureauChrome;

function mountChrome() {
  document.body.innerHTML = `
    <div id="winChrome" hidden>
      <button type="button" id="winMin" title="Minimize"></button>
      <button type="button" id="winMax" title="Maximize" data-i18n-title="Maximize"></button>
      <button type="button" id="winClose" title="Close"></button>
    </div>`;
}

function setSearch(search) {
  window.history.replaceState({}, "", search ? "/?" + search : "/");
}

beforeEach(() => {
  document.documentElement.className = "";
  delete window.botBureauNative;
  delete window.themePref;
  setSearch("");
  mountChrome();
});

describe("html window chrome", () => {
  it("shows on chrome=html and hides on chrome=native", () => {
    setSearch("chrome=html");
    chrome.wireWindowChrome();
    expect(document.documentElement.classList.contains("win-controls")).toBe(true);
    expect(document.getElementById("winChrome").hidden).toBe(false);

    setSearch("chrome=native");
    chrome.wireWindowChrome();
    expect(document.documentElement.classList.contains("win-controls")).toBe(false);
    expect(document.getElementById("winChrome").hidden).toBe(true);
  });

  it("swaps maximize for restore while maximized", () => {
    setSearch("chrome=html");
    chrome.wireWindowChrome();
    chrome.setChromeMaximized(true);
    const box = document.getElementById("winChrome");
    const max = document.getElementById("winMax");
    expect(box.classList.contains("maximized")).toBe(true);
    expect(max.title).toBe("Restore");
    expect(max.getAttribute("aria-label")).toBe("Restore");
    expect(max.getAttribute("data-i18n-title")).toBe("Restore");

    chrome.setChromeMaximized(false);
    expect(box.classList.contains("maximized")).toBe(false);
    expect(max.title).toBe("Maximize");
  });

  it("calls through to native window controls", () => {
    const calls = [];
    window.botBureauNative = {
      windowControls: {
        minimize: () => calls.push("min"),
        maximize: () => calls.push("max"),
        close: () => calls.push("close"),
        isMaximized: () => true,
        onMaximized: (fn) => fn(true),
      }
    };
    setSearch("chrome=html");
    chrome.wireWindowChrome();
    document.getElementById("winMin").click();
    document.getElementById("winMax").click();
    document.getElementById("winClose").click();
    expect(calls).toEqual(["min", "max", "close"]);
    expect(document.getElementById("winChrome").classList.contains("maximized")).toBe(true);
  });

  it("resolves appearance from the preference and the system", () => {
    window.themePref = () => "light";
    expect(chrome.resolvedAppearance()).toBe("light");
    window.themePref = () => "dark";
    expect(chrome.resolvedAppearance()).toBe("dark");
    window.themePref = () => "auto";
    window.matchMedia = (query) => ({ matches: query.includes("light"), addEventListener() {} });
    expect(chrome.resolvedAppearance()).toBe("light");
  });

  it("reports the resolved appearance to the main process", () => {
    const seen = [];
    window.themePref = () => "light";
    window.botBureauNative = { setAppearance: (v) => seen.push(v) };
    chrome.syncWindowAppearance();
    expect(seen).toEqual(["light"]);
  });

  it("falls back to the user agent when chrome is not in the query", () => {
    setSearch("");
    Object.defineProperty(window.navigator, "userAgent", { configurable: true, value: "Mozilla/5.0 (X11; Linux x86_64)" });
    expect(chrome.htmlWindowChrome()).toBe(true);
    Object.defineProperty(window.navigator, "userAgent", { configurable: true, value: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)" });
    expect(chrome.htmlWindowChrome()).toBe(false);
  });

  it("is a no-op without the chrome markup", () => {
    document.body.replaceChildren();
    expect(() => chrome.wireWindowChrome()).not.toThrow();
    expect(() => chrome.setChromeMaximized(true)).not.toThrow();
  });
});
