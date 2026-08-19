package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"

	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Fetch a web page and turn it into text.

// Why this tool exists: Claude has server-side web_search/web_fetch, OpenAI-compatible endpoints do not
// (see SupportsWebTools in providers.go). That left those bots one way onto the network — curl inside
// bash — and curl both reaches the network and writes files, with a first word nowhere near the
// read-only whitelist, so reading one page cost one approval. Which makes the most ordinary thing the
// most annoying one, and to get past it you approve a whole shell: far more than one GET.

// So reading a page becomes a first-class read-only tool that never reaches the approval gate.

// It genuinely only reads, as far as this machine is concerned — but it is not free, and that should be
// said plainly: a GET can carry things out in its URL. The trade is deliberate. Without this tool the
// model reaches for curl anyway, the user approves anyway, and what gets approved is larger. The way to
// tighten it further is the hostname allowlist in Settings: empty still means any public host;
// non-empty means only those hosts. Loopback and private addresses stay refused either way.

const (
	fetchTimeout  = 20 * time.Second
	fetchMaxBytes = 4 << 20 // stop reading here
	fetchMaxChars = 40000   // the cap on what reaches the model
)

// fetchClient's dialer checks each IP it is about to connect to, rather than glancing at the hostname
// in the URL.

// Checking the hostname does not hold: DNS can resolve to 127.0.0.1 and a redirect can lead from a
// public address into the private network, and hostname-checking code knows about neither. The only
// thing that decides is the IP actually being connected to, so the check lives in the dialer — every
// hop and every re-resolution has to come through it.
var fetchClient = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !publicIP(ip.IP) {
					return nil, fmt.Errorf(i18n.T("%s resolves to an address on this machine or this network, which fetch_url will not open"), host)
				}
			}
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		},
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New(i18n.T("too many redirects"))
		}
		if err := httpsOrHTTP(req.URL); err != nil {
			return err
		}
		return config.HostAllowedErr(req.URL.Hostname(), fetchHostsFrom(req.Context()))
	},
}

// publicIP reports whether an address is genuinely out there. Loopback, private ranges and link-local
// are all excluded: the engine itself listens on 127.0.0.1:8973, and router admin pages, printers and
// cloud metadata endpoints (169.254.169.254) live in those ranges too. A tool for reading web pages has
// no business reaching any of them.
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}

	// An IPv4-mapped address is judged again as IPv4, or ::ffff:127.0.0.1 slips past the checks above
	if v4 := ip.To4(); v4 != nil && !ip.Equal(v4) {
		return publicIP(v4)
	}
	return true
}

func httpsOrHTTP(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf(i18n.T("fetch_url only opens http and https addresses, not %s"), u.Scheme)
	}
	return nil
}

type fetchHostsCtxKey struct{}

func withFetchHosts(ctx context.Context, hosts []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(hosts) == 0 {
		return ctx
	}
	return context.WithValue(ctx, fetchHostsCtxKey{}, hosts)
}

func fetchHostsFrom(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	hosts, _ := ctx.Value(fetchHostsCtxKey{}).([]string)
	return hosts
}

func (t *Toolbox) fetchHosts() []string {
	if t == nil || t.settings == nil {
		return nil
	}
	return t.settings.FetchHosts()
}

// runFetchURL fetches one address and hands back its body. Read-only; it never reaches the gate.
func (t *Toolbox) runFetchURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return i18n.T("The address is empty"), true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return i18n.T("Not a valid address: ") + raw, true
	}
	if u.Scheme == "" {

		// Models routinely write bare example.com/x; supplying https beats making it guess again
		if u, err = url.Parse("https://" + raw); err != nil {
			return i18n.T("Not a valid address: ") + raw, true
		}
	}
	if err := httpsOrHTTP(u); err != nil {
		return err.Error(), true
	}
	hosts := t.fetchHosts()
	if err := config.HostAllowedErr(u.Hostname(), hosts); err != nil {
		return err.Error(), true
	}

	ctx := t.turnCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = withFetchHosts(ctx, hosts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return i18n.T("Not a valid address: ") + raw, true
	}

	// Plenty of sites answer 403 to a request with no user agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; BotBureau/0.1; +https://github.com/)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := fetchClient.Do(req)
	if err != nil {
		return i18n.T("Fetch failed: ") + err.Error(), true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return i18n.T("Fetch failed: ") + err.Error(), true
	}
	ctype := resp.Header.Get("Content-Type")
	text, ok := readableText(ctype, body)
	if !ok {
		return fmt.Sprintf(i18n.T("%s returned %s, which is not text — fetch_url only reads pages, not binary files"),
			u.String(), firstToken(ctype)), true
	}
	if resp.StatusCode >= 400 {

		// The error page's own text still goes back: a 4xx or 5xx body usually says what went wrong
		return fmt.Sprintf(i18n.T("HTTP %d from %s\n\n%s"), resp.StatusCode, u.String(), clip(text)), true
	}
	return clip(text), false
}

func clip(s string) string {
	if len(s) > fetchMaxChars {
		return s[:fetchMaxChars] + i18n.T("\n...(page truncated)")
	}
	return s
}

func firstToken(ctype string) string {
	if ctype == "" {
		return "?"
	}
	return strings.TrimSpace(strings.Split(ctype, ";")[0])
}

// readableText turns a response body into text the model can read, returning false when it is not text.
func readableText(ctype string, body []byte) (string, bool) {
	mime := strings.ToLower(firstToken(ctype))
	switch {
	case strings.Contains(mime, "html"), strings.Contains(mime, "xml"):
		return htmlToText(body), true
	case strings.HasPrefix(mime, "text/"), strings.Contains(mime, "json"),
		strings.Contains(mime, "javascript"), strings.Contains(mime, "csv"):
		return strings.TrimSpace(string(body)), true
	case mime == "?" || mime == "":

		// No type given: guess from the content. HTTP permits omitting Content-Type, and most such
		// responses are text.
		if s := string(body); strings.ContainsRune(s, 0) {
			return "", false
		} else if strings.Contains(strings.ToLower(s[:min(len(s), 512)]), "<html") {
			return htmlToText(body), true
		} else {
			return strings.TrimSpace(s), true
		}
	}
	return "", false
}

// htmlToText pulls the readable text out of a page. The contents of script, style and noscript are
// dropped wholesale: tens or hundreds of kilobytes of code that would only push the actual text of the
// page out of the model's context.
func htmlToText(body []byte) string {
	z := html.NewTokenizer(strings.NewReader(string(body)))
	var b strings.Builder
	skip := 0
	var title string
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			out := strings.TrimSpace(collapse(b.String()))
			if title != "" {
				out = strings.TrimSpace(title) + "\n\n" + out
			}
			return out
		case html.StartTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript", "svg", "template":
				skip++
			case "title":
				inTitle = true
			case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article":
				b.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript", "svg", "template":
				if skip > 0 {
					skip--
				}
			case "title":
				inTitle = false
			case "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article":
				b.WriteString("\n")
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			if inTitle {
				title += string(z.Text())
				continue
			}
			b.Write(z.Text())
		}
	}
}

// collapse squeezes runs of whitespace while keeping blank lines between paragraphs: web pages put
// dozens of spaces and newlines between words, and handed over as-is the whitespace alone would eat a
// good part of the context.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	newlines, space := 0, false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			newlines++
			space = false
		case r == ' ' || r == '\t':
			space = true
		default:
			if newlines > 0 {
				if newlines > 1 {
					b.WriteString("\n\n")
				} else {
					b.WriteString("\n")
				}
			} else if space && b.Len() > 0 {
				b.WriteString(" ")
			}
			newlines, space = 0, false
			b.WriteRune(r)
		}
	}
	return b.String()
}
