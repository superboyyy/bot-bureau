"use strict";
// 开发态（npm start）身份修正：Electron 自带的 .app 会让 Dock 和菜单栏显示 "Electron"，
// 这里把它的 Info.plist 名字和图标换成 Bot Bureau 的。
// 打包产物不走这条路径——electron-builder 用 package.json 里的 productName / icon。
//
// Dev-mode (npm start) identity fix: the stock Electron .app makes the Dock and menu bar read
// "Electron", so its Info.plist name and icon are swapped for Bot Bureau's.
// Packaged builds never take this path — electron-builder uses productName / icon from package.json.
//
// 关键：改动 .app 内部会让原签名失效（macOS 会报 "code has no resources but signature
// indicates they must be present"），破签名的 app 在严格策略下会被 Gatekeeper 拦，
// 也容易让钥匙串/网络权限反复弹窗。所以改完必须原地重新 ad-hoc 签一次。
//
// Critically: touching the bundle invalidates its signature (macOS reports "code has no resources
// but signature indicates they must be present"). A broken signature can be refused by Gatekeeper
// under stricter policies and tends to re-prompt for keychain/network access, so the bundle is
// re-signed ad-hoc right after being modified.
//
// ⚠️ 这个脚本改的是 node_modules/electron/dist/Electron.app，而 electron-builder 打包时正是
// 拿那个目录当源。它会在此基础上改可执行文件名、塞进 app.asar、换图标——这些改动让这里打的
// ad-hoc 签名失效。正常构建（有 Developer ID）最后会整包重签，没问题；但若用
// CSC_IDENTITY_AUTO_DISCOVERY=false 跳过签名，坏签名就留在成品里，macOS 直接拒绝启动，
// 且症状是"启动后立刻静默退出"，很难往签名上想。
// 要出一个不签名的包做快速验证：先删 node_modules/electron/dist/.botbureau-identity 并重装
// electron，让打包源回到出厂签名。
//
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
    } catch { /* 名字改不动就算了，不值得挡住启动 / not worth blocking startup over a name */ }
  }
}

// 已经改过就直接退出：每次 prestart 都重签一遍要几秒，没必要。
// 戳记必须落在 .app 外面——写进 Contents/ 会成为一个没被封进签名的资源，反过来把签名弄坏。
// Bail out if it was already applied: re-signing on every prestart costs seconds for nothing.
// The stamp must live outside the .app — a file under Contents/ would be an unsealed resource
// and would itself invalidate the signature we just made.
const stamp = path.join(root, "node_modules/electron/dist/.botbureau-identity");
if (fs.existsSync(stamp)) process.exit(0);

const info = path.join(electronApp, "Contents/Info.plist");
setPlist(info, "CFBundleName", "Bot Bureau");
setPlist(info, "CFBundleDisplayName", "Bot Bureau");

const icns = path.join(root, "build/icon.icns");
const dest = path.join(electronApp, "Contents/Resources/electron.icns");
if (fs.existsSync(icns)) fs.copyFileSync(icns, dest);

// 重新签名。两个坑：扩展属性（下载隔离标记等）会让 codesign 直接拒签，要先清掉；
// 而 --deep 的一趟签名里，外层封印是在嵌套签名更新之前算的，所以第一遍常常验不过，
// 得再签一遍。这里签到 verify 通过为止，最多三次。
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
    } catch { /* 再来一次 / go around again */ }
  }
  if (!signed) throw new Error("codesign verification never passed");
  fs.writeFileSync(stamp, "");
} catch {
  // 签不回去就把改动退回原样，宁可 Dock 上显示 "Electron"，也别留一个坏签名的 app
  // If it cannot be re-signed, roll back — a Dock reading "Electron" beats a broken-signature app
  setPlist(info, "CFBundleName", "Electron");
  setPlist(info, "CFBundleDisplayName", "Electron");
  console.warn("[identity] could not re-sign Electron.app; left it untouched");
}
