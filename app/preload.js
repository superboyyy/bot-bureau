// 渲染进程与本地后端只走 HTTP/SSE；这里仅暴露两个引擎切换动作给设置面板。
// The renderer talks to the local backend over HTTP/SSE only; just two engine-switch actions are exposed here for the settings panel.
const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("botBureauNative", {
  connectTo: (url) => ipcRenderer.invoke("connect-to", url),
  connectLocal: () => ipcRenderer.invoke("connect-local"),
  // 局域网上发现别的引擎时通知界面，由界面决定怎么轻提示
  // Notifies the UI when another engine turns up on the LAN; the UI decides how to hint at it
  onEnginesFound: (fn) => ipcRenderer.on("engines-found", (_e, list) => fn(list)),
});
