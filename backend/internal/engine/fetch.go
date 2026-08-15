package engine

import (
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

// 取一个网页，转成文字。
//
// 为什么要有这个工具：Claude 那边有服务端的 web_search/web_fetch，OpenAI 兼容端点没有
// （见 providers.go 的 SupportsWebTools）。于是那些 bot 唯一的上网路子是 bash 里的 curl——
// 而 curl 既联网又落盘，首词也不在只读白名单里，取一个网页就要你点一次头。结果是：
// 最常见的一件事最烦，而你为了让它过去，批出去的是一整个 shell，比一次 GET 危险得多。
//
// 所以这里把"读一个网页"做成一等公民的只读工具，不过审批门。
//
// 它对这台机器确实是只读的；但要说清楚它不是没有代价的——一个 GET 的 URL 里可以夹带东西出去。
// 这是明知的取舍：不给这个工具，模型照样会去 curl，你照样会批，而批完给出去的更多。
// 真要收紧，该加的是按 bot 的域名白名单，不是每次弹窗。
//
// Fetch a web page and turn it into text.
//
// Why this tool exists: Claude has server-side web_search/web_fetch, OpenAI-compatible endpoints do not
// (see SupportsWebTools in providers.go). That left those bots one way onto the network — curl inside
// bash — and curl both reaches the network and writes files, with a first word nowhere near the
// read-only whitelist, so reading one page cost one approval. Which makes the most ordinary thing the
// most annoying one, and to get past it you approve a whole shell: far more than one GET.
//
// So reading a page becomes a first-class read-only tool that never reaches the approval gate.
//
// It genuinely only reads, as far as this machine is concerned — but it is not free, and that should be
// said plainly: a GET can carry things out in its URL. The trade is deliberate. Without this tool the
// model reaches for curl anyway, the user approves anyway, and what gets approved is larger. The way to
// tighten it is a per-bot domain allowlist, not a prompt every time.

const (
	fetchTimeout  = 20 * time.Second
	fetchMaxBytes = 4 << 20 // 读到这里就停 / stop reading here
	fetchMaxChars = 40000   // 交给模型的上限 / the cap on what reaches the model
)

// fetchClient 的拨号器逐个 IP 地检查，而不是只看一眼 URL 里的主机名。
//
// 看主机名是挡不住的：DNS 可以解到 127.0.0.1，跳转可以从公网地址转进内网，而检查主机名的代码
// 对这两件事一无所知。真正连出去的那个 IP 才是唯一说了算的东西，所以判定放在拨号这一层——
// 每一跳、每一次重解析都必然经过它。
//
// fetchClient's dialer checks each IP it is about to connect to, rather than glancing at the hostname
// in the URL.
//
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
		return httpsOrHTTP(req.URL)
	},
}

// publicIP 判断一个地址是不是真的在外面。本机、局域网、链路本地一律不算——
// 引擎自己就监听在 127.0.0.1:8973，路由器管理页、打印机、云厂商的元数据地址（169.254.169.254）
// 也都在这些段里。一个"读网页"的工具没有任何理由能够到它们。
//
// publicIP reports whether an address is genuinely out there. Loopback, private ranges and link-local
// are all excluded: the engine itself listens on 127.0.0.1:8973, and router admin pages, printers and
// cloud metadata endpoints (169.254.169.254) live in those ranges too. A tool for reading web pages has
// no business reaching any of them.
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// IPv4 映射地址要按 IPv4 再判一次，否则 ::ffff:127.0.0.1 会从上面漏过去
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

// runFetchURL 取一个地址并交回正文。只读，不过审批门。
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
		// 模型常常只写 example.com/x；补一个 https 比让它自己再试一遍强
		// Models routinely write bare example.com/x; supplying https beats making it guess again
		if u, err = url.Parse("https://" + raw); err != nil {
			return i18n.T("Not a valid address: ") + raw, true
		}
	}
	if err := httpsOrHTTP(u); err != nil {
		return err.Error(), true
	}

	ctx := t.turnCtx
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return i18n.T("Not a valid address: ") + raw, true
	}
	// 不少站点对没有 UA 的请求直接返 403
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
		// 出错页的正文照样给出去：4xx/5xx 的页面里常常写着到底哪儿不对
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

// readableText 把响应体变成模型读得懂的文字，认不出是文本时返回 false。
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
		// 没说类型：按内容猜。HTTP 允许不带 Content-Type，而多数这种响应是文本。
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

// htmlToText 抽出一页里能读的文字。script/style/noscript 的正文整段丢掉——
// 那是几十上百 KB 的代码，塞给模型只会把真正的正文挤出上下文。
//
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

// collapse 把连片的空白压掉，但保留段落之间的空行——网页里一个词之间动辄几十个空格和换行，
// 原样交出去，光是空白就能占掉上下文的一大截。
//
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
