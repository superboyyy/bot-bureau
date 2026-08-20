"use strict";

// electron-builder takes a single afterPack, so the steps are chained here in order.

// The order matters: things are added to the bundle first (the icon asset catalog) and attributes are
// cleared last. The cleanup has to be the final move before signing, or files written by a later step
// carry attributes in and codesign refuses them all the same.
const steps = [
  require("./linux-apparmor"),    // Ubuntu 24.04+ userns profile, Linux only
  require("./mac-liquid-icon"),   // .icon → Assets.car + CFBundleIconName
  require("./strip-xattr"),       // clear attributes, has to be last
];

exports.default = async function afterPack(context) {
  for (const step of steps) {
    await (typeof step === "function" ? step : step.default)(context);
  }
};
