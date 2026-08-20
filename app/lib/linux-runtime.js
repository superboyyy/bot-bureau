"use strict";

const fs = require("fs");
const path = require("path");

function linuxRestrictsUserns(readFileSync = fs.readFileSync, platform = process.platform) {
  if (platform !== "linux") return false;
  try {
    return String(readFileSync("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "utf8")).trim() === "1";
  } catch {
    return false;
  }
}

function linuxHasAppArmorProfile(existsSync = fs.existsSync, execPath = process.execPath) {
  return existsSync(path.join("/etc/apparmor.d", path.basename(execPath)));
}

// Ubuntu 24.04+ sets apparmor_restrict_unprivileged_userns=1. Chromium then cannot
// create a user namespace unless this executable has an AppArmor profile with `userns`.
// Without that, it aborts with SIGTRAP on startup. The .deb installs the profile;
// AppImage and a missing profile fall back to --no-sandbox so the app still opens.
function linuxNeedsNoSandbox(opts = {}) {
  const platform = opts.platform || process.platform;
  const readFileSync = opts.readFileSync || fs.readFileSync;
  const existsSync = opts.existsSync || fs.existsSync;
  const execPath = opts.execPath || process.execPath;
  if (!linuxRestrictsUserns(readFileSync, platform)) return false;
  if (linuxHasAppArmorProfile(existsSync, execPath)) return false;
  return true;
}

function apparmorProfile({ executable, installDir }) {
  return [
    "# Chromium user namespaces on Ubuntu 24.04+",
    "# (kernel.apparmor_restrict_unprivileged_userns=1).",
    "abi <abi/4.0>,",
    "include <tunables/global>",
    "",
    `profile ${executable} "/opt/${installDir}/${executable}" flags=(unconfined) {`,
    "  userns,",
    `  include if exists <local/${executable}>`,
    "}",
    "",
  ].join("\n");
}

module.exports = { linuxNeedsNoSandbox, apparmorProfile };
