"use strict";
// electron-builder 的 afterPack 钩子：签名前把打好的 .app 上的扩展属性清干净。
//
// 起因是 codesign 会拒签带 com.apple.FinderInfo 或资源分叉的文件，报一句
// "resource fork, Finder information, or similar detritus not allowed"，然后整个打包失败。
//
// 属性不是打包过程加的，是 iCloud Drive 加的：项目放在"桌面"下且开了"桌面与文档"同步时，
// file provider 会给 node_modules/electron/dist 里的 bundle 目录盖上 FinderInfo，
// electron-builder 复制这些目录进 dist/ 时属性一起带过去，签到某个 helper 就崩。
// 同一份代码打到 /tmp 就没事——只是因为那儿不归 iCloud 管，所以这个坑很容易被误判成签名证书的问题。
//
// 清理只动构建产物，不碰 node_modules 里的源（那是别人的东西，而且 iCloud 隔一会儿还会加回去）。
//
// electron-builder afterPack hook: clear extended attributes from the packaged .app before signing.
//
// The reason is that codesign refuses files carrying com.apple.FinderInfo or a resource fork, failing
// the whole build with "resource fork, Finder information, or similar detritus not allowed".
//
// Those attributes do not come from packaging — they come from iCloud Drive: with the project under
// Desktop and "Desktop & Documents" syncing enabled, the file provider stamps FinderInfo onto the
// bundle directories inside node_modules/electron/dist, electron-builder copies those directories into
// dist/ with the attributes attached, and signing dies on one of the helpers. The same source builds
// fine into /tmp purely because iCloud does not manage it, which makes this very easy to misread as a
// signing-certificate problem.
//
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
    // 清不掉就让签名自己去报错，别把失败藏在这儿
    // If it cannot be cleared, let signing report the failure rather than hiding it here
    console.warn("[xattr] could not clear attributes: " + ((e && e.message) || e));
  }
};
