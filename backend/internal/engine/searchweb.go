package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Engine-side web search. Claude already has server-side web_search; everyone else was told they
// have no search engine and only fetch_url. This tool is the same GET-only, SSRF-safe path as
// fetch_url, and it is offered to every member. A Brave or Tavily key in the store, when present,
// is used instead of the DuckDuckGo HTML page. Google is never scraped.

const maxSearchHits = 8

type searchHit struct {
	Title   string
	URL     string
	Snippet string
}

var (
	ddgSearchURL    = "https://html.duckduckgo.com/html/"
	braveSearchURL  = "https://api.search.brave.com/res/v1/web/search"
	tavilySearchURL = "https://api.tavily.com/search"
)

// searchDo is the HTTP hook search uses. Tests replace it so fixtures never leave the machine.
// Production points it at fetchClient, so loopback and private IPs fail the same way fetch_url does.
var searchDo = defaultSearchDo

func defaultSearchDo(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) ([]byte, string, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", 0, fmt.Errorf("%s", i18n.T("Not a valid address: ")+rawURL)
	}
	if err := httpsOrHTTP(u); err != nil {
		return nil, "", 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, "", 0, fmt.Errorf("%s", i18n.T("Not a valid address: ")+rawURL)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; BotBureau/0.1; +https://github.com/)")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return nil, "", 0, err
	}
	return raw, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

func (t *Toolbox) runWebSearch(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return i18n.T("query cannot be empty"), true
	}
	query = textutil.Brief(query, 200)
	ctx := t.turnCtx
	hits, err := t.searchWeb(ctx, query)
	if err != nil {
		return i18n.T("Search failed: ") + err.Error(), true
	}
	hits = publicSearchHits(hits, maxSearchHits)
	hits = filterHitsByHosts(hits, t.fetchHosts())
	if len(hits) == 0 {
		return i18n.T("No search results"), true
	}
	return formatSearchHits(hits), false
}

func (t *Toolbox) searchWeb(ctx context.Context, query string) ([]searchHit, error) {
	if t.ks != nil {
		if key := firstKey(t.ks.Get, "BRAVE_API_KEY", "BRAVE_SEARCH_API_KEY"); key != "" {
			return searchBrave(ctx, query, key)
		}
		if key := t.ks.Get("TAVILY_API_KEY"); key != "" {
			return searchTavily(ctx, query, key)
		}
	}
	return searchDDG(ctx, query)
}

func firstKey(get func(string) string, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(get(n)); v != "" {
			return v
		}
	}
	return ""
}

func searchDDG(ctx context.Context, query string) ([]searchHit, error) {
	u, err := url.Parse(ddgSearchURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()
	body, _, status, err := searchDo(ctx, http.MethodGet, u.String(), map[string]string{
		"Accept": "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8",
	}, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf(i18n.T("The search endpoint returned HTTP %d"), status)
	}
	return parseDDGHTML(body), nil
}

func searchBrave(ctx context.Context, query, key string) ([]searchHit, error) {
	u, err := url.Parse(braveSearchURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", maxSearchHits))
	u.RawQuery = q.Encode()
	body, _, status, err := searchDo(ctx, http.MethodGet, u.String(), map[string]string{
		"Accept":               "application/json",
		"X-Subscription-Token": key,
	}, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf(i18n.T("The search endpoint returned HTTP %d"), status)
	}
	return parseBraveJSON(body)
}

func searchTavily(ctx context.Context, query, key string) ([]searchHit, error) {
	payload, err := json.Marshal(map[string]any{
		"api_key": key, "query": query, "max_results": maxSearchHits,
	})
	if err != nil {
		return nil, err
	}
	body, _, status, err := searchDo(ctx, http.MethodPost, tavilySearchURL, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}, payload)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf(i18n.T("The search endpoint returned HTTP %d"), status)
	}
	return parseTavilyJSON(body)
}

func parseDDGHTML(raw []byte) []searchHit {
	z := html.NewTokenizer(bytes.NewReader(raw))
	var hits []searchHit
	var cur searchHit
	var buf strings.Builder
	inLink, inSnippet := false, false
	flush := func() {
		cur.Title = collapse(strings.TrimSpace(cur.Title))
		cur.Snippet = collapse(strings.TrimSpace(cur.Snippet))
		cur.URL = unwrapSearchURL(strings.TrimSpace(cur.URL))
		if cur.URL != "" && cur.Title != "" {
			hits = append(hits, cur)
		}
		cur = searchHit{}
	}
	for {
		switch z.Next() {
		case html.ErrorToken:
			flush()
			return hits
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			href, class := "", ""
			if hasAttr {
				for {
					k, v, more := z.TagAttr()
					switch string(k) {
					case "href":
						href = string(v)
					case "class":
						class = string(v)
					}
					if !more {
						break
					}
				}
			}
			switch string(name) {
			case "a":
				if hasClass(class, "result__a") {
					flush()
					cur.URL = href
					inLink, inSnippet = true, false
					buf.Reset()
				} else if hasClass(class, "result__snippet") {
					inSnippet, inLink = true, false
					buf.Reset()
					if cur.URL == "" && href != "" {
						cur.URL = href
					}
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) != "a" {
				continue
			}
			text := strings.TrimSpace(buf.String())
			buf.Reset()
			if inLink {
				cur.Title = text
				inLink = false
			} else if inSnippet {
				cur.Snippet = text
				inSnippet = false
			}
		case html.TextToken:
			if inLink || inSnippet {
				buf.Write(z.Text())
			}
		}
	}
}

func parseBraveJSON(raw []byte) ([]searchHit, error) {
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, r := range parsed.Web.Results {
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return hits, nil
}

func parseTavilyJSON(raw []byte) ([]searchHit, error) {
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	var hits []searchHit
	for _, r := range parsed.Results {
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return hits, nil
}

func unwrapSearchURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(u.Host, "duckduckgo.com") {
		if v := u.Query().Get("uddg"); v != "" {
			unescaped, err := url.QueryUnescape(v)
			if err == nil {
				return unescaped
			}
			return v
		}
	}
	return u.String()
}

func publicSearchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		return false
	}
	return true
}

func publicSearchHits(hits []searchHit, max int) []searchHit {
	out := make([]searchHit, 0, max)
	seen := map[string]bool{}
	for _, h := range hits {
		h.URL = unwrapSearchURL(h.URL)
		h.Title = collapse(strings.TrimSpace(h.Title))
		h.Snippet = collapse(strings.TrimSpace(h.Snippet))
		if h.Title == "" || !publicSearchURL(h.URL) || seen[h.URL] {
			continue
		}
		seen[h.URL] = true
		out = append(out, h)
		if len(out) >= max {
			break
		}
	}
	return out
}

func filterHitsByHosts(hits []searchHit, hosts []string) []searchHit {
	if len(hosts) == 0 {
		return hits
	}
	out := hits[:0]
	for _, h := range hits {
		u, err := url.Parse(h.URL)
		if err != nil || !config.HostAllowed(u.Hostname(), hosts) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func formatSearchHits(hits []searchHit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
		if h.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", textutil.Brief(h.Snippet, 300))
		}
	}
	return strings.TrimSpace(b.String())
}

func hasClass(class, name string) bool {
	for _, c := range strings.Fields(class) {
		if c == name {
			return true
		}
	}
	return false
}
