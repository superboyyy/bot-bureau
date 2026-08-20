// @vitest-environment node
import { describe, expect, it } from "vitest";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { isRunning, listDescendants, stopEngine } from "../lib/stop-engine.js";

function mockProc(pid) {
  const listeners = [];
  return {
    pid,
    exitCode: null,
    signalCode: null,
    kill(sig) {
      this._killed = sig || true;
      if (typeof sig === "string") this.signalCode = sig;
      else this.exitCode = 1;
      for (const fn of listeners) fn();
    },
    once(ev, fn) {
      if (ev === "exit") listeners.push(fn);
    },
  };
}

describe("listDescendants", () => {
  it("walks pgrep -P recursively", () => {
    const run = (cmd, args) => {
      expect(cmd).toBe("pgrep");
      const parent = args[1];
      const trees = { 10: "11\n12", 11: "13", 12: "", 13: "" };
      return { stdout: trees[parent] || "", status: trees[parent] ? 0 : 1 };
    };
    expect(listDescendants(10, run).sort((a, b) => a - b)).toEqual([11, 12, 13]);
  });

  it("returns an empty list when pgrep is missing", () => {
    const run = () => { throw new Error("ENOENT"); };
    expect(listDescendants(10, run)).toEqual([]);
  });
});

describe("stopEngine", () => {
  it("is a no-op when the process has already exited", async () => {
    const killed = [];
    await stopEngine({ pid: 1, exitCode: 0 }, { kill: (id) => killed.push(id) });
    expect(killed).toEqual([]);
  });

  it("uses taskkill /T /F on Windows", async () => {
    const proc = mockProc(42);
    const calls = [];
    await stopEngine(proc, {
      platform: "win32",
      termWaitMs: 5,
      hardWaitMs: 5,
      spawnSync: (cmd, args, opts) => {
        calls.push({ cmd, args, opts });
        proc.kill();
        return { status: 0 };
      },
    });
    expect(calls).toHaveLength(1);
    expect(calls[0].cmd).toBe("taskkill");
    expect(calls[0].args).toEqual(["/pid", "42", "/T", "/F"]);
    expect(proc.exitCode).not.toBeNull();
  });

  it("SIGTERMs unix descendants then SIGKILLs what is left", async () => {
    const sent = [];
    const proc = {
      pid: 10,
      exitCode: null,
      signalCode: null,
      kill(sig) { sent.push(["proc", sig || "kill"]); },
      once() { /* stay alive so the SIGKILL path runs */ },
    };
    const kids = { 10: "11", 11: "" };
    await stopEngine(proc, {
      platform: "linux",
      termWaitMs: 20,
      hardWaitMs: 20,
      spawnSync: (_cmd, args) => ({ stdout: kids[args[1]] || "", status: 0 }),
      kill: (id, sig) => sent.push([id, sig]),
    });
    expect(sent.filter((row) => row[1] === "SIGTERM")).toEqual([
      [11, "SIGTERM"],
      [10, "SIGTERM"],
    ]);
    expect(sent.some((row) => row[0] === 11 && row[1] === "SIGKILL")).toBe(true);
    expect(sent.some((row) => row[0] === 10 && row[1] === "SIGKILL")).toBe(true);
  });

  it("kills a real grandchild on unix", async () => {
    if (process.platform === "win32") return;
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "stop-engine-"));
    const marker = path.join(dir, "child.pid");
    const proc = spawn("sh", ["-ec", `sleep 120 & echo $! > "${marker}"; wait`], {
      stdio: "ignore",
    });
    try {
      const deadline = Date.now() + 3000;
      while (!fs.existsSync(marker) && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 20));
      }
      const childPid = Number(fs.readFileSync(marker, "utf8").trim());
      expect(childPid).toBeGreaterThan(0);
      process.kill(childPid, 0);
      process.kill(proc.pid, 0);

      await stopEngine(proc, { termWaitMs: 500, hardWaitMs: 500 });

      expect(isRunning(proc)).toBe(false);
      expect(() => process.kill(proc.pid, 0)).toThrow();
      expect(() => process.kill(childPid, 0)).toThrow();
    } finally {
      try { process.kill(proc.pid, "SIGKILL"); } catch { /* already dead */ }
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }, 10000);
});
