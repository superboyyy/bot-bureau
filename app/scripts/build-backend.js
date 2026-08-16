"use strict";

// Builds the Go engine for a target platform into bin/, to ship inside the package.

// Why this is not a one-line `go build`: electron-builder's mac target emits both an arm64 and an x64
// dmg, while a bare go build only produces the host architecture — so the Intel dmg would carry an
// arm64 engine and simply fail to start, a breakage only Intel users would ever hit.

// On macOS the answer is a universal binary: build both architectures and lipo them into one file that
// runs on either machine, with no change needed in main.js. Other platforms cross-compile for the
// target architecture (the engine is pure Go, so CGO can stay off).
const { execFileSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const APP = path.join(__dirname, "..");
const BACKEND = path.join(APP, "..", "backend");
const BIN = path.join(APP, "bin");

function arg(name, fallback) {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

function goBuild(goos, goarch, out) {



  // Remove any previous output first: it may be a lipo-made universal binary, which go build does not
  // recognise as its own object file and refuses to overwrite ("already exists and is not an object file").
  fs.rmSync(out, { force: true });
  execFileSync("go", ["build", "-trimpath", "-ldflags", "-s -w", "-o", out, "."], {
    cwd: BACKEND,
    stdio: "inherit",
    env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: "0" },
  });
}

const platform = arg("platform", process.platform);
const arch = arg("arch", "");
fs.mkdirSync(BIN, { recursive: true });

// On macOS a universal binary is built only when no --arch is given; with one, just that architecture.
// Per-architecture packages halve each download (a universal one carries both Electron builds), while
// the universal one wins by being a single file that runs anywhere.
if (platform === "darwin" && !arch) {
  const a = path.join(BIN, ".arm64");
  const b = path.join(BIN, ".amd64");
  const out = path.join(BIN, "botbureau-backend");
  goBuild("darwin", "arm64", a);
  goBuild("darwin", "amd64", b);
  execFileSync("lipo", ["-create", a, b, "-output", out], { stdio: "inherit" });
  fs.rmSync(a, { force: true });
  fs.rmSync(b, { force: true });

  // Confirm both slices are actually there; never ship a package that only thinks it is universal
  const info = execFileSync("lipo", ["-archs", out], { encoding: "utf8" }).trim();
  if (!info.includes("arm64") || !info.includes("x86_64")) {
    throw new Error("universal binary is missing an architecture: " + info);
  }
  console.log("[backend] universal (" + info + ") → " + path.relative(APP, out));
} else {
  const goarch = (arch || process.arch) === "arm64" ? "arm64" : "amd64";
  const out = path.join(BIN, platform === "win32" ? "botbureau-backend.exe" : "botbureau-backend");
  goBuild(platform === "win32" ? "windows" : platform, goarch, out);
  console.log("[backend] " + platform + "/" + goarch + " → " + path.relative(APP, out));
}
