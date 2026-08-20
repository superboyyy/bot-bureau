
// Bot Bureau Electron main process: spawn the Go backend child process, create windows, clean up on exit.
const { app, BrowserWindow, dialog, ipcMain, shell, Menu, nativeTheme, Tray } = require("electron");
const { spawn } = require("child_process");
const { isRunning, stopEngine } = require("./lib/stop-engine");
const { linuxNeedsNoSandbox } = require("./lib/linux-runtime");
const net = require("net");
const https = require("https");
const crypto = require("crypto");
const path = require("path");
const fs = require("fs");
const { Bonjour } = require("bonjour-service");

// Main-process i18n: the source is English and Chinese comes from the same translation table the
// renderer uses, so one sentence cannot end up worded differently in a window and in a dialog.
// app.getLocale() can return an empty string before the app is ready, so resolve on every call.
let localeTable = null;
function t(en, ...args) {
  if (localeTable === null) {
    localeTable = {};
    try {
      const raw = fs.readFileSync(path.join(__dirname, "renderer", "locales", "zh.js"), "utf8");
      const marker = "window.__i18n.zh = ";
      const body = raw.slice(raw.indexOf(marker) + marker.length, raw.lastIndexOf("}") + 1);
      localeTable = JSON.parse(body);
    } catch { /* with no table everything stays English */ }
  }
  const zh = (app.getLocale() || "").toLowerCase().startsWith("zh");
  let out = (zh && localeTable[en]) || en;
  for (const a of args) out = out.replace("%s", String(a));
  return out;
}

// App name and icon: in dev mode (npm start) the Dock/menu bar would otherwise read "Electron"
app.setName("Bot Bureau");
app.setAboutPanelOptions({ applicationName: "Bot Bureau", applicationVersion: app.getVersion() });
if (process.platform === "win32") app.setAppUserModelId("app.botbureau.desktop");

// Ubuntu 24.04+ AppArmor blocks Chromium's sandbox userns unless a profile is loaded
// (the .deb installs one). Without it the process aborts with SIGTRAP before a window.
if (linuxNeedsNoSandbox()) {
  app.commandLine.appendSwitch("no-sandbox");
  console.warn("[bot-bureau] Chromium sandbox disabled (no AppArmor userns profile)");
}
// Electron 43 + native Wayland can crash while creating the window (ClientFrameViewLinux).
// Packaged Linux uses XWayland; unpackaged `npm start` keeps the session default.
if (process.platform === "linux" && app.isPackaged) {
  app.commandLine.appendSwitch("ozone-platform-hint", "x11");
}
// Windows / Linux use the full-bleed squircle (same size as the old square). macOS keeps the
// padded tile so the Dock icon matches neighbouring apps; those files are icon-mac*.png.
const ICON = path.join(__dirname, "build", process.platform === "darwin" ? "icon-mac.png" : "icon.png");
const ICON_LIGHT = path.join(__dirname, "build", process.platform === "darwin" ? "icon-mac-light.png" : "icon-light.png");
let tray = null;

// See scripts/mac-liquid-icon.js for the appearance-aware package icon.

// The icon follows the system appearance. What the package carries is fixed — an .icns holds a
// single image and macOS will not derive light and dark builds from it — so the app swaps it while
// running. It follows the *system* appearance rather than the app's own light/dark preference: the
// icon sits in the Dock among other icons, on the system's material, not on this app's canvas.
// While the app is not running, Finder and the Dock still show whatever the package baked in; real
// appearance-aware bundle icons need macOS 26's .icon format (see scripts/mac-liquid-icon.js).
function appearanceIcon() {
  const icon = nativeTheme.shouldUseDarkColors ? ICON : ICON_LIGHT;
  return fs.existsSync(icon) ? icon : (fs.existsSync(ICON) ? ICON : null);
}

function applyAppearanceIcon() {
  const icon = appearanceIcon();
  if (!icon) return;
  if (process.platform === "darwin") {
    if (app.dock) app.dock.setIcon(icon);
    return;
  }


  // No Dock on Windows or Linux: the window icon is what the taskbar follows. New windows get it at
  // construction, so this only has to catch the ones already open.
  for (const w of BrowserWindow.getAllWindows()) {
    try { w.setIcon(icon); } catch { /* unsupported on some Linux desktops */ }
  }
  if (tray && !tray.isDestroyed()) {
    try { tray.setImage(icon); } catch { /* tray may reject a swap on some desktops */ }
  }
}

// Two roots, kept strictly apart:
// ASSET_ROOT — read-only files shipped inside the package (the backend binary, icons). After
// packaging these live inside app.asar and cannot be written to.
// DATA_ROOT  — everything mutable: bots.yaml, mcp.yaml, data/, connect.json.

// In development both are the repo, so the config sits right where you are working. Once installed,
// DATA_ROOT moves to the platform's user-data location (macOS ~/Library/Application Support/Bot Bureau,
// Windows %APPDATA%, Linux ~/.config). They used to be the same path, which meant anyone who
// downloaded only the client hit a write failure into a read-only directory on first launch.
const ASSET_ROOT = path.join(__dirname, "..");

// BOTBUREAU_DATA_DIR relocates DATA_ROOT wholesale. Three uses: keeping several unrelated setups on
// one machine (work and personal, with separate bots, keys and memory); moving the ever-growing data/
// to another disk; and keeping the repo clean in development, where the default location is the repo
// root and a single run creates data/ and connect.json and rewrites bots.yaml.
function resolveDataRoot() {
  const override = (process.env.BOTBUREAU_DATA_DIR || "").trim();
  if (!override) return app.isPackaged ? app.getPath("userData") : ASSET_ROOT;

  // Expand ~ by hand: the value arrives through the environment, not a shell, so no expansion happened
  const expanded = override.startsWith("~")
    ? path.join(require("os").homedir(), override.slice(1))
    : override;
  return path.resolve(expanded);
}

const DATA_ROOT = resolveDataRoot();

// Creating the data directory is all that happens here; no seed config is copied in.

// A fresh install should open to an empty team — onboarding walks the user through their first bot,
// and the engine is fine with no bots.yaml at all. This used to ship the repo's bots.yaml inside the
// package and copy it across, which handed every user the developer's own private setup: bot names,
// personas and which vendor's key they use.
function seedDataRoot() {
  fs.mkdirSync(DATA_ROOT, { recursive: true });
  console.log("[bot-bureau] data dir: " + DATA_ROOT);
}
let backendProc = null;
let backendURL = null;
let remoteMode = false;
let localToken = "";
let myInstance = ""; // our own engine's id, used to drop ourselves from discovery
let quitting = false;
let engineStopped = false;
let engineStopping = false;

// One desktop process per user-data dir. A second launch should restore the existing window
// (especially after close-to-tray) rather than spawn another engine against the same lock.
const isPrimary = app.requestSingleInstanceLock();
if (!isPrimary) app.quit();

function trayImage() {
  if (process.platform === "win32") {
    const ico = path.join(__dirname, "build", "icon.ico");
    if (fs.existsSync(ico)) return ico;
  }
  return appearanceIcon() || ICON;
}

function destroyTray() {
  if (!tray) return;
  try { tray.destroy(); } catch { /* already gone */ }
  tray = null;
}

function rebuildTrayMenu() {
  if (!tray || tray.isDestroyed()) return;
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: t("Open Bot Bureau"), click: () => restoreMainWindow() },
    { type: "separator" },
    { label: t("Quit"), click: () => app.quit() },
  ]));
}

function ensureTray() {
  if (process.platform === "darwin") return false;
  if (tray && !tray.isDestroyed()) return true;
  const image = trayImage();
  if (!image) return false;
  try {
    tray = new Tray(image);
  } catch (err) {
    console.error("[bot-bureau] tray unavailable:", err);
    tray = null;
    return false;
  }
  tray.setToolTip("Bot Bureau");
  tray.setIgnoreDoubleClickEvents(true);
  rebuildTrayMenu();
  tray.on("click", () => restoreMainWindow());
  return true;
}

function restoreMainWindow() {
  let win = BrowserWindow.getAllWindows().find((w) => !w.isDestroyed());
  if (!win) {
    if (backendURL) win = createWindow();
    else return;
  }
  try { win.setSkipTaskbar(false); } catch { /* some Linux WMs reject this */ }
  if (win.isMinimized()) win.restore();
  win.show();
  win.focus();
  destroyTray();
}

function hideToTray(win) {
  if (!ensureTray()) {
    if (win && !win.isDestroyed()) win.minimize();
    return;
  }
  if (!win || win.isDestroyed()) return;
  win.hide();
  try { win.setSkipTaskbar(true); } catch { /* some Linux WMs reject this */ }
}

async function stopLocalEngine() {
  const proc = backendProc;
  backendProc = null;
  if (!proc) return;
  await stopEngine(proc);
}

// LAN discovery: find a Bot Bureau engine already running on the same network (mDNS _botbureau._tcp)
function discoverEngines(ms = 1500) {
  return new Promise((resolve) => {
    let bonjour;
    try {
      bonjour = new Bonjour();
    } catch {
      return resolve([]);
    }
    const found = [];
    const browser = bonjour.find({ type: "botbureau" }, (s) => {
      const addr = (s.addresses || []).find((a) => /^\d+\.\d+\.\d+\.\d+$/.test(a)) || s.host;
      if (addr && s.port && !found.some((f) => f.url === `http://${addr}:${s.port}`)) {
        found.push({ name: s.name || addr, url: `http://${addr}:${s.port}` });
      }
    });
    setTimeout(() => {
      try { browser.stop(); bonjour.destroy(); } catch {}
      resolve(found);
    }, ms);
  });
}

function readLocalToken() {
  try {
    return fs.readFileSync(path.join(DATA_ROOT, "data", "token"), "utf8").trim();
  } catch {
    return "";
  }
}

// The backend creates data/token immediately before it starts listening, but keep the hand-off
// tolerant of filesystem scheduling (especially on first launch or a synced data directory). The
// renderer must never be shown before it has the local credential, otherwise its first /api/state
// request can race the token write and turn a healthy local engine into a pairing-code prompt.
async function waitForLocalToken(timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const token = readLocalToken();
    if (token) return token;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  return readLocalToken();
}

// Manually saved remote engine address (cross-network cases: Tailscale IP, etc.); enter once, remembered from then on
const CONNECT_FILE = path.join(DATA_ROOT, "connect.json");

function readSavedRemote() {
  try {
    const cfg = JSON.parse(fs.readFileSync(CONNECT_FILE, "utf8"));
    const url = cfg.remote_url;
    if (typeof url !== "string" || !/^https?:\/\//.test(url)) return null;
    return { url: url.replace(/\/+$/, ""), fp: cfg.fingerprint || null };
  } catch {
    return null;
  }
}

function saveRemote(url, fp) {
  fs.writeFileSync(CONNECT_FILE, JSON.stringify({ remote_url: url, fingerprint: fp || undefined }, null, 2));
}

// ping: use fetch for http; for https, do a manual handshake to get the cert fingerprint (self-signed + fingerprint pinning, TOFU)
function pingEngineEx(url, ms = 4000) {
  if (!url.startsWith("https://")) {
    return fetch(url + "/api/ping", { signal: AbortSignal.timeout(ms) })
      .then(async (res) => {
        const j = await res.json();
        return res.ok && j.app === "botbureau" ? { ok: true, name: j.name, fp: null } : { ok: false };
      })
      .catch(() => ({ ok: false }));
  }
  return new Promise((resolve) => {
    const u = new URL(url);
    const req = https.request(
      { host: u.hostname, port: u.port || 443, path: "/api/ping", method: "GET",
        rejectUnauthorized: false, timeout: ms },
      (res) => {
        const cert = res.socket.getPeerCertificate();
        const fp = cert && cert.raw ? crypto.createHash("sha256").update(cert.raw).digest("hex") : null;
        let data = "";
        res.on("data", (d) => (data += d));
        res.on("end", () => {
          try {
            const j = JSON.parse(data);
            resolve(res.statusCode === 200 && j.app === "botbureau" ? { ok: true, name: j.name, fp } : { ok: false });
          } catch {
            resolve({ ok: false });
          }
        });
      }
    );
    req.on("error", () => resolve({ ok: false }));
    req.on("timeout", () => { req.destroy(); resolve({ ok: false }); });
    req.end();
  });
}

function fpFromPEM(pem) {
  const b64 = String(pem).replace(/-----[^-]+-----/g, "").replace(/\s+/g, "");
  return crypto.createHash("sha256").update(Buffer.from(b64, "base64")).digest("hex");
}

// Self-signed cert allow rule: only when the target is the saved remote engine and its fingerprint matches the pinned one
app.on("certificate-error", (event, _wc, urlStr, _err, certificate, callback) => {
  const saved = readSavedRemote();
  if (saved && saved.fp && urlStr.startsWith(saved.url)) {
    try {
      if (fpFromPEM(certificate.data) === saved.fp) {
        event.preventDefault();
        callback(true);
        return;
      }
    } catch {}
  }
  callback(false);
});

function findFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
    srv.on("error", reject);
  });
}

function backendCommand() {
  const name = process.platform === "win32" ? "botbureau-backend.exe" : "botbureau-backend";

  // After packaging __dirname sits inside app.asar, where a file can be read but not executed.

  // The trap is that existsSync cannot tell: Electron patches fs so any path inside the asar returns
  // true (unpacked files are even transparently redirected to app.asar.unpacked for reads), yet
  // spawning one always fails — the kernel cannot exec its way into an archive and reports ENOTDIR.
  // So app.asar.unpacked must come first; the other order reliably picks the path that can be read but
  // not run, and then waits out the full 15-second timeout. In development there is no app.asar, the
  // replace is a no-op, and both paths are the same one anyway.
  for (const dir of [path.join(__dirname.replace("app.asar", "app.asar.unpacked"), "bin"), path.join(__dirname, "bin")]) {
    const bin = path.join(dir, name);
    if (fs.existsSync(bin)) return { cmd: bin, args: [] };
  }

  // Dev fallback: use go run when there is no compiled binary (requires Go installed locally)
  return { cmd: "go", args: ["run", "./backend"], cwd: ASSET_ROOT };
}

// Prefer the fixed port 8973 (so remote peers can remember the address); fall back to a random free
// port if taken.

// The probe has to bind the same address the engine will. It used to probe 127.0.0.1 while the engine
// binds 0.0.0.0: with something already holding 0.0.0.0:8973, binding 127.0.0.1:8973 still succeeds on
// macOS, so the port was declared free, the engine then failed to bind and exited — while a ping to
// 8973 still answered, because the squatter answered it. The client quietly attached to somebody
// else's engine, which surfaced as an inexplicable demand for a pairing code.
async function pickPort(host) {
  const preferred = 8973;
  const free = await new Promise((resolve) => {
    const s = net.createServer();
    s.once("error", () => resolve(false));
    s.listen(preferred, host, () => s.close(() => resolve(true)));
  });
  return free ? preferred : findFreePort();
}

async function startBackend() {
  if (isRunning(backendProc) && backendURL && !remoteMode) {
    return backendURL; // Local engine is already running
  }
  const listen = process.env.BOTBUREAU_LOCAL_ONLY ? "local" : "lan";
  const port = await pickPort(listen === "local" ? "127.0.0.1" : "0.0.0.0");
  const { cmd, args } = backendCommand();
  backendProc = spawn(
    cmd,
    [...args, "-port", String(port), "-listen", listen,
     "-config", path.join(DATA_ROOT, "bots.yaml"),
     "-mcp", path.join(DATA_ROOT, "mcp.yaml"),
     "-data", path.join(DATA_ROOT, "data")],

    // The system language has to travel from here: the engine's "follow the system" has only
    // LANG/LC_ALL to read, and a GUI process started from a double-clicked icon has none of them set,
    // leaving it no choice but English.
    { cwd: DATA_ROOT, stdio: ["ignore", "pipe", "pipe"], windowsHide: true, env: { ...process.env, BOTBUREAU_LOCALE: app.getLocale() } }
  );
  backendProc.stdout.on("data", (d) => process.stdout.write("[backend] " + d));
  backendProc.stderr.on("data", (d) => process.stderr.write("[backend] " + d));

  // A failed spawn (missing file, no exec bit, a path inside the asar) surfaces asynchronously, and
  // with no "error" listener it becomes an uncaught exception in the main process. Catch it and carry
  // the real errno out — otherwise all one sees is "the backend did not become ready" 15 seconds
  // later, with no clue which step broke.
  let spawnErr = null;
  backendProc.on("error", (e) => { spawnErr = e; });

  const url = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    if (spawnErr) {
      throw new Error(t("Could not start the engine (%s): %s", cmd, spawnErr.code || spawnErr.message));
    }
    if (!isRunning(backendProc)) {
      throw new Error(
        t("The backend process exited (possibly a config error, or the data directory is in use by an engine on another device — see the terminal log for details)")
      );
    }
    try {
      const res = await fetch(url + "/api/ping");

      // A successful ping must also confirm our own child answered it: with the port held by someone
      // else, they answer /api/ping just the same while our engine has already failed to bind and exited.
      if (res.ok && isRunning(backendProc)) return url;
    } catch {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(t("The backend did not become ready within 15 seconds"));
}

// Choose an engine: env var > manually saved remote address > start locally.
// LAN discovery is not on this chain; it is a light hint raised after the window is up (announceDiscovery).
async function chooseBackend() {
  if (process.env.BOTBUREAU_BACKEND_URL) {
    remoteMode = true;
    return process.env.BOTBUREAU_BACKEND_URL;
  }
  const saved = readSavedRemote();
  if (saved) {
    const r = await pingEngineEx(saved.url);
    if (r.ok) {

      // https: verify the pinned certificate fingerprint; on first connect (no record) remember this one (TOFU)
      if (saved.url.startsWith("https://")) {
        if (saved.fp && r.fp !== saved.fp) {
          dialog.showMessageBoxSync({
            type: "error",
            buttons: [t("Run locally")],
            message: t("The remote engine's certificate fingerprint has changed!"),
            detail: t("%s\n\nThis may be a man-in-the-middle attack, or the other device may simply have reinstalled its engine. Once you are sure it is safe, connect again from Settings (the new fingerprint will be remembered). This connection was refused.", saved.url),
          });
          try { fs.unlinkSync(CONNECT_FILE); } catch {}
          return startLocalFallback();
        }
        if (!saved.fp && r.fp) saveRemote(saved.url, r.fp);
      }
      remoteMode = true;
      return saved.url;
    }
    const choice = dialog.showMessageBoxSync({
      type: "warning",
      buttons: [
        t("Run locally (keep the remote settings)"),
        t("Forget the remote engine and run locally"),
      ],
      defaultId: 0,
      cancelId: 0,
      message: t("Can't reach the saved remote engine"),
      detail: t("%s\n\nIt may not be running, or the network is unreachable (across networks, connect to a virtual network such as Tailscale first, or check the server's firewall).", saved.url),
    });
    if (choice === 1) {
      try { fs.unlinkSync(CONNECT_FILE); } catch {}
    }
  }

  // Engines found on the LAN do not block startup here.

  // This used to raise a native dialog that had to be answered, and choosing "connect" walked straight
  // into the pairing-code modal — so opening the app meant answering two questions first, when almost
  // everyone just wants the local setup. The local engine now always starts, and anything discovered
  // is offered without blocking once the window is up (see announceDiscovery).
  const url = await startBackend();
  localToken = await waitForLocalToken();
  myInstance = await instanceOf(url);
  return url;
}

// Native macOS vibrancy, confined to the sidebar. Not by enabling vibrancy on a region — in AppKit the
// whole window is the NSVisualEffectView — but the other way round: vibrancy on the window, a fully
// transparent webContents, and then the right half painted back over it by main's own opaque --pane,
// which leaves only the left strip showing the system blur. The renderer has to know, so the canvas
// behind the sidebar can step aside (see ?vibrancy=1 and [data-vibrancy] in style.css).
// The material is "sidebar", not "under-window": that is NSVisualEffectMaterialSidebar, the one macOS
// uses for its own sidebars, with the light and dark recipes already worked out by the system.
const VIBRANCY = process.platform === "darwin";

// Both load sites share one set of query params: the window's first load, and the full reload after
// connecting to an engine. They were written out separately once, and the result was exactly what you
// would expect — the first load got updated, the reload did not, and the params fell off on connect.
// Keep in sync with --titlebar / --pane / --text-2 in renderer/style.css.
const TITLEBAR = 56;
function overlayOpts(appearance) {
  const light = appearance === "light";
  return {
    // Transparent so the sidebar/paper shows through: Linux may put the overlay on the left
    // (over --bg) or the right (over --pane). An opaque fill became a slab on whichever side
    // the desktop did not match. Glyph colour still tracks the app theme (honoured on Windows;
    // Linux may use the GTK theme instead).
    color: "rgba(0,0,0,0)",
    symbolColor: light ? "#62626b" : "#9d9da4",
    height: TITLEBAR,
  };
}

const rendererQuery = () => ({
  backend: backendURL,
  token: localToken,
  remote: remoteMode ? "1" : "0",
  locale: app.getLocale(),
  vibrancy: VIBRANCY ? "1" : "0",
  // macOS: native traffic lights. Windows/Linux: native Window Controls Overlay; the renderer
  // falls back to HTML #winChrome only if the overlay is not actually visible.
  chrome: process.platform === "darwin" ? "native" : "auto",
});

function createWindow() {
  const win = new BrowserWindow({
    width: 1220,
    height: 820,
    minWidth: 900,
    minHeight: 600,
    title: "Bot Bureau",
    icon: appearanceIcon() || ICON,

    // Fully transparent under vibrancy, or this flat fill buries the system blur; the right half's
    // solid ground is painted by main itself. Windows/Linux may replace this once the renderer
    // reports the resolved light/dark appearance (see set-appearance).
    backgroundColor: VIBRANCY ? "#00000000" : "#0a0a0b",  // keep in sync with --bg in style.css

    // Frameless: the title bar merges into the sidebar's top strip, which doubles as the drag region.
    // macOS keeps the traffic lights. Windows and Linux use the platform Window Controls Overlay
    // (Electron 43+ follows the desktop's button layout on Linux) so min/max/close are real OS
    // widgets, not HTML. Overlay fill is transparent so it does not paint a slab over --bg or
    // --pane — Linux may put the buttons on either side. Height matches --titlebar.
    ...(process.platform === "darwin"
      ? {
          titleBarStyle: "hiddenInset",
          trafficLightPosition: { x: 16, y: 20 },

          // visualEffectState is left unset, i.e. follows the window's active state — macOS's own
          // sidebars do fade when the window loses focus, and pinning it to active reads as less native.
          ...(VIBRANCY ? { vibrancy: "sidebar" } : {}),
        }
      : {
          titleBarStyle: "hidden",
          autoHideMenuBar: true,
          titleBarOverlay: overlayOpts("dark"),
        }),
    webPreferences: {
      contextIsolation: true,
      sandbox: true,
      preload: path.join(__dirname, "preload.js"),
    },
  });

  // Forward renderer console output to the terminal for headless debugging

  // Electron 43+ passes a single event object; accept both signatures to avoid the deprecation warning
  win.webContents.on("console-message", (...args) => {
    const details = args[0] && typeof args[0] === "object" && "message" in args[0] ? args[0] : null;
    process.stdout.write("[renderer] " + (details ? details.message : args[2]) + "\n");
  });
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//.test(url)) shell.openExternal(url);
    return { action: "deny" };
  });
  win.webContents.on("will-navigate", (event, url) => {
    const dest = typeof url === "string" ? url : event.url;
    if (dest && dest !== win.webContents.getURL()) {
      event.preventDefault();
      if (/^https?:\/\//.test(dest)) shell.openExternal(dest);
    }
  });
  win.loadFile(path.join(__dirname, "renderer", "index.html"), {
    query: rendererQuery(),
  });
  attachWindowControls(win);
  return win;
}

function attachWindowControls(win) {
  const sendMaximized = () => {
    if (win.isDestroyed()) return;
    win.webContents.send("window-maximized", win.isMaximized());
  };
  win.on("maximize", sendMaximized);
  win.on("unmaximize", sendMaximized);

  // Close is not Quit. macOS: same as the yellow button (Dock). Windows/Linux: hide to the
  // tray, leaving the taskbar slot for the real minimize button. Quit is tray right-click,
  // Cmd/Ctrl+Q, or File/App menu — that sets `quitting` in before-quit.
  win.on("close", (e) => {
    if (quitting) return;
    e.preventDefault();
    const dismiss = () => {
      if (quitting || win.isDestroyed()) return;
      if (process.platform === "darwin") win.minimize();
      else hideToTray(win);
    };
    if (typeof win.isFullScreen === "function" && win.isFullScreen()) {
      win.once("leave-full-screen", dismiss);
      win.setFullScreen(false);
      return;
    }
    dismiss();
  });
}

function canvasColor(appearance) {
  return appearance === "light" ? "#eceef3" : "#0a0a0b";
}

function windowFromEvent(e) {
  return BrowserWindow.fromWebContents(e.sender);
}

ipcMain.handle("window-minimize", (e) => {
  const win = windowFromEvent(e);
  if (win && !win.isDestroyed()) win.minimize();
});
ipcMain.handle("window-maximize", (e) => {
  const win = windowFromEvent(e);
  if (!win || win.isDestroyed()) return;
  if (win.isMaximized()) win.unmaximize();
  else win.maximize();
});
ipcMain.handle("window-close", (e) => {
  const win = windowFromEvent(e);
  if (win && !win.isDestroyed()) win.close();
});
ipcMain.handle("window-is-maximized", (e) => {
  const win = windowFromEvent(e);
  return !!(win && !win.isDestroyed() && win.isMaximized());
});
ipcMain.handle("set-appearance", (e, appearance) => {
  const win = windowFromEvent(e);
  if (!win || win.isDestroyed() || VIBRANCY) return;
  const look = appearance === "light" ? "light" : "dark";
  try { win.setBackgroundColor(canvasColor(look)); } catch { /* some Linux WMs reject this */ }
  if (process.platform === "darwin" || typeof win.setTitleBarOverlay !== "function") return;
  try { win.setTitleBarOverlay(overlayOpts(look)); } catch { /* overlay not available on this shell */ }
});

function reloadAllWindows() {
  for (const w of BrowserWindow.getAllWindows()) {
    w.loadFile(path.join(__dirname, "renderer", "index.html"), {
      query: rendererQuery(),
    });
  }
}

async function startLocalFallback() {
  remoteMode = false;
  const url = await startBackend();
  localToken = await waitForLocalToken();
  return url;
}

// Renderer requests: connect to a remote engine / switch back to the local engine

// Scan the LAN once the window is up and hand the result to the renderer for a light-touch hint.
// Doing it after the window is deliberate: discovery takes over a second and must not sit in front
// of the first paint.

// Ask the engine at an address who it is. /api/ping needs no auth, so this works before pairing.
async function instanceOf(url) {
  try {
    const res = await fetch(url + "/api/ping", { signal: AbortSignal.timeout(3000) });
    const j = await res.json();
    return j.app === "botbureau" ? j.instance || "" : "";
  } catch {
    return "";
  }
}

async function announceDiscovery(win) {
  if (remoteMode) return; // already on a remote engine
  try {
    const found = await discoverEngines();

    // The local engine advertises over mDNS as well and shows up in this list. Left in, it would
    // offer to pair you with yourself on every launch.
    const others = [];
    for (const e of found) {
      const id = await instanceOf(e.url);
      if (id && id !== myInstance) others.push(e);
    }
    console.log(`[bot-bureau] LAN discovery: ${found.length} found, ${others.length} other device(s)`);
    if (!others.length || win.isDestroyed()) return;
    win.webContents.send("engines-found", others);
  } catch { /* a failed scan is not worth interrupting anyone */ }
}

ipcMain.handle("connect-to", async (_e, url) => {
  url = String(url || "").trim().replace(/\/+$/, "");
  if (!/^https?:\/\//.test(url)) {
    return { ok: false, error: t("The address must start with http:// or https://") };
  }
  const r = await pingEngineEx(url);
  if (!r.ok) {
    return { ok: false, error: t("Can't connect, or there is no Bot Bureau engine there") };
  }

  // https: remember the cert fingerprint via TOFU; reject if the fingerprint for the same address changes (anti-MITM)
  const saved = readSavedRemote();
  if (url.startsWith("https://") && saved && saved.url === url && saved.fp && r.fp !== saved.fp) {
    return {
      ok: false,
      error: t("The certificate fingerprint doesn't match the last one (this may be a man-in-the-middle attack). Once you are sure it is safe, use \"Switch to local engine\" to clear the record, then connect again."),
    };
  }
  saveRemote(url, url.startsWith("https://") ? r.fp : null);
  if (isRunning(backendProc)) {
    await stopLocalEngine(); // Stop the local engine to avoid two engines
  }
  backendURL = url;
  remoteMode = true;
  localToken = "";
  reloadAllWindows();
  return { ok: true };
});

ipcMain.handle("connect-local", async () => {
  try { fs.unlinkSync(CONNECT_FILE); } catch {}
  try {
    remoteMode = false;
    backendURL = await startBackend();
    localToken = await waitForLocalToken();
    reloadAllWindows();
    return { ok: true };
  } catch (e) {
    return { ok: false, error: String((e && e.message) || e) };
  }
});

app.whenReady().then(async () => {
  if (!isPrimary) return;
  seedDataRoot();
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    ...(process.platform === "darwin"
      ? [{
          label: "Bot Bureau",
          submenu: [
            { role: "about" },
            { type: "separator" },
            { role: "hide" },
            { role: "hideOthers" },
            { role: "unhide" },
            { type: "separator" },
            { role: "quit" },
          ],
        }]
      : [{ role: "fileMenu" }]),
    { role: "editMenu" },
    { role: "windowMenu" },
  ]));

  // nativeTheme is only usable once the app is ready, so the listener goes here, not at module scope
  applyAppearanceIcon();
  nativeTheme.on("updated", () => applyAppearanceIcon());
  try {
    backendURL = await chooseBackend();
    const win = createWindow();

    // Not awaited: discovery takes its time while the window shows regardless
    win.webContents.once("did-finish-load", () => announceDiscovery(win));
  } catch (e) {

    // Log before the dialog: showErrorBox blocks until someone clicks OK, and when the packaged app is
    // launched from a command line that box often goes unseen — it looks like a hang, and the error
    // text is lost along with it.
    console.error("[bot-bureau] startup failed:", (e && e.stack) || e);
    dialog.showErrorBox(t("Bot Bureau startup failed"), String((e && e.message) || e));
    app.quit();
    return;
  }
  app.on("activate", () => restoreMainWindow());
});

app.on("second-instance", () => restoreMainWindow());

// Close hides the window (tray / Dock); do not quit when the last window is dismissed.
app.on("window-all-closed", () => {});

// Wait for the engine tree to exit so the file lock is released before this process is gone.
app.on("before-quit", (e) => {
  if (!isPrimary) return;
  quitting = true;
  destroyTray();
  if (engineStopped) return;
  e.preventDefault();
  if (engineStopping) return;
  engineStopping = true;
  stopLocalEngine().finally(() => {
    engineStopped = true;
    app.quit();
  });
});
