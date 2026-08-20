// The browser page normally loads locales/zh.js first. Unit tests provide the smallest deterministic
// table they need before importing i18n.js.
if (typeof window === "undefined") {
  // Node tests (stop-engine) share this setup file but have no DOM.
} else {
window.__i18n = {
  en: {},
  zh: {
    "Hello %s": "你好 %s",
    "Placeholder": "占位符",
    "Document title": "文档标题"
  }
};

// Node 26 may leave jsdom's Storage object disabled unless a persistent localstorage file is
// supplied. The renderer only needs the browser Storage contract, so keep the test store in memory.
const storage = new Map();
const localStorageStub = {
  getItem: (key) => storage.has(key) ? storage.get(key) : null,
  setItem: (key, value) => storage.set(String(key), String(value)),
  removeItem: (key) => storage.delete(String(key)),
  clear: () => storage.clear(),
  key: (index) => Array.from(storage.keys())[index] || null,
  get length() { return storage.size; }
};
Object.defineProperty(window, "localStorage", { configurable: true, value: localStorageStub });
Object.defineProperty(globalThis, "localStorage", { configurable: true, value: localStorageStub });

Object.defineProperty(window.navigator, "language", {
  configurable: true,
  value: "en-US"
});
}
