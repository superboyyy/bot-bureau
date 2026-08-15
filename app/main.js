// Bot Bureau Electron 主进程：拉起 Go 后端子进程，创建窗口，退出时收尾。
// Bot Bureau Electron main process: spawn the Go backend child process, create windows, clean up on exit.
const { app, BrowserWindow, dialog, ipcMain, shell, Menu, nativeTheme } = require("electron");
const { spawn } = require("child_process");
const net = require("net");
const https = require("https");
const crypto = require("crypto");
const path = require("path");
const fs = require("fs");
const { Bonjour } = require("bonjour-service");

// 主进程文案国际化：源码写英文原文，中文从渲染层那份译文表里查——两边共用一张表，
// 不会出现同一句话在窗口里和在弹窗里翻得不一样。
// app.getLocale() 在 app ready 之前可能返回空串，所以每次调用时才求值，取不到时按 en。
//
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
    } catch { /* 查不到表就全用英文原文 / with no table everything stays English */ }
  }
  const zh = (app.getLocale() || "").toLowerCase().startsWith("zh");
  let out = (zh && localeTable[en]) || en;
  for (const a of args) out = out.replace("%s", String(a));
  return out;
}

// 应用名与图标：开发态（npm start）下 Dock/菜单栏默认显示 "Electron"，显式设置才对
// App name and icon: in dev mode (npm start) the Dock/menu bar would otherwise read "Electron"
app.setName("Bot Bureau");
app.setAboutPanelOptions({ applicationName: "Bot Bureau", applicationVersion: app.getVersion() });
const ICON = path.join(__dirname, "build", "icon.png");
const ICON_LIGHT = path.join(__dirname, "build", "icon-light.png");

// 图标跟着系统外观走。包里那张是死的——.icns 只能带一张图，macOS 不会替它派生深浅两版——
// 所以应用在跑的时候由我们自己换。跟的是**系统**外观而不是设置里那档「跟随系统/浅色/深色」：
// 图标待在 Dock 里，和别人的图标挨着，用的是系统的底，不是这个应用自己的界面。
// 应用没跑的时候，Finder 和 Dock 显示的仍是包里那张（真正随外观切换要 macOS 26 的 .icon，见
// scripts/mac-liquid-icon.js）。
//
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
  // Windows / Linux 没有 Dock，换的是窗口图标——任务栏跟着窗口走。新开的窗口在构造时就带上了，
  // 这里只管已经开着的那些。
  // No Dock on Windows or Linux: the window icon is what the taskbar follows. New windows get it at
  // construction, so this only has to catch the ones already open.
  for (const w of BrowserWindow.getAllWindows()) {
    try { w.setIcon(icon); } catch { /* 某些 Linux 桌面环境不支持 / unsupported on some Linux desktops */ }
  }
}

// 两个根目录，分得很清楚：
//   ASSET_ROOT —— 随包分发的只读资源（后端二进制、图标）。打包后它在 app.asar 里，写不进去。
//   DATA_ROOT  —— 所有会变的东西：bots.yaml、mcp.yaml、data/、connect.json。
//
// 开发态两者都是仓库目录，配置就在眼前好改；装了 app 之后 DATA_ROOT 走系统的用户数据位置
// （macOS ~/Library/Application Support/Bot Bureau，Windows %APPDATA%，Linux ~/.config）。
// 早先两者是同一个路径，那样只下载客户端的用户一启动就会因为往只读目录写文件而失败。
//
// Two roots, kept strictly apart:
//   ASSET_ROOT — read-only files shipped inside the package (the backend binary, icons). After
//                packaging these live inside app.asar and cannot be written to.
//   DATA_ROOT  — everything mutable: bots.yaml, mcp.yaml, data/, connect.json.
//
// In development both are the repo, so the config sits right where you are working. Once installed,
// DATA_ROOT moves to the platform's user-data location (macOS ~/Library/Application Support/Bot Bureau,
// Windows %APPDATA%, Linux ~/.config). They used to be the same path, which meant anyone who
// downloaded only the client hit a write failure into a read-only directory on first launch.
const ASSET_ROOT = path.join(__dirname, "..");
//
// BOTBUREAU_DATA_DIR 能整体挪走 DATA_ROOT。三个用处：一台机器上并存多套互不相干的配置
// （工作一套、私人一套，bot、key、记忆各自独立）；把会长大的 data/ 放到别的盘；
// 开发时别把仓库弄脏——开发态的默认落点就是仓库根目录，跑一次就生成 data/、connect.json
// 并改写 bots.yaml。
//
// BOTBUREAU_DATA_DIR relocates DATA_ROOT wholesale. Three uses: keeping several unrelated setups on
// one machine (work and personal, with separate bots, keys and memory); moving the ever-growing data/
// to another disk; and keeping the repo clean in development, where the default location is the repo
// root and a single run creates data/ and connect.json and rewrites bots.yaml.
function resolveDataRoot() {
  const override = (process.env.BOTBUREAU_DATA_DIR || "").trim();
  if (!override) return app.isPackaged ? app.getPath("userData") : ASSET_ROOT;
  // ~ 得自己展开：这个值来自环境变量，不经过 shell，波浪号不会被替换
  // Expand ~ by hand: the value arrives through the environment, not a shell, so no expansion happened
  const expanded = override.startsWith("~")
    ? path.join(require("os").homedir(), override.slice(1))
    : override;
  return path.resolve(expanded);
}

const DATA_ROOT = resolveDataRoot();

// 建好数据目录就够了，不往里拷任何种子配置。
//
// 装机第一次打开就该是空团队——引导会带着建第一个 bot，引擎也接受没有 bots.yaml。
// 早先这里会把仓库里的 bots.yaml 打进包再拷过去，那等于把开发机上的私人配置
// （bot 名字、人设、用哪家 key）随安装包发给每一个用户。
//
// Creating the data directory is all that happens here; no seed config is copied in.
//
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
let myInstance = ""; // 本机引擎的实例 id，用来把自己从发现结果里剔掉 / our own engine's id, used to drop ourselves from discovery

// 局域网发现：找同一网络里已在运行的 Bot Bureau 引擎（mDNS _botbureau._tcp）
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

// 手动保存的远程引擎地址（跨网络场景：Tailscale IP 等），一次填写长期记住
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

// ping：http 用 fetch；https 手动握手以拿到证书指纹（自签名 + TOFU 钉扎）
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

// 自签名证书放行规则：仅当目标是已保存的远程引擎、且指纹与钉扎一致
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
  // 打包后 __dirname 在 app.asar 里，而 asar 里的文件读得到却 exec 不了。
  //
  // 坑在于 existsSync 分辨不出这件事：Electron 给 fs 打了补丁，asar 内路径一律返回 true
  // （解包过的文件还会被透明地映射到 app.asar.unpacked 去读），可 spawn 它一定失败——
  // 内核 exec 走不进归档文件，报 ENOTDIR。所以必须先试 app.asar.unpacked：顺序反过来的话，
  // 每次都会稳定挑中那条"读得到、跑不了"的路径，然后静等 15 秒超时。开发态没有 app.asar，
  // replace 是空操作，两条路径本来就是同一条。
  //
  // After packaging __dirname sits inside app.asar, where a file can be read but not executed.
  //
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
  // 开发兜底：没有编译好的二进制时用 go run（需要本机装 Go）
  // Dev fallback: use go run when there is no compiled binary (requires Go installed locally)
  return { cmd: "go", args: ["run", "./backend"], cwd: ASSET_ROOT };
}

// 优先用固定端口 8973（远端才能记住地址），被占用时退回随机空闲端口。
//
// 探测必须绑在引擎真正要绑的那个地址上。之前这里探的是 127.0.0.1，引擎绑的却是 0.0.0.0：
// 别人已经占了 0.0.0.0:8973 时，绑 127.0.0.1:8973 在 macOS 上仍然成功，于是这里判定"端口空闲"，
// 引擎随后 bind 失败退出——而客户端 ping 8973 又能通（应答的是占着端口的那一位），
// 就这么悄悄连上了别人的引擎，界面上表现为莫名其妙要求输配对码。
//
// Prefer the fixed port 8973 (so remote peers can remember the address); fall back to a random free
// port if taken.
//
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
  if (backendProc && backendProc.exitCode === null && backendURL && !remoteMode) {
    return backendURL; // 本机引擎已在跑 / Local engine is already running
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
    // 系统语言得由这里递过去：引擎的"跟随系统"只能查 LANG/LC_ALL，而双击图标起来的
    // GUI 进程这些变量全是空的，它就只好一律判成英文。
    // The system language has to travel from here: the engine's "follow the system" has only
    // LANG/LC_ALL to read, and a GUI process started from a double-clicked icon has none of them set,
    // leaving it no choice but English.
    { cwd: DATA_ROOT, stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, BOTBUREAU_LOCALE: app.getLocale() } }
  );
  backendProc.stdout.on("data", (d) => process.stdout.write("[backend] " + d));
  backendProc.stderr.on("data", (d) => process.stderr.write("[backend] " + d));

  // spawn 本身失败（文件不存在、没有执行位、路径在 asar 里）是异步报出来的，而且没人监听
  // "error" 事件的话会变成主进程未捕获异常。接住它，把真实的 errno 带出去——否则只能看到
  // 15 秒后一句"引擎没能就绪"，完全不知道是哪一步坏了。
  //
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
    if (backendProc.exitCode !== null) {
      throw new Error(
        t("The backend process exited (possibly a config error, or the data directory is in use by an engine on another device — see the terminal log for details)")
      );
    }
    try {
      const res = await fetch(url + "/api/ping");
      // ping 通了还要确认应答的确实是我们这个子进程：端口被别人占着时，
      // 对方一样会应答 /api/ping，而我们的引擎其实已经 bind 失败退出了。
      // A successful ping must also confirm our own child answered it: with the port held by someone
      // else, they answer /api/ping just the same while our engine has already failed to bind and exited.
      if (res.ok && backendProc.exitCode === null) return url;
    } catch {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(t("The backend did not become ready within 15 seconds"));
}

// 选择引擎：环境变量 > 手动保存的远程地址 > 本机启动。
// 局域网发现不在这条链上——那是窗口起来之后的轻提示，见 announceDiscovery。
//
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
      // https：校验钉扎的证书指纹；首次连接（无记录）则记住本次指纹（TOFU）
      // https: verify the pinned certificate fingerprint; on first connect (no record) remember this one (TOFU)
      if (saved.url.startsWith("https://")) {
        if (saved.fp && r.fp !== saved.fp) {
          dialog.showMessageBoxSync({
            type: "error",
            buttons: [t("Run locally")],
            message: t("The remote engine's certificate fingerprint has changed!"),
            detail: t(`${saved.url}\n\nThis may be a man-in-the-middle attack, or the other device may simply have reinstalled its engine. Once you are sure it is safe, connect again from Settings (the new fingerprint will be remembered). This connection was refused.`),
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
      detail: t(`${saved.url}\n\nIt may not be running, or the network is unreachable (across networks, connect to a virtual network such as Tailscale first, or check the server's firewall).`),
    });
    if (choice === 1) {
      try { fs.unlinkSync(CONNECT_FILE); } catch {}
    }
  }
  // 局域网上发现别的引擎，不在这里拦人。
  //
  // 以前这里会弹一个必须作答的原生对话框，选了"连过去"紧接着又被配对码弹窗拦住——
  // 结果是"打开应用先做两道题"，而绝大多数人只是想用本机这一套。
  // 现在一律先把本机引擎起起来，发现结果等窗口出来之后再非阻塞地提示（见 announceDiscovery）。
  //
  // Engines found on the LAN do not block startup here.
  //
  // This used to raise a native dialog that had to be answered, and choosing "connect" walked straight
  // into the pairing-code modal — so opening the app meant answering two questions first, when almost
  // everyone just wants the local setup. The local engine now always starts, and anything discovered
  // is offered without blocking once the window is up (see announceDiscovery).
  const url = await startBackend();
  localToken = readLocalToken();
  myInstance = await instanceOf(url);
  return url;
}

// macOS 原生 vibrancy，只作用在侧栏。
// 做法不是"给某个区域开 vibrancy"——AppKit 里整扇窗户才是一个 NSVisualEffectView。
// 而是反过来：窗口整体开 vibrancy + webContents 全透明，然后右侧那半靠 main 自己那层
// 不透明的 --pane 盖回去，于是只剩左边这一条露出系统模糊。渲染器需要知道这件事，
// 才能让侧栏底下的画布让开（见 ?vibrancy=1 和 style.css 里的 [data-vibrancy]）。
// 用 "sidebar" 这个材质而不是 "under-window"：它就是 NSVisualEffectMaterialSidebar，
// macOS 自家侧栏用的那一档，深浅两套的配方都由系统给好。
//
// Native macOS vibrancy, confined to the sidebar. Not by enabling vibrancy on a region — in AppKit the
// whole window is the NSVisualEffectView — but the other way round: vibrancy on the window, a fully
// transparent webContents, and then the right half painted back over it by main's own opaque --pane,
// which leaves only the left strip showing the system blur. The renderer has to know, so the canvas
// behind the sidebar can step aside (see ?vibrancy=1 and [data-vibrancy] in style.css).
// The material is "sidebar", not "under-window": that is NSVisualEffectMaterialSidebar, the one macOS
// uses for its own sidebars, with the light and dark recipes already worked out by the system.
const VIBRANCY = process.platform === "darwin";

// 两处加载共用同一组查询参数：窗口首次加载，和连接上引擎后的整窗重载。
// 分开写过一次，结果是改了首次加载、忘了重载那份，连上引擎之后参数就丢了。
// Both load sites share one set of query params: the window's first load, and the full reload after
// connecting to an engine. They were written out separately once, and the result was exactly what you
// would expect — the first load got updated, the reload did not, and the params fell off on connect.
const rendererQuery = () => ({
  backend: backendURL,
  token: localToken,
  remote: remoteMode ? "1" : "0",
  locale: app.getLocale(),
  vibrancy: VIBRANCY ? "1" : "0",
});

function createWindow() {
  const win = new BrowserWindow({
    width: 1220,
    height: 820,
    minWidth: 900,
    minHeight: 600,
    title: "Bot Bureau",
    icon: appearanceIcon() || ICON,
    // vibrancy 下必须全透明，否则这层实色把系统模糊整个盖掉；右侧的实色由 main 自己铺
    // Fully transparent under vibrancy, or this flat fill buries the system blur; the right half's
    // solid ground is painted by main itself
    backgroundColor: VIBRANCY ? "#00000000" : "#0a0a0b",  // 与 style.css 的 --bg 保持一致 / keep in sync with --bg in style.css
    // 无边框：标题栏并进侧栏顶部，红绿灯浮在上面，顶部空条负责拖拽
    // Frameless: the title bar merges into the sidebar's top strip, which doubles as the drag region
    ...(process.platform === "darwin"
      ? {
          titleBarStyle: "hiddenInset",
          trafficLightPosition: { x: 16, y: 20 },
          // visualEffectState 不指定，走系统默认（跟随窗口激活状态）：
          // macOS 自家的侧栏失焦时就是会褪一档，钉成 active 反而不像原生。
          // visualEffectState is left unset, i.e. follows the window's active state — macOS's own
          // sidebars do fade when the window loses focus, and pinning it to active reads as less native.
          ...(VIBRANCY ? { vibrancy: "sidebar" } : {}),
        }
      : { titleBarStyle: "hidden", titleBarOverlay: { color: "#0a0a0b", symbolColor: "#9d9da4", height: 56 } }),
    webPreferences: {
      contextIsolation: true,
      sandbox: true,
      preload: path.join(__dirname, "preload.js"),
    },
  });
  // 渲染进程的 console 转发到终端，便于无头排查
  // Forward renderer console output to the terminal for headless debugging
  // Electron 43 起改为传一个事件对象；这里两种签名都兼容，避免弃用告警
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
  return win;
}

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
  localToken = readLocalToken();
  return url;
}

// 渲染器请求：连接远程引擎 / 切回本机引擎
// Renderer requests: connect to a remote engine / switch back to the local engine
// 窗口出来之后再扫一遍局域网，把结果推给渲染层去做轻提示。
// 放在窗口之后是刻意的：发现要花一秒多，不该让它挡在第一屏前面。
//
// Scan the LAN once the window is up and hand the result to the renderer for a light-touch hint.
// Doing it after the window is deliberate: discovery takes over a second and must not sit in front
// of the first paint.
// 问一个地址上的引擎「你是谁」。/api/ping 免认证，所以没配对也问得到。
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
  if (remoteMode) return; // 已经连着远程引擎了，没什么可提示的 / already on a remote engine
  try {
    const found = await discoverEngines();
    // 本机引擎自己也在 mDNS 上广播，会出现在这份名单里。不剔掉的话，
    // 每次打开都会提示你"跟自己配对"。
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
  } catch { /* 发现失败不值得打扰用户 / a failed scan is not worth interrupting anyone */ }
}

ipcMain.handle("connect-to", async (_e, url) => {
  url = String(url || "").trim().replace(/\/+$/, "");
  if (!/^https?:\/\//.test(url)) {
    return { ok: false, error: t("The address must start with http(s)://") };
  }
  const r = await pingEngineEx(url);
  if (!r.ok) {
    return { ok: false, error: t("Can't connect, or there is no Bot Bureau engine there") };
  }
  // https：TOFU 记住证书指纹；同地址指纹变化则拒绝（防中间人）
  // https: remember the cert fingerprint via TOFU; reject if the fingerprint for the same address changes (anti-MITM)
  const saved = readSavedRemote();
  if (url.startsWith("https://") && saved && saved.url === url && saved.fp && r.fp !== saved.fp) {
    return {
      ok: false,
      error: t("The certificate fingerprint doesn't match the last one (this may be a man-in-the-middle attack). Once you are sure it is safe, use \"Switch to local engine\" to clear the record, then connect again."),
    };
  }
  saveRemote(url, url.startsWith("https://") ? r.fp : null);
  if (backendProc && backendProc.exitCode === null) {
    backendProc.kill(); // 本机不再跑引擎，避免双引擎 / Stop the local engine to avoid two engines
    backendProc = null;
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
    localToken = readLocalToken();
    reloadAllWindows();
    return { ok: true };
  } catch (e) {
    return { ok: false, error: String((e && e.message) || e) };
  }
});

app.whenReady().then(async () => {
  seedDataRoot();
  if (process.platform === "darwin") {
    Menu.setApplicationMenu(Menu.buildFromTemplate([
      {
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
      },
      { role: "editMenu" },
      { role: "windowMenu" },
    ]));
  }
  // nativeTheme 要等 app ready 才能用，所以监听装在这儿而不是模块顶上
  // nativeTheme is only usable once the app is ready, so the listener goes here, not at module scope
  applyAppearanceIcon();
  nativeTheme.on("updated", () => applyAppearanceIcon());
  try {
    backendURL = await chooseBackend();
    const win = createWindow();
    // 不 await：发现慢慢跑，窗口该显示就显示
    // Not awaited: discovery takes its time while the window shows regardless
    win.webContents.once("did-finish-load", () => announceDiscovery(win));
  } catch (e) {
    // 先打日志再弹框：showErrorBox 会一直阻塞到有人点确定，从命令行跑打包好的 app 时
    // 那个框往往没人看见，表现成"卡住不动"，错误内容也就跟着一起丢了。
    //
    // Log before the dialog: showErrorBox blocks until someone clicks OK, and when the packaged app is
    // launched from a command line that box often goes unseen — it looks like a hang, and the error
    // text is lost along with it.
    console.error("[bot-bureau] startup failed:", (e && e.stack) || e);
    dialog.showErrorBox(t("Bot Bureau startup failed"), String((e && e.message) || e));
    app.quit();
    return;
  }
  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0 && backendURL) createWindow();
  });
});

// 桌面工具的语义：窗口全关即退出（连同后端）
// Desktop-tool semantics: quit when all windows are closed (backend included)
app.on("window-all-closed", () => app.quit());
app.on("quit", () => {
  if (backendProc && backendProc.exitCode === null) backendProc.kill();
});
