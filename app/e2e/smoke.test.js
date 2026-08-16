const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");
const { _electron: electron } = require("playwright");

test("Electron launches the real client and reaches the renderer", {
  skip: process.env.BOTBUREAU_RUN_E2E === "1" ? false : "set BOTBUREAU_RUN_E2E=1 to run Electron E2E"
}, async () => {
  const dataDir = await fs.mkdtemp(path.join(os.tmpdir(), "botbureau-e2e-"));
  const appDir = path.resolve(__dirname, "..");
  const testEnv = { ...process.env };
  delete testEnv.BOTBUREAU_BACKEND_URL;
  Object.assign(testEnv, {
    BOTBUREAU_DATA_DIR: dataDir,
    BOTBUREAU_LOCAL_ONLY: "1",
    BOTBUREAU_E2E: "1",
    ELECTRON_DISABLE_SECURITY_WARNINGS: "true"
  });
  let electronApp;
  try {
    electronApp = await electron.launch({
      args: [appDir, "--no-sandbox"],
      env: testEnv,
      timeout: 30000
    });
    const window = await electronApp.firstWindow();
    window.on("console", (message) => {
      if (message.type() === "error") console.error("[renderer:error]", message.text());
    });
    window.on("pageerror", (error) => console.error("[renderer:pageerror]", error));
    await window.waitForLoadState("domcontentloaded");
    try {
      await window.locator("#app").waitFor({ state: "attached", timeout: 30000 });
    } catch (error) {
      console.error("[renderer:body]", await window.locator("body").innerText().catch(() => "<unavailable>"));
      console.error("[electron:diagnostic]", await electronApp.evaluate(() => {
        const fs = require("node:fs");
        const path = require("node:path");
        const root = process.env.BOTBUREAU_DATA_DIR || "";
        const tokenPath = path.join(root, "data", "token");
        let tokenLength = 0;
        try { tokenLength = fs.readFileSync(tokenPath, "utf8").trim().length; } catch {}
        return { root, tokenExists: fs.existsSync(tokenPath), tokenLength };
      }));
      throw error;
    }
    assert.equal(await window.title(), "Bot Bureau");
    assert.equal(await window.locator("#app").count(), 1);
    assert.equal(await window.locator("#newBtn").count(), 1);
  } finally {
    if (electronApp) await electronApp.close();
    await fs.rm(dataDir, { recursive: true, force: true });
  }
});
