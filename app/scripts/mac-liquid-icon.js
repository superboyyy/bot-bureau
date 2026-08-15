"use strict";
// electron-builder 的 afterPack 钩子：把 Icon Composer 出的 AppIcon.icon 编成资源目录塞进包里，
// 让 macOS 26 自己按外观渲染应用图标（浅色 / 深色 / 着色 / 透明），而不是永远显示同一张死图。
//
// 为什么非得绕这一趟：.icns 只能带一张位图，系统不会替它派生深浅两版；macOS 26 起真正随外观切换
// 的是 .icon 格式，而 electron-builder 25 还不认这个格式。它认的只有 icon 字段那张 .icns，
// 所以这里在打包之后、签名之前自己补上——这个时序是关键，afterPack 跑在签名前，改动会被随后的
// 签名一起封进去；放到 afterSign 里改就等于亲手破坏刚签好的签名。
//
// 两张图标并存不冲突，各管各的系统：
//   CFBundleIconFile —— electron-builder 写的 icon.icns，macOS 26 以下走这条
//   CFBundleIconName —— 这里补的 Assets.car 里那个 AppIcon，macOS 26 优先用它
// 所以旧系统上一切照旧，新系统上才有外观切换，不需要为此分两个包。
//
// 没有 assets/AppIcon.icon 就整段跳过：这一步是锦上添花，不该让打包失败。
//
// electron-builder afterPack hook: compile the AppIcon.icon that Icon Composer produced into an
// asset catalog and place it in the bundle, so macOS 26 renders the app icon per appearance —
// light, dark, tinted, clear — instead of showing one fixed bitmap forever.
//
// Why the detour: an .icns carries a single bitmap and the system will not derive light and dark
// builds from it. The format that is genuinely appearance-aware from macOS 26 on is .icon, and
// electron-builder 25 does not know it — it only knows the .icns named in the icon field. So the
// catalog is added after packing and before signing. That order matters: afterPack runs before the
// signature is made, so these changes get sealed into it; doing the same work in afterSign would
// break the signature that was just applied.
//
// The two icons coexist, each serving the systems that understand it:
//   CFBundleIconFile — icon.icns, written by electron-builder; the path macOS below 26 takes
//   CFBundleIconName — the AppIcon inside the Assets.car added here; macOS 26 prefers it
// Older systems are unaffected and newer ones gain appearance switching, from a single package.
//
// With no assets/AppIcon.icon present the whole step is skipped: this is a bonus, not something
// worth failing a build over.
const { execFileSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const ICON_NAME = "AppIcon";

function actoolPath() {
  try {
    return execFileSync("xcrun", ["-f", "actool"], { encoding: "utf8" }).trim();
  } catch {
    return null;
  }
}

// actool 出错时照样退 0，光看退出码会把失败当成功——它把错误写在输出的 plist 里，得自己看。
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

  const work = fs.mkdtempSync(path.join(os.tmpdir(), "bb-icon-"));
  const outDir = path.join(work, "out");
  const plist = path.join(work, "partial.plist");
  fs.mkdirSync(outDir);

  // 先把 .icon 直接喂给 actool；万一这个版本只肯吃资源目录，就套一层 .xcassets 再来一次。
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

  const appDir = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  fs.copyFileSync(path.join(outDir, "Assets.car"), path.join(appDir, "Contents", "Resources", "Assets.car"));
  // CFBundleIconFile 不动：那是 electron-builder 放的 .icns，留给 macOS 26 以下的系统兜底。
  // CFBundleIconFile is left alone: that is electron-builder's .icns, the fallback for macOS below 26.
  setPlist(path.join(appDir, "Contents", "Info.plist"), "CFBundleIconName", ICON_NAME);
  fs.rmSync(work, { recursive: true, force: true });
  console.log(`[icon] ${ICON_NAME}.icon compiled into the bundle — macOS 26 will switch it by appearance`);
};
