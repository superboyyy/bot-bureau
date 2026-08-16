"use strict";

// Dev-mode (npm start) identity fix: the stock Electron .app makes the Dock and menu bar read
// "Electron", so its Info.plist name and icon are swapped for Bot Bureau's.
// Packaged builds never take this path — electron-builder uses productName / icon from package.json.

// Critically: touching the bundle invalidates its signature (macOS reports "code has no resources
// but signature indicates they must be present"). A broken signature can be refused by Gatekeeper
// under stricter policies and tends to re-prompt for keychain/network access, so the bundle is
// re-signed ad-hoc right after being modified.

// ⚠️ This script modifies node_modules/electron/dist/Electron.app, which is exactly what
// electron-builder packages from. It then renames the executable, injects app.asar and swaps the icon
// — changes that invalidate the ad-hoc signature applied here. A normal build (with a Developer ID)
// re-signs the whole bundle at the end and is fine; but skipping signing via
// CSC_IDENTITY_AUTO_DISCOVERY=false leaves the broken signature in the product and macOS refuses to
// launch it, with the symptom being "quits silently right after starting" — not something anyone
// traces back to a signature.
// To produce an unsigned package for a quick check: delete
// node_modules/electron/dist/.botbureau-identity, reinstall electron, and the packaging source
// returns to its factory signature.
const { execFileSync } = require("child_process");
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

const root = path.join(__dirname, "..");
const electronApp = path.join(root, "node_modules/electron/dist/Electron.app");
if (process.platform !== "darwin" || !fs.existsSync(electronApp)) process.exit(0);

function run(cmd, args) {
  execFileSync(cmd, args, { stdio: "ignore" });
}

function setPlist(file, key, value) {
  try {
    run("/usr/libexec/PlistBuddy", ["-c", `Set :${key} ${value}`, file]);
  } catch {
    try {
      run("/usr/libexec/PlistBuddy", ["-c", `Add :${key} string ${value}`, file]);
    } catch { /* not worth blocking startup over a name */ }
  }
}

// Bail out if the current source icon was already applied: re-signing on every prestart costs seconds
// for nothing, but a changed icon must invalidate the stamp and be copied into Electron.app again.
// The stamp must live outside the .app — a file under Contents/ would be an unsealed resource
// and would itself invalidate the signature we just made.
const stamp = path.join(root, "node_modules/electron/dist/.botbureau-identity");
const icns = path.join(root, "build/icon.icns");
const dest = path.join(electronApp, "Contents/Resources/electron.icns");
const iconStamp = fs.existsSync(icns)
  ? "v2:" + crypto.createHash("sha256").update(fs.readFileSync(icns)).digest("hex")
  : "v2:no-icon";
if (fs.existsSync(stamp) && fs.readFileSync(stamp, "utf8").trim() === iconStamp) process.exit(0);

const info = path.join(electronApp, "Contents/Info.plist");
setPlist(info, "CFBundleName", "Bot Bureau");
setPlist(info, "CFBundleDisplayName", "Bot Bureau");

if (fs.existsSync(icns)) fs.copyFileSync(icns, dest);

// Re-sign. Two gotchas: extended attributes (quarantine flags and friends) make codesign refuse
// outright and must be cleared first; and in a single --deep pass the outer seal is computed
// before the nested signatures are updated, so the first pass often fails verification and needs
// a second. Sign until verification passes, at most three attempts.
try {
  let signed = false;
  for (let attempt = 0; attempt < 3 && !signed; attempt++) {
    run("xattr", ["-cr", electronApp]);
    run("codesign", ["--force", "--deep", "--sign", "-", electronApp]);
    try {
      run("codesign", ["-v", electronApp]);
      signed = true;
    } catch { /* go around again */ }
  }
  if (!signed) throw new Error("codesign verification never passed");
  fs.writeFileSync(stamp, iconStamp + "\n");
} catch {

  // If it cannot be re-signed, roll back — a Dock reading "Electron" beats a broken-signature app
  setPlist(info, "CFBundleName", "Electron");
  setPlist(info, "CFBundleDisplayName", "Electron");
  console.warn("[identity] could not re-sign Electron.app; left it untouched");
}
