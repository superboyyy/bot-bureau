"use strict";

// electron-builder afterPack hook: clear extended attributes from the packaged .app before signing.

// The reason is that codesign refuses files carrying com.apple.FinderInfo or a resource fork, failing
// the whole build with "resource fork, Finder information, or similar detritus not allowed".

// Those attributes do not come from packaging — they come from iCloud Drive: with the project under
// Desktop and "Desktop & Documents" syncing enabled, the file provider stamps FinderInfo onto the
// bundle directories inside node_modules/electron/dist, electron-builder copies those directories into
// dist/ with the attributes attached, and signing dies on one of the helpers. The same source builds
// fine into /tmp purely because iCloud does not manage it, which makes this very easy to misread as a
// signing-certificate problem.

// The cleanup touches only the build output, never the source in node_modules (that belongs to someone
// else, and iCloud would re-add the attributes there anyway).
const { execFileSync } = require("child_process");
const path = require("path");

exports.default = async function stripXattr(context) {
  if (context.electronPlatformName !== "darwin") return;
  const app = path.join(context.appOutDir, context.packager.appInfo.productFilename + ".app");
  try {
    execFileSync("xattr", ["-cr", app], { stdio: "ignore" });
    console.log("[xattr] cleared extended attributes on " + path.basename(app));
  } catch (e) {

    // If it cannot be cleared, let signing report the failure rather than hiding it here
    console.warn("[xattr] could not clear attributes: " + ((e && e.message) || e));
  }
};
