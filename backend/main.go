// Bot Bureau backend — the Go engine behind a resident team of AI members on your own machine.
// Serves HTTP + SSE APIs, consumed by the Electron client (app/) or any frontend.
// By default it listens on the LAN and advertises via mDNS; clients on the same network auto-discover it and connect directly (pairing-code auth).
package main

import (
	"botbureau/backend/internal/api"
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/netx"
	"botbureau/backend/internal/secret"

	"botbureau/backend/internal/i18n"

	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }

func main() {

	// Flag help text is needed before settings load, so preset the locale from the system (config.NewSettings later applies an explicit preference)
	i18n.SetLocale(i18n.DetectSystemLocale())

	port := flag.Int("port", 8973, i18n.T("Listen port"))
	listen := flag.String("listen", "lan", i18n.T("lan = discoverable on the LAN (0.0.0.0); local = this machine only. Both require the pairing code."))
	tlsFlag := flag.String("tls", "", i18n.T(`TLS (for public-internet access): "auto" = self-signed cert + fingerprint pinning; "cert.pem:key.pem" = your own cert; empty = plaintext (LAN / virtual network only)`))
	cfgPath := flag.String("config", "bots.yaml", i18n.T("Path to bots.yaml"))
	mcpPath := flag.String("mcp", "mcp.yaml", i18n.T("Path to mcp.yaml (plugin/connector definitions)"))

	// Running the binary directly honours BOTBUREAU_DATA_DIR too, matching the client; an explicit
	// -data still wins
	defaultData := "data"
	if v := strings.TrimSpace(os.Getenv("BOTBUREAU_DATA_DIR")); v != "" {
		defaultData = v
	}
	dataDir := flag.String("data", defaultData, i18n.T("Data directory (workspaces / memory / routines)"))
	flag.Parse()

	cfgs, err := config.LoadBotConfigs(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("Configuration error:"), err)
		os.Exit(1)
	}
	if err := ensureDir(*dataDir); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("Failed to create the data directory:"), err)
		os.Exit(1)
	}

	// Engine lock: keeps two devices from running the engine at once when the data directory is on a sync drive (bots would answer twice)
	lock, err := netx.AcquireEngineLock(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer lock.Release()

	bus := engine.NewBus()
	bus.EnableEventLog(filepath.Join(*dataDir, "events.json"))
	sched := engine.NewScheduler(bus, filepath.Join(*dataDir, "routines.json"))
	settings := config.NewSettings(*dataDir) // resolves the language preference; all text follows it
	ks := secret.NewKeyStore(filepath.Join(*dataDir, "keys.json"))
	deps := engine.NewTeamDeps(*dataDir, ks, *mcpPath)
	deps.Settings = settings // the toolbox reads the global permission tier from it
	deps.MCP.SetOnChange(func() { bus.Emit("refresh", "", "system", "mcp", nil) })
	deps.MCP.ConnectAll() // plugins connect asynchronously; won't block startup
	for _, c := range cfgs {
		w, err := engine.NewBotWorker(c, bus, sched, *dataDir, deps)
		if err != nil {
			lock.Release()
			fmt.Fprintf(os.Stderr, i18n.T("Invalid bots.yaml entry (%s): %v\n"), c.Name, err)
			os.Exit(1)
		}
		bus.Register(w)
	}
	bus.LoadGroups(filepath.Join(*dataDir, "groups.json"), filepath.Join(*dataDir, "group.json"),
		settings.GroupTitle, settings.GroupAvatar)
	for _, w := range bus.Bots() {
		w.Start()
	}
	sched.Start()

	tg := bridge.NewTGBridge(bus, ks, filepath.Join(*dataDir, "telegram.json"))
	if tg.Status()["enabled"] == true {
		if err := tg.Start(); err != nil {
			fmt.Fprintln(os.Stderr, i18n.T("Telegram bridge failed to start:"), err)
		}
	}

	app := api.NewApp(bus, sched, deps, tg, settings, cfgs, *cfgPath, *dataDir)
	handler := http.Handler(app.Handler())

	// Authentication applies in every mode, including local.

	// "Bound to 127.0.0.1" is not a security boundary: every web page open on this machine can reach
	// localhost. Local mode used to require nothing, so any site the user visited could read the whole
	// team and its history, create bots, set the global permission tier to "no approvals" and send
	// messages — which chains into running arbitrary commands on the user's machine from a web page.
	// The pairing code lives in data/token (0600): a page cannot read it, the client can.
	token, err := netx.LoadOrCreateToken(*dataDir)
	if err != nil {
		lock.Release()
		fmt.Fprintln(os.Stderr, i18n.T("Failed to generate the pairing code:"), err)
		os.Exit(1)
	}
	handler = netx.RequireToken(token, handler)

	var addr string
	if *listen == "lan" {
		addr = fmt.Sprintf(":%d", *port)
		if shutdown, err := netx.AdvertiseMDNS(*port); err != nil {
			fmt.Fprintln(os.Stderr, i18n.T("mDNS advertising failed (other devices must enter the address manually):"), err)
		} else {
			defer shutdown()
		}
		fmt.Printf(i18n.T("Bot Bureau backend listening on 0.0.0.0:%d (discoverable on the LAN)\n"), *port)
		fmt.Printf(i18n.T("Pairing code: %s (enter it once on each other device)\n"), token)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", *port)
		fmt.Printf(i18n.T("Bot Bureau backend listening on http://%s (this machine only)\n"), addr)
	}
	fmt.Printf(i18n.T("Members: %v\n"), bus.BotNames())

	// Release the engine lock when an exit signal arrives
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		lock.Release()
		os.Exit(0)
	}()

	var serveErr error
	if *tlsFlag != "" {
		var certPath, keyPath string
		if *tlsFlag == "auto" {
			var fp string
			certPath, keyPath, fp, err = netx.EnsureSelfSignedCert(*dataDir)
			if err != nil {
				lock.Release()
				fmt.Fprintln(os.Stderr, i18n.T("Failed to generate the TLS certificate:"), err)
				os.Exit(1)
			}
			fmt.Printf(i18n.T("TLS: self-signed certificate, SHA-256 fingerprint %s\n"), fp)
			fmt.Println(i18n.T("     Clients remember this fingerprint on first connect (TOFU); any later change is rejected"))
		} else {
			parts := strings.SplitN(*tlsFlag, ":", 2)
			if len(parts) != 2 {
				lock.Release()
				fmt.Fprintln(os.Stderr, i18n.T(`-tls must be "auto" or "cert.pem:key.pem"`))
				os.Exit(1)
			}
			certPath, keyPath = parts[0], parts[1]
		}
		srv := &http.Server{Addr: addr, Handler: handler, TLSConfig: netx.ModernTLSConfig()}
		fmt.Println(i18n.T("Protocol: HTTPS (client addresses must start with https://)"))
		serveErr = srv.ListenAndServeTLS(certPath, keyPath)
	} else {
		serveErr = http.ListenAndServe(addr, handler)
	}
	if serveErr != nil {
		lock.Release()
		fmt.Fprintln(os.Stderr, i18n.T("Server failed to start:"), serveErr)
		os.Exit(1)
	}
}
