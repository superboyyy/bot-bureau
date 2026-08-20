const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { launchDesktop, closeDesktop } = require("./helpers");

const e2e = process.env.BOTBUREAU_RUN_E2E === "1"
  ? false
  : "set BOTBUREAU_RUN_E2E=1 to run Electron E2E";

function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function waitForEnginePid(dataDir, timeoutMs = 10000) {
  const lockPath = path.join(dataDir, "data", "engine.lock");
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const raw = await fs.readFile(lockPath, "utf8");
      const m = /pid=(\d+)/.exec(raw);
      if (m) {
        const pid = Number(m[1]);
        if (pidAlive(pid)) return pid;
      }
    } catch { /* lock not written yet */ }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error("engine.lock pid did not appear");
}

async function waitUntilDead(pid, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (!pidAlive(pid)) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return !pidAlive(pid);
}

test("close minimizes on every OS; quit stops the engine", { skip: e2e, timeout: 60000 }, async () => {
  const session = await launchDesktop();
  try {
    const pid = await waitForEnginePid(session.dataDir);

    await session.electronApp.evaluate(({ BrowserWindow }) => {
      const w = BrowserWindow.getAllWindows()[0];
      w.close();
    });
    await new Promise((r) => setTimeout(r, 300));

    const state = await session.electronApp.evaluate(({ BrowserWindow }) => {
      const w = BrowserWindow.getAllWindows()[0];
      return {
        windows: BrowserWindow.getAllWindows().length,
        minimized: !!(w && w.isMinimized()),
        visible: !!(w && w.isVisible()),
        destroyed: !w || w.isDestroyed(),
      };
    });
    assert.equal(state.windows, 1);
    assert.equal(state.destroyed, false);
    assert.ok(state.minimized || !state.visible, "close should minimize, not destroy the window");
    assert.equal(pidAlive(pid), true, "close must not stop the engine");

    await session.electronApp.close();
    session.electronApp = null;
    assert.equal(await waitUntilDead(pid, 8000), true, "quit must stop the engine process");
  } finally {
    await closeDesktop(session);
  }
});
