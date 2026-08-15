"use strict";
// electron-builder 只认一个 afterPack，所以这里按顺序把该做的几步串起来。
//
// 顺序是有讲究的：先往包里补东西（图标资源目录），最后再清扩展属性——清理必须是签名前的最后一动，
// 否则后一步往包里写的文件带着属性进去，codesign 照样拒签。
//
// electron-builder takes a single afterPack, so the steps are chained here in order.
//
// The order matters: things are added to the bundle first (the icon asset catalog) and attributes are
// cleared last. The cleanup has to be the final move before signing, or files written by a later step
// carry attributes in and codesign refuses them all the same.
const steps = [
  require("./mac-liquid-icon"),   // .icon → Assets.car + CFBundleIconName
  require("./strip-xattr"),       // 清扩展属性，必须最后 / clear attributes, has to be last
];

exports.default = async function afterPack(context) {
  for (const step of steps) {
    await (typeof step === "function" ? step : step.default)(context);
  }
};
