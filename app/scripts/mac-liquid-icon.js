"use strict";

// electron-builder afterPack hook: compile the AppIcon.icon that Icon Composer produced into an
// asset catalog and place it in the bundle, so macOS 26 renders the app icon per appearance —
// light, dark, tinted, clear — instead of showing one fixed bitmap forever.

// Why the detour: an .icns carries a single bitmap and the system will not derive light and dark
// builds from it. The format that is genuinely appearance-aware from macOS 26 on is .icon, and
// electron-builder 25 does not know it — it only knows the .icns named in the icon field. So the
// catalog is added after packing and before signing. That order matters: afterPack runs before the
// signature is made, so these changes get sealed into it; doing the same work in afterSign would
// break the signature that was just applied.

// The two icons coexist, each serving the systems that understand it:
// CFBundleIconFile — icon.icns, written by electron-builder; the path macOS below 26 takes
// CFBundleIconName — the AppIcon inside the Assets.car added here; macOS 26 prefers it
// Older systems are unaffected and newer ones gain appearance switching, from a single package.

// With no assets/AppIcon.icon present the whole step is skipped: this is a bonus, not something
// worth failing a build over.
const { execFileSync } = require("child_process");
const crypto = require("crypto");
const fs = require("fs");
const os = require("os");
const path = require("path");

const ICON_NAME = "AppIcon";

function sourceDigest(root) {
  const files = [];
  function visit(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const file = path.join(dir, entry.name);
      if (entry.isDirectory()) visit(file);
      else files.push(file);
    }
  }
  visit(root);
  const hash = crypto.createHash("sha256");
  for (const file of files) {
    hash.update(path.relative(root, file));
    hash.update(fs.readFileSync(file));
  }
  return hash.digest("hex").slice(0, 16);
}

function actoolPath() {
  try {
    return execFileSync("xcrun", ["-f", "actool"], { encoding: "utf8" }).trim();
  } catch {
    return null;
  }
}

// actool exits 0 even when it fails, so the exit code alone would read a failure as success; the
// errors live in the plist it prints, which is what has to be inspected.
function compile(actool, input, outDir, plist) {
  let output = "";
  try {
    output = execFileSync(actool, [
      input,
      "--compile", outDir,
      "--platform", "macosx",
      "--minimum-deployment-target", "26.0",
      "--app-icon", ICON_NAME,
      "--output-partial-info-plist", plist,
    ], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  } catch (e) {
    return { ok: false, log: String((e && (e.stdout || e.message)) || e) };
  }
  if (output.includes("com.apple.actool.errors")) return { ok: false, log: output };
  return { ok: fs.existsSync(path.join(outDir, "Assets.car")), log: output };
}

function setPlist(file, key, value) {
  try {
    execFileSync("/usr/libexec/PlistBuddy", ["-c", `Set :${key} ${value}`, file], { stdio: "ignore" });
  } catch {
    execFileSync("/usr/libexec/PlistBuddy", ["-c", `Add :${key} string ${value}`, file], { stdio: "ignore" });
  }
}

module.exports = async function (context) {
  if (context.electronPlatformName !== "darwin") return;

  const source = path.join(__dirname, "..", "..", "assets", `${ICON_NAME}.icon`);
  if (!fs.existsSync(source)) {
    console.log(`[icon] ${path.relative(process.cwd(), source)} not found — packaging with .icns only`);
    return;
  }

  const actool = actoolPath();
  if (!actool) {
    console.warn("[icon] actool unavailable (full Xcode required) — packaging with .icns only");
    return;
  }

  // Universal packaging runs this hook once for each architecture. actool can emit an Assets.car
  // with architecture-specific metadata even when the source icon is identical, and
  // @electron/universal then refuses to merge the two non-Mach-O files. Reuse one deterministic
  // compilation for the same source so both slices carry byte-identical appearance assets.
  const cache = path.join(os.tmpdir(), `botbureau-${ICON_NAME}-${sourceDigest(source)}.car`);
  const appDir = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  const target = path.join(appDir, "Contents", "Resources", "Assets.car");
  if (fs.existsSync(cache)) {
    fs.copyFileSync(cache, target);
    setPlist(path.join(appDir, "Contents", "Info.plist"), "CFBundleIconName", ICON_NAME);
    console.log(`[icon] reused cached ${ICON_NAME}.icon asset catalog`);
    return;
  }

  const work = fs.mkdtempSync(path.join(os.tmpdir(), "bb-icon-"));
  const outDir = path.join(work, "out");
  const plist = path.join(work, "partial.plist");
  fs.mkdirSync(outDir);


  // Hand the .icon straight to actool; should this version only accept an asset catalog, wrap it in
  // one and go around again.
  let result = compile(actool, source, outDir, plist);
  if (!result.ok) {
    const catalog = path.join(work, "Assets.xcassets");
    fs.mkdirSync(catalog);
    fs.writeFileSync(path.join(catalog, "Contents.json"),
      JSON.stringify({ info: { author: "xcode", version: 1 } }, null, 2));
    fs.cpSync(source, path.join(catalog, `${ICON_NAME}.icon`), { recursive: true });
    result = compile(actool, catalog, outDir, plist);
  }

  if (!result.ok) {
    console.warn("[icon] actool could not compile the icon — packaging with .icns only");
    console.warn(result.log.trim());
    fs.rmSync(work, { recursive: true, force: true });
    return;
  }

  fs.copyFileSync(path.join(outDir, "Assets.car"), cache);
  fs.copyFileSync(cache, target);

  // CFBundleIconFile is left alone: that is electron-builder's .icns, the fallback for macOS below 26.
  setPlist(path.join(appDir, "Contents", "Info.plist"), "CFBundleIconName", ICON_NAME);
  fs.rmSync(work, { recursive: true, force: true });
  console.log(`[icon] ${ICON_NAME}.icon compiled into the bundle — macOS 26 will switch it by appearance`);
};
