const assert = require("node:assert/strict");
const { test } = require("node:test");
const { launchDesktop, closeDesktop } = require("./helpers");

const e2e = process.env.BOTBUREAU_RUN_E2E === "1"
  ? false
  : "set BOTBUREAU_RUN_E2E=1 to run Electron E2E";

const DIFF = [
  "--- a/notes.txt",
  "+++ b/notes.txt",
  "@@ -1,1 +1,1 @@",
  "-hello",
  "+HELLO"
].join("\n");

async function skipOnboarding(window) {
  const skip = window.locator("#onboardSkip");
  try {
    await skip.waitFor({ state: "visible", timeout: 8000 });
  } catch {
    return;
  }
  await skip.click();
  await window.locator("#onboardModal").waitFor({ state: "hidden", timeout: 5000 });
}

async function hireFakeBot(window) {
  await window.locator("#newBtn").click();
  await window.locator(".pop .pop-wide").first().waitFor({ state: "visible" });
  await window.locator(".pop .pop-wide").first().click();
  await window.locator("#botModal").waitFor({ state: "visible" });
  await window.locator('#botForm input[name="display_name"]').fill("Wren");
  await window.locator("#botModelPicker select").first().selectOption("fake");
  await window.locator("#botSubmit").click();
  await window.locator("#botModal").waitFor({ state: "hidden", timeout: 10000 });
  await window.locator("#composer").waitFor({ state: "visible", timeout: 10000 });
}

async function showApprovals(window, approvals) {
  await window.unroute("**/api/state").catch(() => {});
  await window.route("**/api/state", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.approvals = approvals;
    await route.fulfill({ json: body });
  });
  await window.evaluate(() => window.refetchState());
}

test("approval card in the desktop chat pane shows a unified diff", { skip: e2e, timeout: 90000 }, async () => {
  const session = await launchDesktop();
  try {
    const { window } = session;
    await skipOnboarding(window);
    await hireFakeBot(window);

    await showApprovals(window, [{
      id: 7,
      bot: "assistant",
      action: "edit_file: notes.txt",
      chat: "group",
      dir: "",
      diff: DIFF
    }]);

    const card = window.locator("#msgs .approval");
    await card.waitFor({ state: "visible", timeout: 8000 });
    const text = await card.locator("pre.diff").innerText();
    assert.match(text, /--- a\/notes\.txt/);
    assert.match(text, /-hello/);
    assert.match(text, /\+HELLO/);
    assert.equal(await card.locator("button.yes").count(), 1);
    assert.equal(await card.locator("button.no").count(), 1);

    await showApprovals(window, [{
      id: 8,
      bot: "assistant",
      action: "bash: ls",
      chat: "group",
      dir: ""
    }]);
    await window.locator("#msgs .approval .code").waitFor({ state: "visible", timeout: 8000 });
    assert.equal(await window.locator("#msgs .approval pre.diff").count(), 0);
    assert.equal(await window.locator("#msgs .approval .code").innerText(), "bash: ls");
  } finally {
    await closeDesktop(session);
  }
});

test("plan card shows the body and has no directory-grant button", { skip: e2e, timeout: 90000 }, async () => {
  const session = await launchDesktop();
  try {
    const { window } = session;
    await skipOnboarding(window);
    await hireFakeBot(window);

    await showApprovals(window, [{
      id: 9,
      bot: "assistant",
      action: "Split the auth package",
      chat: "group",
      kind: "plan",
      title: "Split the auth package",
      body: "1. edit a.go\n2. edit b.go"
    }]);

    const card = window.locator("#msgs .approval");
    await card.waitFor({ state: "visible", timeout: 8000 });
    const text = await card.locator("pre.diff").innerText();
    assert.match(text, /edit a\.go/);
    assert.match(text, /edit b\.go/);
    const head = await card.locator(".head").innerText();
    assert.match(head, /plan|计划/i);
    assert.equal(await card.locator("button.yes").count(), 1);
    assert.equal(await card.locator("button.no").count(), 1);
    assert.equal(await card.locator("button.grant").count(), 0);
  } finally {
    await closeDesktop(session);
  }
});
