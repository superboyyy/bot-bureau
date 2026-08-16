
// The renderer talks to the local backend over HTTP/SSE only; just two engine-switch actions are exposed here for the settings panel.
const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("botBureauNative", {
  connectTo: (url) => ipcRenderer.invoke("connect-to", url),
  connectLocal: () => ipcRenderer.invoke("connect-local"),

  // Notifies the UI when another engine turns up on the LAN; the UI decides how to hint at it
  onEnginesFound: (fn) => ipcRenderer.on("engines-found", (_e, list) => fn(list)),
});
