const { defineConfig } = require("vitest/config");

module.exports = defineConfig({
  test: {
    environment: "jsdom",
    environmentOptions: {
      jsdom: {
        url: "http://localhost/"
      }
    },
    setupFiles: ["./test/setup.js"],
    include: ["test/**/*.test.js"],
    coverage: {
      provider: "v8",
      reporter: ["text", "html", "lcov"],
      include: ["renderer/markdown.js", "renderer/i18n.js", "renderer/views/chrome.js"],
      exclude: ["renderer/locales/**"],
      thresholds: {
        lines: 80,
        functions: 80,
        statements: 80,
        branches: 70
      }
    }
  }
});
