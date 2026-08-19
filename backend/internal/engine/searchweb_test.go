package engine

import (
	"botbureau/backend/internal/model"

	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const ddgFixture = `<!DOCTYPE html><html><body>
<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc">The Go Programming Language</a>
<a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc">Official docs for the Go language.</a>
<a class="result__a" href="https://pkg.go.dev/std">Go Packages</a>
<a class="result__snippet">The standard library.</a>
<a class="result__a" href="http://localhost/admin">Local admin</a>
<a class="result__snippet">must never be returned</a>
<a class="result__a" href="http://127.0.0.1/secret">Loopback secret</a>
<a class="result__snippet">must never be returned either</a>
<a class="result__a" href="ftp://files.example.com/x">FTP listing</a>
</body></html>`

const braveFixture = `{
  "web": {
    "results": [
      {"title": "Brave One", "url": "https://example.com/one", "description": "first hit"},
      {"title": "Local", "url": "http://127.0.0.1/x", "description": "drop this"},
      {"title": "Brave Two", "url": "https://example.com/two", "description": "second hit"}
    ]
  }
}`

const tavilyFixture = `{
  "results": [
    {"title": "Tavily One", "url": "https://example.org/a", "content": "alpha"},
    {"title": "Tavily One", "url": "https://example.org/a", "content": "duplicate url"},
    {"title": "Tavily Two", "url": "https://example.org/b", "content": "beta"}
  ]
}`

func clearSearchProviderKeys(t *testing.T) {
	t.Helper()
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
}

func stubSearchDo(t *testing.T, fn func(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) ([]byte, string, int, error)) {
	t.Helper()
	origDo, origDDG, origBrave, origTavily := searchDo, ddgSearchURL, braveSearchURL, tavilySearchURL
	t.Cleanup(func() {
		searchDo = origDo
		ddgSearchURL = origDDG
		braveSearchURL = origBrave
		tavilySearchURL = origTavily
	})
	if fn != nil {
		searchDo = fn
	}
}

func TestParseDDGHTMLUnwrapsAndKeepsPublicHits(t *testing.T) {
	hits := parseDDGHTML([]byte(ddgFixture))
	if len(hits) < 2 {
		t.Fatalf("expected public rows plus the ones the filter will drop, got %#v", hits)
	}
	if hits[0].Title != "The Go Programming Language" || hits[0].URL != "https://go.dev/doc" {
		t.Fatalf("uddg should unwrap to the real page: %#v", hits[0])
	}
	if !strings.Contains(hits[0].Snippet, "Official docs") {
		t.Fatalf("snippet: %#v", hits[0])
	}
	if hits[1].URL != "https://pkg.go.dev/std" || hits[1].Title != "Go Packages" {
		t.Fatalf("direct href: %#v", hits[1])
	}

	pub := publicSearchHits(hits, maxSearchHits)
	if len(pub) != 2 {
		t.Fatalf("localhost, loopback and ftp must be dropped: %#v", pub)
	}
	for _, h := range pub {
		if strings.Contains(h.URL, "127.0.0.1") || strings.Contains(h.URL, "localhost") {
			t.Fatalf("private result leaked: %#v", h)
		}
	}
}

func TestParseBraveAndTavilyJSON(t *testing.T) {
	brave, err := parseBraveJSON([]byte(braveFixture))
	if err != nil || len(brave) != 3 {
		t.Fatalf("brave: %v %#v", err, brave)
	}
	if brave[0].Title != "Brave One" || brave[0].Snippet != "first hit" {
		t.Fatalf("brave row: %#v", brave[0])
	}
	pub := publicSearchHits(brave, maxSearchHits)
	if len(pub) != 2 || pub[0].URL != "https://example.com/one" {
		t.Fatalf("brave filter: %#v", pub)
	}

	tv, err := parseTavilyJSON([]byte(tavilyFixture))
	if err != nil || len(tv) != 3 {
		t.Fatalf("tavily: %v %#v", err, tv)
	}
	pub = publicSearchHits(tv, maxSearchHits)
	if len(pub) != 2 || pub[0].URL != "https://example.org/a" || pub[1].URL != "https://example.org/b" {
		t.Fatalf("tavily should drop the duplicate url: %#v", pub)
	}
}

func TestPublicSearchHitsCapsAtEight(t *testing.T) {
	var hits []searchHit
	for i := 0; i < 12; i++ {
		hits = append(hits, searchHit{Title: "n", URL: "https://example.com/" + strings.Repeat("x", i+1)})
	}
	if got := publicSearchHits(hits, maxSearchHits); len(got) != 8 {
		t.Fatalf("cap: %d", len(got))
	}
}

func TestWebSearchExecuteNeverAsksAndFormatsRows(t *testing.T) {
	clearSearchProviderKeys(t)
	tb, bus := fetchToolbox(t)
	stubSearchDo(t, func(_ context.Context, method, rawURL string, _ map[string]string, _ []byte) ([]byte, string, int, error) {
		if method != http.MethodGet || !strings.Contains(rawURL, "duckduckgo.com") {
			t.Fatalf("default engine should GET DuckDuckGo HTML, got %s %s", method, rawURL)
		}
		if !strings.Contains(rawURL, "q=") {
			t.Fatalf("query should be on the URL: %s", rawURL)
		}
		return []byte(ddgFixture), "text/html", 200, nil
	})

	out, _, isErr := tb.Execute("web_search", map[string]any{"query": "golang docs"})
	if isErr {
		t.Fatalf("search should succeed: %q", out)
	}
	if !strings.Contains(out, "The Go Programming Language") || !strings.Contains(out, "https://go.dev/doc") {
		t.Fatalf("formatted rows: %q", out)
	}
	if strings.Contains(out, "localhost") || strings.Contains(out, "127.0.0.1") || strings.Contains(out, "must never be returned") {
		t.Fatalf("private hits must not reach the model: %q", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("web_search must never raise an approval, got %d", n)
	}

	out, _, isErr = tb.Execute("web_search", map[string]any{"query": "  "})
	if !isErr || !strings.Contains(out, "query") {
		t.Fatalf("empty query should fail: %q %v", out, isErr)
	}
}

func TestWebSearchPrefersBraveThenTavily(t *testing.T) {
	clearSearchProviderKeys(t)
	tb, _ := fetchToolbox(t)

	var gotURL, gotMethod, gotToken string
	stubSearchDo(t, func(_ context.Context, method, rawURL string, headers map[string]string, body []byte) ([]byte, string, int, error) {
		gotMethod, gotURL, gotToken = method, rawURL, headers["X-Subscription-Token"]
		if strings.Contains(rawURL, "api.search.brave.com") {
			return []byte(braveFixture), "application/json", 200, nil
		}
		if strings.Contains(rawURL, "tavily.com") {
			if !strings.Contains(string(body), "golang") {
				t.Fatalf("tavily payload should carry the query: %s", body)
			}
			return []byte(tavilyFixture), "application/json", 200, nil
		}
		t.Fatalf("unexpected search URL %s", rawURL)
		return nil, "", 0, nil
	})

	if err := tb.ks.Set("TAVILY_API_KEY", "tvly-test"); err != nil {
		t.Fatal(err)
	}
	if err := tb.ks.Set("BRAVE_API_KEY", "brave-test"); err != nil {
		t.Fatal(err)
	}
	out, _, isErr := tb.Execute("web_search", map[string]any{"query": "golang"})
	if isErr || !strings.Contains(out, "Brave One") || !strings.Contains(gotURL, "api.search.brave.com") {
		t.Fatalf("Brave key should win: out=%q url=%s err=%v", out, gotURL, isErr)
	}
	if gotMethod != http.MethodGet || gotToken != "brave-test" {
		t.Fatalf("Brave GET with the stored token: method=%s token=%s", gotMethod, gotToken)
	}
	if strings.Contains(out, "Tavily") {
		t.Fatalf("Tavily must not run while a Brave key is present: %q", out)
	}

	if !tb.ks.Delete("BRAVE_API_KEY") {
		t.Fatal("expected to remove the Brave key")
	}
	out, _, isErr = tb.Execute("web_search", map[string]any{"query": "golang"})
	if isErr || !strings.Contains(out, "Tavily One") || gotMethod != http.MethodPost || !strings.Contains(gotURL, "tavily.com") {
		t.Fatalf("Tavily next: out=%q method=%s url=%s err=%v", out, gotMethod, gotURL, isErr)
	}
}

func TestWebSearchRefusesLoopbackSearchEndpoint(t *testing.T) {
	clearSearchProviderKeys(t)
	tb, _ := fetchToolbox(t)
	stubSearchDo(t, nil) // keep the real dialer: that is the SSRF check

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()
	ddgSearchURL = srv.URL

	out, _, isErr := tb.Execute("web_search", map[string]any{"query": "anything"})
	if !isErr || strings.Contains(out, "The Go Programming Language") || strings.Contains(out, "go.dev") {
		t.Fatalf("a loopback search endpoint must not be read: %q", out)
	}
}

func TestWebSearchHTTPErrorAndNoResults(t *testing.T) {
	clearSearchProviderKeys(t)
	tb, _ := fetchToolbox(t)
	stubSearchDo(t, func(context.Context, string, string, map[string]string, []byte) ([]byte, string, int, error) {
		return []byte("nope"), "text/html", 503, nil
	})
	out, _, isErr := tb.Execute("web_search", map[string]any{"query": "x"})
	if !isErr || !strings.Contains(out, "Search failed") || !strings.Contains(out, "503") {
		t.Fatalf("HTTP error: %q", out)
	}

	stubSearchDo(t, func(context.Context, string, string, map[string]string, []byte) ([]byte, string, int, error) {
		return []byte(`<html></html>`), "text/html", 200, nil
	})
	out, _, isErr = tb.Execute("web_search", map[string]any{"query": "x"})
	if !isErr || !strings.Contains(out, "No search results") {
		t.Fatalf("empty page: %q", out)
	}
}

type webCapableProvider struct{}

func (webCapableProvider) SupportsWebTools() bool { return true }
func (webCapableProvider) Label() string          { return "claude-like" }
func (webCapableProvider) NewSession() model.Session {
	return (&scriptedProvider{}).NewSession()
}

func TestSystemPromptOffersWebSearchForEveryProvider(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	prompt := w.systemPrompt("dm")
	if !strings.Contains(prompt, "web_search") || !strings.Contains(prompt, "fetch_url") {
		t.Fatalf("engine web_search should be in the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "no search engine") {
		t.Fatal("nobody should be told they have no search engine")
	}
	if strings.Contains(prompt, "vendor's web_search") {
		t.Fatal("a fake/OpenAI-compatible member does not have vendor web tools")
	}

	names := map[string]bool{}
	for _, d := range w.toolbox.Defs() {
		names[d.Name] = true
	}
	if !names["web_search"] || !names["fetch_url"] {
		t.Fatalf("defs should always include web_search: %v", names)
	}
	if !w.toolbox.parallelizable("web_search") {
		t.Fatal("web_search is a read and should batch with the others")
	}

	cw, _, _ := newTestWorker(t, "c", webCapableProvider{})
	cPrompt := cw.systemPrompt("dm")
	if !strings.Contains(cPrompt, "vendor's web_search / web_fetch") {
		t.Fatalf("Claude-like prompt should name the vendor tools:\n%s", cPrompt)
	}
}

func TestDescribeToolCallWebSearch(t *testing.T) {
	got := describeToolCall(model.ToolCall{Name: "web_search", Input: map[string]any{"query": "golang channels"}})
	if got != "web_search: golang channels" {
		t.Fatalf("got %q", got)
	}
}
