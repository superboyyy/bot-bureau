"use strict";

// Stop the Go engine child and any grandchildren (`go run` wraps the real binary).
// Windows uses taskkill's process tree; Unix walks pgrep children then SIGTERM/SIGKILL.
// A child that dies from a signal keeps exitCode === null in Node; signalCode is what changes.

const { spawnSync } = require("child_process");

function isRunning(proc) {
  return !!(proc && proc.pid && proc.exitCode == null && proc.signalCode == null);
}

function listDescendants(pid, run) {
  const seen = new Set();
  const walk = (p) => {
    if (!p || seen.has(p)) return;
    seen.add(p);
    let out;
    try {
      out = run("pgrep", ["-P", String(p)], { encoding: "utf8" });
    } catch {
      return;
    }
    const kids = String((out && out.stdout) || "")
      .trim()
      .split(/[\s\n]+/)
      .map((s) => Number(s))
      .filter((n) => n > 0);
    for (const k of kids) walk(k);
  };
  walk(pid);
  seen.delete(pid);
  return [...seen];
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function signal(kill, pid, sig) {
  try { kill(pid, sig); } catch { /* already gone */ }
}

function stopEngine(proc, opts = {}) {
  if (!isRunning(proc)) return Promise.resolve();
  const pid = proc.pid;

  const platform = opts.platform || process.platform;
  const kill = opts.kill || ((id, sig) => process.kill(id, sig));
  const run = opts.spawnSync || spawnSync;
  const termWaitMs = opts.termWaitMs ?? 2000;
  const hardWaitMs = opts.hardWaitMs ?? 1500;

  const exited = new Promise((resolve) => {
    if (typeof proc.once === "function") proc.once("exit", () => resolve());
    else resolve();
  });

  if (platform === "win32") {
    const result = run("taskkill", ["/pid", String(pid), "/T", "/F"], {
      windowsHide: true,
      timeout: 8000,
      stdio: "ignore",
    });
    if (result && result.error) {
      try { proc.kill(); } catch { /* ignore */ }
    }
  } else {
    const targets = [...listDescendants(pid, run), pid];
    for (const id of targets) signal(kill, id, "SIGTERM");
  }

  return Promise.race([exited, wait(termWaitMs)]).then(() => {
    if (!isRunning(proc)) return;
    if (platform === "win32") {
      try { proc.kill(); } catch { /* ignore */ }
    } else {
      const leftover = [pid, ...listDescendants(pid, run)];
      for (const id of leftover) signal(kill, id, "SIGKILL");
      try { proc.kill("SIGKILL"); } catch { /* ignore */ }
    }
    return Promise.race([exited, wait(hardWaitMs)]);
  }).then(() => {
    if (isRunning(proc)) {
      try { proc.kill(); } catch { /* ignore */ }
    }
  });
}

module.exports = { stopEngine, listDescendants, isRunning };
