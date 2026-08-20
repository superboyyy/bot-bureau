"use strict";

// Write an AppArmor profile next to the packaged Linux binary so the .deb after-install
// script can load it. Ubuntu 24.04+ blocks unprivileged user namespaces without this,
// and Chromium then exits with SIGTRAP before any window appears.
const fs = require("fs");
const path = require("path");
const { apparmorProfile } = require("../lib/linux-runtime");

exports.default = async function linuxApparmor(context) {
  if (context.electronPlatformName !== "linux") return;
  const executable = context.packager.executableName;
  const installDir = context.packager.appInfo.sanitizedProductName;
  const dest = path.join(context.appOutDir, "resources", "apparmor-profile");
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, apparmorProfile({ executable, installDir }));
  console.log("[apparmor] wrote " + dest);
};
