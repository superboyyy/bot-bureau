"use strict";

const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { _electron: electron } = require("playwright");

async function launchDesktop() {
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
  const userDataDir = path.join(dataDir, "electron-profile");
  await fs.mkdir(userDataDir, { recursive: true });
  const electronApp = await electron.launch({
    args: [appDir, "--no-sandbox", "--user-data-dir=" + userDataDir],
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
    throw error;
  }
  return { electronApp, window, dataDir };
}

async function closeDesktop(session) {
  if (session && session.electronApp) await session.electronApp.close();
  if (session && session.dataDir) await fs.rm(session.dataDir, { recursive: true, force: true });
}

module.exports = { launchDesktop, closeDesktop };
