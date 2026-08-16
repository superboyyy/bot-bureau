package engine

import (
	"botbureau/backend/internal/config"

	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func fetchToolbox(t *testing.T) (*Toolbox, *Bus) {
	t.Helper()
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	w.toolbox.currentChat = "group"
	return w.toolbox, bus
}

// Fetching never asks: the whole point of the tool is to displace one approval per page for curl.
func TestFetchURLNeverAsksForApproval(t *testing.T) {
	if config.PermAsk.NeedsApproval(config.ToolAct{Kind: config.ActBash, ReadOnly: true}) {
		t.Fatal("read-only actions must pass at every tier, including ask")
	}
	tb, bus := fetchToolbox(t)
	out, isErr := tb.Execute("fetch_url", map[string]any{"url": "ftp://example.com/x"})
	if !isErr || !strings.Contains(out, "ftp") {
		t.Fatalf("a non-http scheme should be refused by name: %q", out)
	}

	// Even the refusal raised no approval: this tool never reaches the gate at all
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("fetch_url must never raise an approval, got %d", n)
	}
}

// Nothing on this machine or this network is reachable. The engine itself sits on 127.0.0.1:8973, and
// router admin pages, printers and cloud metadata endpoints live in those ranges too — none of which a
// page-reading tool has any business touching.
func TestFetchURLRefusesLocalAndPrivateAddresses(t *testing.T) {
	tb, _ := fetchToolbox(t)

	// A real local server, so that what stops it is the check rather than a failure to connect
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this must never be read"))
	}))
	defer srv.Close()

	out, isErr := tb.Execute("fetch_url", map[string]any{"url": srv.URL})
	if !isErr || strings.Contains(out, "this must never be read") {
		t.Fatalf("a loopback address must not be fetched: %q", out)
	}

	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "[::1]"} {
		if out, isErr := tb.Execute("fetch_url", map[string]any{"url": "http://" + ip + "/"}); !isErr {
			t.Fatalf("%s must be refused: %q", ip, out)
		}
	}
}

// publicIP is the check itself. IPv4-mapped addresses are the spelling most likely to slip through.
func TestPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.5", "172.20.0.1", "169.254.169.254",
		"::1", "fe80::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if publicIP(net.ParseIP(s)) {
			t.Fatalf("%s should not count as public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(s)) {
			t.Fatalf("%s should count as public", s)
		}
	}
}

// HTML in, readable text out: scripts and styles dropped wholesale, the title kept up front.
func TestHTMLToText(t *testing.T) {
	page := []byte(`<html><head><title>Price list</title>
<style>body{color:#fff}</style>
<script>var leak="` + strings.Repeat("x", 5000) + `";</script>
</head><body>
<h1>MacBook Pro</h1>
<p>RMB 16,499</p><p>32GB    memory</p>
<noscript>enable javascript</noscript>
</body></html>`)
	got := htmlToText(page)
	for _, want := range []string{"Price list", "MacBook Pro", "RMB 16,499", "32GB memory"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"var leak", "color:#fff", "enable javascript", "xxxxx"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("should have been dropped: %q in:\n%s", unwanted, got)
		}
	}

	// Pages put dozens of spaces between words; left alone the whitespace alone eats a lot of context
	if strings.Contains(got, "    ") {
		t.Fatalf("runs of whitespace should be collapsed:\n%q", got)
	}
}

// A non-text response is explained, rather than poured into the model's context as binary.
func TestReadableTextRejectsBinary(t *testing.T) {
	if _, ok := readableText("image/png", []byte{0x89, 'P', 'N', 'G'}); ok {
		t.Fatal("a PNG is not readable text")
	}
	if _, ok := readableText("application/json", []byte(`{"a":1}`)); !ok {
		t.Fatal("JSON is readable text")
	}
	if s, ok := readableText("text/plain; charset=utf-8", []byte(" hi ")); !ok || s != "hi" {
		t.Fatalf("plain text should come back trimmed: %q %v", s, ok)
	}
}
