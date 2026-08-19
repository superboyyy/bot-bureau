const assert = require("node:assert/strict");
const { test } = require("node:test");
const { launchDesktop, closeDesktop } = require("./helpers");

const e2e = process.env.BOTBUREAU_RUN_E2E === "1"
  ? false
  : "set BOTBUREAU_RUN_E2E=1 to run Electron E2E";

test("Electron launches the real client and reaches the renderer", { skip: e2e, timeout: 60000 }, async () => {
  const session = await launchDesktop();
  try {
    const { window } = session;
    assert.equal(await window.title(), "Bot Bureau");
    assert.equal(await window.locator("#app").count(), 1);
    assert.equal(await window.locator("#newBtn").count(), 1);
    if (process.platform === "darwin") {
      assert.ok(await window.locator("#winChrome").getAttribute("hidden") !== null);
    } else {
      assert.equal(await window.locator("#winChrome").getAttribute("hidden"), null);
      assert.equal(await window.locator("#winMin").count(), 1);
      assert.equal(await window.locator("#winMax").count(), 1);
      assert.equal(await window.locator("#winClose").count(), 1);
      const color = await window.locator("#winMin").evaluate((el) => getComputedStyle(el).color);
      assert.notEqual(color, "rgb(0, 0, 0)");
      assert.notEqual(color, "rgba(0, 0, 0, 0)");
    }
  } finally {
    await closeDesktop(session);
  }
});
