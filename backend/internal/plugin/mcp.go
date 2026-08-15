package plugin

// MCP（Model Context Protocol）客户端：Bot Bureau 的「插件」机制。
// 引擎级实现，任何 provider 的 bot 都能用同一批插件工具。
// 支持两种传输：stdio（本地命令插件）与 Streamable HTTP（远程连接器）。
// MCP (Model Context Protocol) client: Bot Bureau's "plugin" mechanism.
// Implemented at the engine level, so bots on any provider can use the same set of plugin tools.
// Two transports are supported: stdio (local command plugins) and Streamable HTTP (remote connectors).

import (
	"botbureau/backend/internal/secret"
	"botbureau/backend/internal/textutil"

	"botbureau/backend/internal/i18n"

	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// 插件名的取值范围和 bot 名一样：小写字母数字与 -/_，因为它要拼进工具名 mcp_<插件>_<工具>。
// A plugin name uses the same alphabet as a bot name — lowercase, digits, - and _ — because it is
// spliced into the tool name mcp_<plugin>_<tool>.
// 插件返回的内容同样要限长：一个 list_directory 打到几万行不是稀奇事。
// Plugin output is capped too: a list_directory returning tens of thousands of lines is unremarkable.
const ToolOutputLimit = 20000

// 单张图的 base64 上限。一张 4K 截图 base64 之后几 MB 很常见，整个塞进上下文
// 既贵又能把这一轮直接撑爆；超了就丢，但会明说丢了什么。
// Cap on one image's base64 payload. A 4K screenshot routinely exceeds several megabytes once encoded,
// and pushing that into the context is both expensive and enough to blow up the turn; anything larger is
// dropped, with a note saying so.
const ImageDataLimit = 4 << 20

var pluginNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)

const (
	// 我们提议的版本。服务端在 initialize 的回复里说定用哪个，以它为准（见 handshake）。
	// 规范往前走时把这里抬上去即可，不用改别处。
	// The version we propose. The server names the one actually used in its initialize reply, and that
	// wins (see handshake). Raise this as the spec moves; nothing else needs touching.
	mcpProtocolVersion = "2025-06-18"
	mcpInitTimeout     = 15 * time.Second
	// 本地插件第一次启动往往要 npx/uvx 现下整个包，几十秒是常态。按 15 秒算，
	// 冷缓存下「点安装」必然先失败一次，而失败的原因（在下载）用户完全看不出来。
	// 远程连接器没有这个理由，仍旧要求它立刻响应。
	//
	// A local plugin's first start usually has npx/uvx download the whole package, which routinely takes
	// tens of seconds. At 15s, a click on "install" is bound to fail the first time on a cold cache — and
	// nothing about the failure tells the user it was still downloading. A remote connector has no such
	// excuse and is still held to 15s.
	mcpStdioInitTimeout = 180 * time.Second
	mcpCallTimeout      = 120 * time.Second
	// 插件起不来时，它 stderr 的最后几行往往是唯一的线索，留一段。
	// When a plugin fails to start, the tail of its stderr is often the only clue — keep some.
	stderrTailLimit = 4000
	// 掉线后的重连退避。次数封顶，之后停下等人工——一个一起来就崩的插件不该被无限重启。
	// Backoff after a disconnection. The attempt count is capped and then waits for a human: a plugin that
	// crashes on start should not be restarted forever.
	reconnectBackoffMin = 2 * time.Second
	reconnectBackoffMax = 30 * time.Second
	reconnectMaxTries   = 5
)

// ---- 配置 ----
// ---- configuration ----

type MCPServerConfig struct {
	Name    string   `yaml:"name" json:"name"`
	Command string   `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string `yaml:"args,omitempty" json:"args,omitempty"`
	// 值以 $ 开头时从密钥仓库/环境变量解析
	// values starting with $ are resolved from the key store / environment variables
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	URL string            `yaml:"url,omitempty" json:"url,omitempty"`
	// 密钥仓库/环境变量名
	// name of a key store entry / environment variable
	BearerKey string `yaml:"bearer_key,omitempty" json:"bearer_key,omitempty"`
	// 只暴露这几个工具给模型；留空 = 全部。
	// 大插件是必须有这个的：GitHub 官方 MCP 有九十多个工具，整包塞进工具列表既撑上下文，
	// 又明显拉低模型选对工具的概率。勾选留在配置里而不是每次连接时协商，是因为它是用户的
	// 意图，不该被服务端的工具列表变动悄悄覆盖。
	//
	// Expose only these tools to the model; empty means all of them.
	// Large plugins need this: the official GitHub MCP server has ninety-odd tools, and pushing the lot
	// into the tool list both eats context and measurably lowers the odds of the model picking the right
	// one. The selection lives in config rather than being renegotiated on each connect because it is
	// the user's intent, which a change in the server's tool list should not quietly override.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// auth: "oauth" 表示这个远程连接器的令牌由 OAuth 流程管理（动态注册 + 授权码 + PKCE），
	// 留空则沿用 bearer_key 那条静态令牌的路。
	// auth: "oauth" means this remote connector's token is managed by the OAuth flow (dynamic
	// registration, authorization code, PKCE); empty keeps the static bearer_key path.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty"`
}

func (c MCPServerConfig) usesOAuth() bool { return c.Auth == "oauth" }

func (c MCPServerConfig) transport() string {
	if c.URL != "" {
		return "http"
	}
	return "stdio"
}

type mcpFile struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

// ---- JSON-RPC ----

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type mcpConn interface {
	call(method string, params any, timeout time.Duration) (json.RawMessage, error)
	notify(method string, params any) error
	close()
}

// readMCPFrame 读一帧：官方 Content-Length，或兼容按行 JSON。
// readMCPFrame reads one frame: official Content-Length, or a line of JSON for compatibility.
func readMCPFrame(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] == ' ' || b[0] == '\r' || b[0] == '\n' || b[0] == '\t' {
			if _, err := r.ReadByte(); err != nil {
				return nil, err
			}
			continue
		}
		if b[0] == '{' {
			line, err := r.ReadBytes('\n')
			return bytes.TrimSpace(line), err
		}
		break
	}
	contentLen := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "content-length:") {
			n, convErr := fmt.Sscanf(strings.TrimSpace(line[len("content-length:"):]), "%d", &contentLen)
			if convErr != nil || n != 1 {
				return nil, fmt.Errorf("bad Content-Length: %q", line)
			}
		}
	}
	if contentLen <= 0 || contentLen > 16<<20 {
		return nil, fmt.Errorf("invalid Content-Length %d", contentLen)
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeMCPFrame 按 MCP stdio 规范写一帧：一行 JSON 加一个 \n。
// 不能用 LSP 那套 Content-Length 头——官方 SDK 的 stdio server 是逐行读、逐行 JSON.parse 的，
// 收到头部行会当成坏消息丢掉，于是握手一声不吭地卡到超时。读方向仍然两种都收（见 readMCPFrame），
// 因为个别服务器确实按 LSP 实现；宽进严出。
//
// writeMCPFrame writes one frame per the MCP stdio spec: a single line of JSON plus \n.
// LSP-style Content-Length headers cannot be used here — the official SDK's stdio server reads line by
// line and JSON.parses each one, so it discards a header line as a bad message and the handshake stalls
// silently until it times out. Reading still accepts both forms (see readMCPFrame), since a few servers
// really do speak LSP framing: liberal in what we accept, strict in what we send.
func writeMCPFrame(w io.Writer, raw []byte) error {
	// JSON 编码不会产生裸换行，所以「一行一帧」是安全的。
	// JSON encoding never emits a bare newline, so one-line-per-frame is safe.
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n"))
	return err
}

// stderrTail 是个只留末尾一段的 io.Writer，用来收插件进程的 stderr。
// stderrTail is an io.Writer that keeps only the final stretch of what is written to it, used to collect
// a plugin process's stderr.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > stderrTailLimit {
		s.buf = s.buf[len(s.buf)-stderrTailLimit:]
	}
	return len(p), nil
}

func (s *stderrTail) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}

// diagnosable 让连接在出错时补充一点自己才知道的上下文（目前只有 stdio 有：子进程的 stderr）。
// diagnosable lets a connection add context only it has when something fails (today only stdio has any:
// the child process's stderr).
type diagnosable interface{ diagnostics() string }

// ---- stdio 传输：本地插件进程，LSP 风格 Content-Length 分帧 ----
// ---- stdio transport: local plugin process, LSP-style Content-Length framing ----

// connEvents 是连接报给上层的两件事，两件都只有连接这一侧知道：
// 进程/连接没了，以及服务端说工具列表变了。
// 没有这层回调的话，上层只能靠"下一次调用失败"才发现插件已经死了——而那时候模型已经
// 拿着一份不存在的工具在调了。
//
// connEvents carries the two things only the connection side can know: that the process or connection
// is gone, and that the server says its tool list changed.
// Without this callback layer the layer above only finds out a plugin died when the next call fails —
// by which point the model is already calling tools that are no longer there.
type connEvents struct {
	onClosed       func()
	onToolsChanged func()
}

type stdioConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *stderrTail
	ev      connEvents
	writeMu sync.Mutex
	pending map[int64]chan rpcResponse
	pmu     sync.Mutex
	nextID  int64
	done    chan struct{}
	// 主动关闭时不该触发重连——用户刚删掉这个插件，或者正在重连它。
	// A deliberate close must not trigger a reconnect: the user just removed this plugin, or is
	// reconnecting it right now.
	closing  atomic.Bool
	closeOne sync.Once
}

func (c *stdioConn) diagnostics() string { return c.stderr.String() }

// 传给插件进程的环境变量白名单。
//
// 以前这里是 os.Environ() 全量继承，也就是说一个从 npm 拉下来的插件进程能拿到 SSH_AUTH_SOCK
// （等于能用你的 SSH 私钥去登任何机器、推任何仓库，还不必读到密钥文件本身）、AWS/GitHub 令牌，
// 以及你在 shell 里 export 过的一切。插件要手填命令的年代这还能说得过去；面板上点一下就装之后
// 就不行了——装的东西不再是你逐条审过的。
//
// 名单里那些不是随便凑的：代理和证书变量少了，公司网络里所有插件都会连不出去，而症状是"超时"，
// 很难让人联想到是环境被摘了；Windows 那一组缺了进程根本起不来。
//
// The environment allowlist handed to a plugin process.
//
// This used to be a wholesale os.Environ(), meaning a plugin pulled off npm received SSH_AUTH_SOCK
// (enough to use your SSH keys against any host, without ever reading the key file), AWS and GitHub
// tokens, and everything else exported in your shell. That was defensible while plugins were typed in
// by hand; it stopped being defensible once one click from a panel installs them.
//
// The entries are not arbitrary: without the proxy and certificate variables every plugin behind a
// corporate network fails to reach anything, and the symptom is a timeout that nobody would trace back
// to a stripped environment; without the Windows group the process does not start at all.
var stdioEnvAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP", "TERM",
	"LANG", "LC_ALL", "LC_CTYPE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"SystemRoot", "SystemDrive", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	"ProgramData", "ProgramFiles", "PATHEXT", "COMSPEC", "WINDIR", "NUMBER_OF_PROCESSORS",
}

// pluginEnv 拼出插件进程的环境：白名单里存在的那些，加上插件自己声明的。
// 插件声明的排在后面，所以它可以覆盖白名单里的值——那是用户在 mcp.yaml 里写下的意图。
//
// pluginEnv assembles a plugin process's environment: whichever allowlisted variables exist, plus the
// ones the plugin declares. The declared ones come last and may therefore override an allowlisted
// value — that is the user's own intent, written down in mcp.yaml.
func pluginEnv(cfg MCPServerConfig, ks *secret.KeyStore) []string {
	env := make([]string, 0, len(stdioEnvAllow)+len(cfg.Env))
	for _, k := range stdioEnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range cfg.Env {
		if strings.HasPrefix(v, "$") && ks != nil {
			v = ks.Get(strings.TrimPrefix(v, "$"))
		}
		env = append(env, k+"="+v)
	}
	return env
}

func newStdioConn(cfg MCPServerConfig, ks *secret.KeyStore, ev connEvents) (*stdioConn, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = pluginEnv(cfg, ks)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &stderrTail{}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to start the plugin process: %w"), err)
	}
	c := &stdioConn{
		cmd: cmd, stdin: stdin, stderr: tail, ev: ev,
		pending: map[int64]chan rpcResponse{},
		done:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *stdioConn) readLoop(stdout io.Reader) {
	defer func() {
		close(c.done)
		// 进程自己没的（崩了、被系统回收、机器睡醒后管道断了）才上报；主动关的不报。
		// Report only when the process went away on its own (a crash, the OS reaping it, a pipe broken by
		// the machine waking up); a deliberate close stays quiet.
		if !c.closing.Load() && c.ev.onClosed != nil {
			c.ev.onClosed()
		}
	}()
	r := bufio.NewReaderSize(stdout, 64*1024)
	for {
		line, err := readMCPFrame(r)
		if err != nil {
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var resp rpcResponse
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		switch {
		case resp.Method == "" && resp.ID != nil: // 响应 / a response
			c.pmu.Lock()
			ch := c.pending[*resp.ID]
			delete(c.pending, *resp.ID)
			c.pmu.Unlock()
			if ch != nil {
				ch <- resp
			}
		// 服务器→客户端请求：礼貌拒绝
		// server→client request: decline politely
		case resp.Method != "" && resp.ID != nil:
			c.writeMsg(map[string]any{
				"jsonrpc": "2.0", "id": *resp.ID,
				"error": map[string]any{"code": -32601, "message": "method not supported"},
			})
		// 通知：只有工具列表变更需要处理。
		// 必须另起 goroutine——这里就是 readLoop 本身，回调里若再发一次请求，
		// 响应要靠这个循环读回来，同步调用就是自己等自己，直接死锁。
		//
		// Notifications: only a tool-list change needs handling.
		// It has to run in its own goroutine — this is readLoop itself, and a callback issuing another
		// request would need this very loop to read the reply, so a synchronous call would deadlock
		// waiting on itself.
		case resp.Method == "notifications/tools/list_changed":
			if c.ev.onToolsChanged != nil {
				go c.ev.onToolsChanged()
			}
		default: // 其它通知：忽略 / other notifications: ignore
		}
	}
}

func (c *stdioConn) writeMsg(msg any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeMCPFrame(c.stdin, raw)
}

func (c *stdioConn) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan rpcResponse, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()
	if err := c.writeMsg(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf(i18n.T("Plugin returned an error: %s"), resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf(i18n.T("Plugin call timed out (%s %s)"), method, timeout)
	case <-c.done:
		return nil, errors.New(i18n.T("The plugin process has exited"))
	}
}

func (c *stdioConn) notify(method string, params any) error {
	return c.writeMsg(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// close 幂等：重连路径和删除路径都可能关同一个连接，而 cmd.Wait() 被并发调用两次是数据竞争。
// close is idempotent: both the reconnect path and the removal path may close the same connection, and
// calling cmd.Wait() twice concurrently is a data race.
func (c *stdioConn) close() {
	c.closeOne.Do(func() {
		c.closing.Store(true)
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
}

// ---- Streamable HTTP 传输：远程 MCP 连接器 ----
// ---- Streamable HTTP transport: remote MCP connectors ----

type httpConn struct {
	url string
	// 静态令牌（bearer_key 那条路）
	// The static token (the bearer_key path)
	bearer string
	// OAuth 那条路：每次请求现取，因为令牌会过期、会被刷新——启动时取一次存着的话，
	// 令牌一过期这个连接器就默默地全线 401 了。
	// The OAuth path: fetched per request, because the token expires and gets refreshed. Grabbing it once
	// at startup would leave the connector quietly 401-ing on everything the moment it expired.
	token     func() (string, error)
	sessionID string
	// 握手协商出来的版本，之后每个请求都要带回去（2025-06-18 起是硬要求，
	// 不带的服务器可以直接拒）。空表示还没握手完。
	// The version settled at handshake, echoed on every later request (a hard requirement from
	// 2025-06-18 on, and a server may refuse a request without it). Empty until the handshake finishes.
	protocolVersion string
	nextID          int64
	httpc           *http.Client
	mu              sync.Mutex
}

// setProtocolVersion 由握手在协商完成后调用。
// setProtocolVersion is called by the handshake once negotiation is done.
func (c *httpConn) setProtocolVersion(v string) {
	c.mu.Lock()
	c.protocolVersion = v
	c.mu.Unlock()
}

// versionAware 只有 HTTP 传输需要：stdio 是一条独占的管道，没有"后续请求要自报版本"这回事。
// versionAware is only needed by the HTTP transport: stdio is a dedicated pipe with no notion of later
// requests having to restate the version.
type versionAware interface{ setProtocolVersion(string) }

func newHTTPConn(cfg MCPServerConfig, ks *secret.KeyStore, oauth TokenSource) *httpConn {
	c := &httpConn{url: cfg.URL, httpc: &http.Client{Timeout: mcpCallTimeout + 10*time.Second}}
	if cfg.usesOAuth() && oauth != nil {
		name := cfg.Name
		c.token = func() (string, error) { return oauth.Bearer(name) }
		return c
	}
	if cfg.BearerKey != "" && ks != nil {
		c.bearer = ks.Get(cfg.BearerKey)
	}
	return c
}

// TokenSource 是 OAuth 令牌的来源。做成接口是为了让 plugin 包不必依赖 secret 里的具体实现，
// 测试也能塞一个假的进来。
// TokenSource supplies OAuth tokens. It is an interface so the plugin package need not depend on the
// concrete implementation in secret, and so a fake can be substituted in tests.
type TokenSource interface {
	Bearer(name string) (string, error)
}

func (c *httpConn) post(msg any, timeout time.Duration) (*http.Response, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.mu.Lock()
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocolVersion)
	}
	c.mu.Unlock()
	switch {
	case c.token != nil:
		tok, err := c.token()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case c.bearer != "":
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	return c.httpc.Do(req)
}

func (c *httpConn) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.post(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}, timeout)
	if err != nil {
		return nil, fmt.Errorf(i18n.T("Cannot reach the connector: %w"), err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf(i18n.T("Connector returned HTTP %d: %.200s"), resp.StatusCode, string(body))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return readSSEResponse(resp.Body, id, timeout)
	}
	var parsed rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf(i18n.T("Invalid connector response: %w"), err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf(i18n.T("Connector returned an error: %s"), parsed.Error.Message)
	}
	return parsed.Result, nil
}

// readSSEResponse 从 SSE 流里找到 id 匹配的 JSON-RPC 响应。
// readSSEResponse finds the JSON-RPC response with the matching id in an SSE stream.
func readSSEResponse(body io.Reader, wantID int64, timeout time.Duration) (json.RawMessage, error) {
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var parsed rpcResponse
			if json.Unmarshal([]byte(data), &parsed) != nil {
				continue
			}
			if parsed.ID == nil || *parsed.ID != wantID || parsed.Method != "" {
				continue
			}
			if parsed.Error != nil {
				ch <- result{nil, fmt.Errorf(i18n.T("Connector returned an error: %s"), parsed.Error.Message)}
			} else {
				ch <- result{parsed.Result, nil}
			}
			return
		}
		ch <- result{nil, errors.New(i18n.T("The SSE stream ended without a response"))}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf(i18n.T("Timed out waiting for the connector response (%s)"), timeout)
	}
}

func (c *httpConn) notify(method string, params any) error {
	resp, err := c.post(rpcRequest{JSONRPC: "2.0", Method: method, Params: params}, mcpInitTimeout)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// close 主动终止远程会话。以前这里什么都不做，于是每次重连、每次改配置都在服务端留下一个
// 挂着的会话，直到它自己超时——对有会话数配额的服务商，攒够了就开始拒绝新连接。
// 失败无所谓：会话本来也会超时，这只是把它提前还回去。
//
// close terminates the remote session. This used to do nothing, so every reconnect and every config
// change left a session dangling on the server until it timed out — and against a provider with a
// session quota, enough of them start refusing new connections.
// Failure does not matter: the session would expire anyway; this just hands it back early.
func (c *httpConn) close() {
	c.mu.Lock()
	session, version := c.sessionID, c.protocolVersion
	c.mu.Unlock()
	if session == "" {
		return
	}
	req, err := http.NewRequest("DELETE", c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if c.token != nil {
		if tok, err := c.token(); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.httpc.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ---- 客户端（单个插件）----
// ---- client (a single plugin) ----

type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Properties  map[string]any `json:"-"`
	Required    []string       `json:"-"`
	// Extras 是 inputSchema 根层 properties/required/type 之外的其余关键字（$defs、oneOf、
	// additionalProperties…）。以前这些被直接丢掉，结果是：属性里写 $ref: "#/$defs/X" 的插件，
	// 到了模型那儿引用悬空，参数怎么填都不对。schema 得原样过去。
	//
	// Extras holds the inputSchema's root-level keywords other than properties/required/type ($defs,
	// oneOf, additionalProperties, ...). These used to be dropped outright, which left any plugin whose
	// properties say $ref: "#/$defs/X" with a dangling reference by the time the model saw it, and no
	// way to fill the arguments correctly. The schema has to arrive intact.
	Extras   map[string]any `json:"-"`
	ReadOnly bool           `json:"read_only"`
}

type mcpClient struct {
	cfg MCPServerConfig
	// 所属管理器。连接侧出事时要通知界面、要重连，都得从这儿走。
	// The owning manager: notifying the UI and reconnecting after trouble on the connection side both go
	// through it.
	mgr    *MCPManager
	mu     sync.Mutex
	conn   mcpConn
	tools  []MCPTool
	status string // connecting / connected / error
	errMsg string
	// 已经有一轮重连在跑；防止一次掉线拉起好几个重连循环。
	// A reconnect round is already running; keeps one disconnection from starting several loops.
	retrying atomic.Bool
}

// onConnClosed 在插件连接自己断掉时被调用（进程崩了、机器睡醒后管道断了、服务端踢了会话）。
//
// 以前没有这条路径：状态会一直停在 connected，工具列表照旧摆在模型面前，模型接着调，
// 然后每一次都失败——而界面上那颗点还是绿的，用户只能自己想到去点"重连"。
// 而 Bot Bureau 的前提就是一台常开的机器上住着一队 bot，这种事不是意外，是迟早的事。
//
// onConnClosed fires when a plugin connection drops on its own (the process crashed, a pipe broke when
// the machine woke, the server dropped the session).
//
// There was no such path before: the status stayed at connected, the tool list stayed in front of the
// model, the model kept calling, and every call failed — while the dot in the UI stayed green and the
// user's only recourse was to think of hitting "reconnect". Given that Bot Bureau's premise is a team of
// bots living on an always-on machine, this is not an accident but a matter of time.
func (c *mcpClient) onConnClosed() {
	c.mu.Lock()
	c.conn = nil
	c.status = "error"
	c.errMsg = i18n.T("The plugin stopped; reconnecting…")
	c.mu.Unlock()
	c.mgr.fireChange()
	go c.reconnectLoop()
}

// reconnectLoop 退避重试，几次不成就停下等人工。
// 必须封顶：一个一起来就崩的插件会把这里变成一个刷满 CPU 和日志的死循环。
//
// reconnectLoop retries with backoff and stops for a human after a few attempts.
// The cap is essential: a plugin that crashes on start would otherwise turn this into a spin that burns
// CPU and floods the log.
func (c *mcpClient) reconnectLoop() {
	if !c.retrying.CompareAndSwap(false, true) {
		return
	}
	defer c.retrying.Store(false)

	delay := reconnectBackoffMin
	for attempt := 1; attempt <= reconnectMaxTries; attempt++ {
		time.Sleep(delay)
		if delay *= 2; delay > reconnectBackoffMax {
			delay = reconnectBackoffMax
		}
		// 期间被删掉或被手动重连过就收手
		// Give up if it was removed, or reconnected by hand, in the meantime
		if !c.mgr.Has(c.cfg.Name) {
			return
		}
		c.mu.Lock()
		live := c.conn != nil
		c.mu.Unlock()
		if live {
			return
		}
		if err := c.connect(c.mgr.ks, c.mgr.tokenSource()); err == nil {
			c.mgr.fireChange()
			return
		}
	}
	c.mu.Lock()
	c.errMsg = i18n.T("The plugin stopped and could not be reconnected automatically; reconnect it from the panel")
	c.mu.Unlock()
	c.mgr.fireChange()
}

// onToolsChanged 处理服务端的 tools/list_changed：工具集是动态的服务器（典型的是
// "登录前 3 个工具、登录后 30 个"）以前会永远停在第一次握手时那份名单上。
//
// onToolsChanged handles the server's tools/list_changed. A server with a dynamic tool set — the classic
// case being three tools before sign-in and thirty after — used to stay frozen at whatever the first
// handshake saw.
func (c *mcpClient) onToolsChanged() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	if err := c.refreshTools(conn, mcpInitTimeout); err != nil {
		return
	}
	c.mgr.fireChange()
}

func (c *mcpClient) connect(ks *secret.KeyStore, oauth TokenSource) error {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.close()
		c.conn = nil
	}
	c.status = "connecting"
	c.errMsg = ""
	c.mu.Unlock()

	// 只在成功时才把连接装进接口变量：newStdioConn 失败返回的是 (*stdioConn)(nil)，
	// 直接赋给 mcpConn 会得到一个「非 nil 接口装着 nil 指针」，下面的 conn != nil 拦不住，
	// close() 一调就空指针崩溃。而命令不存在（没装 uvx、命令名打错）正是最常见的失败。
	//
	// Only store the connection in the interface variable on success: on failure newStdioConn returns
	// (*stdioConn)(nil), and assigning that to an mcpConn yields a non-nil interface holding a nil
	// pointer — which the conn != nil check below does not stop, so close() panics. A missing command
	// (uvx not installed, a typo'd command name) is the most common failure of all.
	var conn mcpConn
	var err error
	if c.cfg.transport() == "stdio" {
		// 通知只在 stdio 上收得到：远程连接器的服务端推送要靠一条常开的 GET SSE 流，
		// 而我们只发请求、不开那条流。
		// Notifications only arrive over stdio: server pushes on a remote connector need a long-lived GET
		// SSE stream, and we only ever issue requests rather than holding one open.
		sc, startErr := newStdioConn(c.cfg, ks, connEvents{
			onClosed:       c.onConnClosed,
			onToolsChanged: c.onToolsChanged,
		})
		if startErr != nil {
			err = startErr
		} else {
			conn = sc
		}
	} else {
		conn = newHTTPConn(c.cfg, ks, oauth)
	}
	if err == nil {
		err = c.handshake(conn)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// 插件自己在 stderr 上讲的话（"找不到该包"、"缺少 API key"…）比一句"初始化失败"有用得多
		// What the plugin said on stderr ("no such package", "missing API key", ...) is far more use than
		// a bare "initialization failed"
		if d, ok := conn.(diagnosable); ok {
			if tail := d.diagnostics(); tail != "" {
				err = fmt.Errorf("%w\n%s", err, textutil.Brief(tail, 600))
			}
		}
		if conn != nil {
			conn.close()
		}
		c.status = "error"
		c.errMsg = err.Error()
		return err
	}
	c.conn = conn
	c.status = "connected"
	return nil
}

// initTimeout：本地插件要留出现下包的时间，远程连接器不用。
// initTimeout: a local plugin needs room to download its package first; a remote connector does not.
func (c *mcpClient) initTimeout() time.Duration {
	if c.cfg.transport() == "stdio" {
		return mcpStdioInitTimeout
	}
	return mcpInitTimeout
}

func (c *mcpClient) handshake(conn mcpConn) error {
	timeout := c.initTimeout()
	initRaw, err := conn.call("initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "botbureau", "version": "0.1.0"},
	}, timeout)
	if err != nil {
		return fmt.Errorf(i18n.T("Initialization failed: %w"), err)
	}
	// 版本以服务端回的为准，而不是我们提的那个。以前这个返回值被直接丢掉，于是
	// "对方打算按哪个版本说话"我们根本没看——服务器一旦淘汰老版本就会毫无预警地整片连不上。
	// 回的版本我们不认也照用：协议是向后兼容的，认死一个版本只会把能用的服务器挡在门外。
	//
	// The version the server names wins, not the one we proposed. This result used to be discarded, so
	// which version the other side intended to speak was never even read — and the day a server drops the
	// old one, everything fails with no warning. An unfamiliar version is still used: the protocol is
	// backward compatible, and insisting on one version only shuts out servers that would have worked.
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(initRaw, &initRes) == nil && initRes.ProtocolVersion != "" {
		if va, ok := conn.(versionAware); ok {
			va.setProtocolVersion(initRes.ProtocolVersion)
		}
	}
	_ = conn.notify("notifications/initialized", map[string]any{})

	return c.refreshTools(conn, timeout)
}

// refreshTools 取一次工具列表并替换本地那份。握手时用它，服务端通知工具变了时也用它。
// refreshTools fetches the tool list and replaces the local copy. Used at handshake, and again whenever
// the server says its tools changed.
func (c *mcpClient) refreshTools(conn mcpConn, timeout time.Duration) error {
	raw, err := conn.call("tools/list", map[string]any{}, timeout)
	if err != nil {
		return fmt.Errorf(i18n.T("Failed to fetch the tool list: %w"), err)
	}
	var parsed struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf(i18n.T("Invalid tool list: %w"), err)
	}
	var tools []MCPTool
	for _, t := range parsed.Tools {
		props, required, extras := splitSchema(t.InputSchema)
		tools = append(tools, MCPTool{
			Name: t.Name, Description: t.Description,
			Properties: props, Required: required, Extras: extras,
			ReadOnly: t.Annotations.ReadOnlyHint,
		})
	}
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

// splitSchema 把 inputSchema 拆成 properties / required / 其余关键字三份。
// type 不进 extras——工具参数一定是 object，各家 provider 那边都自己写死了。
//
// splitSchema separates an inputSchema into properties, required and everything else.
// type is left out of the extras: tool arguments are always an object, and each provider hardcodes that
// on its own side.
func splitSchema(schema map[string]any) (props map[string]any, required []string, extras map[string]any) {
	props = map[string]any{}
	if schema == nil {
		return props, nil, nil
	}
	if p, ok := schema["properties"].(map[string]any); ok {
		props = p
	}
	if r, ok := schema["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	for k, v := range schema {
		switch k {
		case "properties", "required", "type":
			continue
		}
		if extras == nil {
			extras = map[string]any{}
		}
		extras[k] = v
	}
	return props, required, extras
}

// Image 是插件返回的一张图（base64）。浏览器插件的截图、图表插件的成品都走这里。
// Image is one image returned by a plugin, base64-encoded — screenshots from a browser plugin, finished
// charts from a charting one.
type Image struct {
	MIME   string
	Base64 string
}

// CallResult 是一次插件调用的产物。以前这里只有一个 string，于是所有非文本内容
// （最典型的就是截图）在协议边界上就被扔了。
// CallResult is what one plugin call produced. This used to be a bare string, so every non-text piece of
// content — a screenshot above all — was discarded right at the protocol boundary.
type CallResult struct {
	Text   string
	Images []Image
}

// Call 调用插件工具，返回（结果, 是否业务错误, 传输错误）。
// Call invokes a plugin tool and returns (result, business-error flag, transport error).
func (c *mcpClient) Call(tool string, args map[string]any) (CallResult, bool, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return CallResult{}, false, errors.New(i18n.T("Plugin is not connected"))
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := conn.call("tools/call", map[string]any{"name": tool, "arguments": args}, mcpCallTimeout)
	if err != nil {
		return CallResult{}, false, err
	}
	var parsed struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return CallResult{}, false, fmt.Errorf(i18n.T("Invalid plugin response: %w"), err)
	}
	var sb strings.Builder
	var images []Image
	for _, blk := range parsed.Content {
		switch {
		case blk.Type == "text":
			sb.WriteString(blk.Text)
			sb.WriteString("\n")
		case blk.Type == "image" && blk.Data != "":
			// 超大的图直接丢，但要说清楚丢了什么：base64 一张 4K 截图轻松几 MB，
			// 一股脑塞进上下文既贵又会把这一轮撑爆。
			// An oversized image is dropped, but says so: a 4K screenshot in base64 runs to several MB,
			// and pushing that into the context is both expensive and enough to blow up the turn.
			if len(blk.Data) > ImageDataLimit {
				fmt.Fprintf(&sb, i18n.T("[an image was returned but it is too large to include (%d KB)]\n"), len(blk.Data)/1024)
				continue
			}
			mime := blk.MimeType
			if mime == "" {
				mime = "image/png"
			}
			images = append(images, Image{MIME: mime, Base64: blk.Data})
		default:
			fmt.Fprintf(&sb, i18n.T("[%s content omitted]\n"), blk.Type)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" && len(images) == 0 {
		out = i18n.T("(no output)")
	}
	return CallResult{
		Text:   textutil.Truncate(out, ToolOutputLimit),
		Images: images,
	}, parsed.IsError, nil
}

// Tools 返回该插件当前生效的工具（已按勾选过滤）。
// 勾了一个已经不存在的工具名不算错：服务端升级后工具改名很常见，多余的名字忽略即可。
//
// Tools returns the plugin's tools currently in force, filtered by the selection.
// A selected name that no longer exists is not an error: tools get renamed across server upgrades often
// enough, and a stale name is simply ignored.
func (c *mcpClient) Tools() []MCPTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfg.Tools) == 0 {
		return append([]MCPTool(nil), c.tools...)
	}
	allow := make(map[string]bool, len(c.cfg.Tools))
	for _, n := range c.cfg.Tools {
		allow[n] = true
	}
	var out []MCPTool
	for _, t := range c.tools {
		if allow[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// AllTools 是服务端报上来的全部工具，不过滤——界面要用它渲染勾选框。
// AllTools is everything the server reported, unfiltered — the UI needs it to render the checkboxes.
func (c *mcpClient) AllTools() []MCPTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MCPTool(nil), c.tools...)
}

// ---- 管理器（全部插件）----
// ---- manager (all plugins) ----

type MCPManager struct {
	path     string
	ks       *secret.KeyStore
	oauth    TokenSource
	mu       sync.Mutex
	servers  map[string]*mcpClient
	order    []string
	onChange func()
}

// SetTokenSource 注入 OAuth 令牌来源。分两步而不是构造时传，是因为 OAuth 存储和插件管理器
// 都挂在 TeamDeps 上，构造顺序上谁也不该先于谁。
// SetTokenSource injects the OAuth token source. It is a second step rather than a constructor argument
// because the OAuth store and the plugin manager both hang off TeamDeps, and neither should have to be
// constructed first.
func (m *MCPManager) SetTokenSource(ts TokenSource) {
	m.mu.Lock()
	m.oauth = ts
	m.mu.Unlock()
}

// tokenSource 取令牌来源。连接是在 goroutine 里发起的，所以这个字段要和别的共享状态一样上锁读。
// tokenSource reads the token source. Connections are started from goroutines, so this field is read
// under the lock like every other piece of shared state.
func (m *MCPManager) tokenSource() TokenSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.oauth
}

func NewMCPManager(path string, ks *secret.KeyStore) *MCPManager {
	m := &MCPManager{path: path, ks: ks, servers: map[string]*mcpClient{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var f mcpFile
	if yaml.Unmarshal(raw, &f) != nil {
		fmt.Fprintf(os.Stderr, i18n.T("Failed to parse %s; ignoring its contents\n"), path)
		return m
	}
	for _, cfg := range f.Servers {
		if err := validateMCPConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, i18n.T("Skipping bad entry %q in mcp.yaml: %v\n"), cfg.Name, err)
			continue
		}
		if _, dup := m.servers[cfg.Name]; dup {
			continue
		}
		m.servers[cfg.Name] = &mcpClient{cfg: cfg, mgr: m, status: "connecting"}
		m.order = append(m.order, cfg.Name)
	}
	return m
}

func validateMCPConfig(cfg MCPServerConfig) error {
	if !pluginNameRe.MatchString(cfg.Name) {
		return errors.New(i18n.T("Name must be 1-24 characters of lowercase letters, digits, - or _"))
	}
	hasCmd, hasURL := cfg.Command != "", cfg.URL != ""
	if hasCmd == hasURL {
		return errors.New(i18n.T("Specify exactly one of command (local plugin) or url (remote connector)"))
	}
	return nil
}

func (m *MCPManager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *MCPManager) fireChange() {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ConnectAll 异步连接全部插件（启动时用，不阻塞）。连上后触发 onChange，下一回合就能用工具。
// ConnectAll connects all plugins asynchronously (used at startup; does not block). onChange fires when a plugin comes up so the next turn sees its tools.
func (m *MCPManager) ConnectAll() {
	m.mu.Lock()
	clients := make([]*mcpClient, 0, len(m.servers))
	for _, c := range m.servers {
		clients = append(clients, c)
	}
	m.mu.Unlock()
	for _, c := range clients {
		go func(c *mcpClient) {
			_ = c.connect(m.ks, m.tokenSource())
			m.fireChange()
		}(c)
	}
}

func (m *MCPManager) save() error {
	m.mu.Lock()
	var cfgs []MCPServerConfig
	for _, n := range m.order {
		if c, ok := m.servers[n]; ok {
			cfgs = append(cfgs, c.cfg)
		}
	}
	m.mu.Unlock()
	out, err := yaml.Marshal(mcpFile{Servers: cfgs})
	if err != nil {
		return err
	}
	header := []byte(i18n.T("# MCP plugin/connector definitions. Add or remove them in the client's plugins panel, or edit by hand and restart.\n"))
	return os.WriteFile(m.path, append(header, out...), 0o644)
}

// Add 校验、持久化并同步连接（把连接错误立刻反馈给 UI）。
// Add validates, persists, and connects synchronously (so connection errors surface in the UI immediately).
func (m *MCPManager) Add(cfg MCPServerConfig) error {
	if err := validateMCPConfig(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	if _, dup := m.servers[cfg.Name]; dup {
		m.mu.Unlock()
		return fmt.Errorf(i18n.T("A plugin named %s already exists"), cfg.Name)
	}
	c := &mcpClient{cfg: cfg, mgr: m, status: "connecting"}
	m.servers[cfg.Name] = c
	m.order = append(m.order, cfg.Name)
	m.mu.Unlock()
	if err := m.save(); err != nil {
		return err
	}
	if err := c.connect(m.ks, m.tokenSource()); err != nil {
		m.fireChange()
		return fmt.Errorf(i18n.T("Saved, but the connection failed: %v (you can reconnect from the panel)"), err)
	}
	m.fireChange()
	return nil
}

func (m *MCPManager) Remove(name string) bool {
	m.mu.Lock()
	c, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.servers, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	c.mu.Lock()
	if c.conn != nil {
		c.conn.close()
	}
	c.mu.Unlock()
	_ = m.save()
	m.fireChange()
	return true
}

func (m *MCPManager) Reconnect(name string) error {
	m.mu.Lock()
	c, ok := m.servers[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf(i18n.T("No plugin named %s"), name)
	}
	err := c.connect(m.ks, m.tokenSource())
	m.fireChange()
	return err
}

// Path 返回配置文件位置，供日志与诊断说明"配置在哪儿"。
// Path returns the config file location so logs and diagnostics can say where it lives.
func (m *MCPManager) Path() string { return m.path }

// URLOf 返回某个远程连接器的地址（本地插件为空）。
// URLOf returns a remote connector's address (empty for a local plugin).
func (m *MCPManager) URLOf(name string) string {
	m.mu.Lock()
	c := m.servers[name]
	m.mu.Unlock()
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.URL
}

func (m *MCPManager) Has(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.servers[name]
	return ok
}

func (m *MCPManager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

func (m *MCPManager) Tools(server string) []MCPTool {
	m.mu.Lock()
	c := m.servers[server]
	m.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Tools()
}

func (m *MCPManager) Call(server, tool string, args map[string]any) (CallResult, bool, error) {
	m.mu.Lock()
	c := m.servers[server]
	m.mu.Unlock()
	if c == nil {
		return CallResult{}, false, fmt.Errorf(i18n.T("No plugin named %s"), server)
	}
	return c.Call(tool, args)
}

// SetTools 改一个插件暴露给模型的工具子集（空表示全部），并落盘。
// SetTools changes which subset of a plugin's tools is exposed to the model (empty means all) and
// persists it.
func (m *MCPManager) SetTools(server string, tools []string) error {
	m.mu.Lock()
	c := m.servers[server]
	m.mu.Unlock()
	if c == nil {
		return fmt.Errorf(i18n.T("No plugin named %s"), server)
	}
	c.mu.Lock()
	c.cfg.Tools = tools
	c.mu.Unlock()
	if err := m.save(); err != nil {
		return err
	}
	m.fireChange()
	return nil
}

// Status 供 UI 展示。
// Status returns plugin state for display in the UI.
func (m *MCPManager) Status() []map[string]any {
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()
	out := []map[string]any{}
	for _, n := range names {
		m.mu.Lock()
		c := m.servers[n]
		m.mu.Unlock()
		if c == nil {
			continue
		}
		c.mu.Lock()
		entry := map[string]any{
			"name": c.cfg.Name, "transport": c.cfg.transport(),
			"status": c.status, "error": c.errMsg,
		}
		// all_tools 是服务端报上来的全部，tools 是当前生效的那些；界面要同时知道这两者
		// 才能画出"90 个里选了 6 个"这种状态。
		// all_tools is everything the server reported and tools is what is in force; the UI needs both to
		// render a state like "6 of 90 selected".
		var toolNames, allNames []string
		selected := map[string]bool{}
		for _, n := range c.cfg.Tools {
			selected[n] = true
		}
		for _, t := range c.tools {
			allNames = append(allNames, t.Name)
			if len(c.cfg.Tools) == 0 || selected[t.Name] {
				toolNames = append(toolNames, t.Name)
			}
		}
		c.mu.Unlock()
		entry["tools"] = toolNames
		entry["all_tools"] = allNames
		out = append(out, entry)
	}
	return out
}

// ---- 工具名映射 ----
// ---- tool name mapping ----

var mcpNameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// MCPToolName 生成暴露给模型的工具名：mcp_<插件>_<工具>，限长 64。
// MCPToolName builds the tool name exposed to the model: mcp_<plugin>_<tool>, capped at 64 chars.
func MCPToolName(server, tool string) string {
	name := "mcp_" + server + "_" + mcpNameSanitizeRe.ReplaceAllString(tool, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}
