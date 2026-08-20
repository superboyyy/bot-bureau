// @vitest-environment node
import { describe, expect, it } from "vitest";
import { apparmorProfile, linuxNeedsNoSandbox } from "../lib/linux-runtime.js";
import linuxApparmor from "../scripts/linux-apparmor.js";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

describe("linuxNeedsNoSandbox", () => {
  it("is off on non-Linux", () => {
    expect(linuxNeedsNoSandbox({
      platform: "darwin",
      readFileSync: () => "1",
      existsSync: () => false,
      execPath: "/opt/Bot Bureau/bot-bureau",
    })).toBe(false);
  });

  it("is off when the kernel does not restrict userns", () => {
    expect(linuxNeedsNoSandbox({
      platform: "linux",
      readFileSync: () => "0\n",
      existsSync: () => false,
      execPath: "/opt/Bot Bureau/bot-bureau",
    })).toBe(false);
  });

  it("is off when the sysctl is missing", () => {
    expect(linuxNeedsNoSandbox({
      platform: "linux",
      readFileSync: () => { throw new Error("ENOENT"); },
      existsSync: () => false,
      execPath: "/opt/Bot Bureau/bot-bureau",
    })).toBe(false);
  });

  it("is on when Ubuntu 24+ restricts userns and no profile is installed", () => {
    expect(linuxNeedsNoSandbox({
      platform: "linux",
      readFileSync: () => "1\n",
      existsSync: (p) => p === "/etc/apparmor.d/bot-bureau",
      execPath: "/opt/Bot Bureau/bot-bureau",
    })).toBe(false);

    expect(linuxNeedsNoSandbox({
      platform: "linux",
      readFileSync: () => "1\n",
      existsSync: () => false,
      execPath: "/opt/Bot Bureau/bot-bureau",
    })).toBe(true);
  });
});

describe("apparmorProfile", () => {
  it("quotes the /opt path so a product name with spaces still matches", () => {
    const text = apparmorProfile({ executable: "bot-bureau", installDir: "Bot Bureau" });
    expect(text).toContain('profile bot-bureau "/opt/Bot Bureau/bot-bureau" flags=(unconfined)');
    expect(text).toContain("userns,");
    expect(text).toContain("abi <abi/4.0>,");
  });

  it("afterPack writes the profile next to the Linux binary", async () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "apparmor-"));
    try {
      const write = typeof linuxApparmor === "function" ? linuxApparmor : linuxApparmor.default;
      await write({
        electronPlatformName: "linux",
        appOutDir: dir,
        packager: { executableName: "bot-bureau", appInfo: { sanitizedProductName: "Bot Bureau" } },
      });
      const text = readFileSync(path.join(dir, "resources", "apparmor-profile"), "utf8");
      expect(text).toContain('"/opt/Bot Bureau/bot-bureau"');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
