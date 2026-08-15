package secret

// 用 ChatGPT Plus/Pro 订阅代替 API 额度。
//
// 同样是设备码 + PKCE：不起本地回调服务，授权码用 code_verifier 兑换，中途截获也换不出令牌。
// 订阅签发的令牌打的是 Codex 后端（chatgpt.com/backend-api/codex/responses），
// 跟 api.openai.com 的 API key 不是一回事——端点、鉴权、可用模型都不同。
// 令牌 0600 落盘；account_id 从 id_token 里解出来，请求时要带上。
//
// Use a ChatGPT Plus/Pro subscription instead of API credit.
//
// Device code plus PKCE: no local callback server, and the authorization code is redeemed with a
// code_verifier, so intercepting it alone yields no token. A subscription token targets the Codex
// backend (chatgpt.com/backend-api/codex/responses), which is a different thing from an
// api.openai.com API key — different endpoint, different auth, different available models.
// Tokens are stored 0600; the account_id is decoded from the id_token and sent with each request.
import (
	"botbureau/backend/internal/textutil"

	"botbureau/backend/internal/i18n"

	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	chatgptClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptIssuer   = "https://auth.openai.com"
	ChatGPTCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	// 订阅令牌只认 Codex 后端，列模型也得走它，api.openai.com 的 /models 开不了
	// A subscription token only opens the Codex backend, listing models included; /models on api.openai.com stays shut
	ChatGPTModelsURL = "https://chatgpt.com/backend-api/codex/models"
	// /models 必须带 client_version，缺了直接 400；而且服务端按它裁剪返回的型号，
	// 报一个偏低的版本号会安安静静地返回空列表——所以这里报的是协议版本，不是本应用的版本号。
	//
	// /models requires a client_version and answers 400 without one. The server also gates the list by
	// that value: report a low version and it quietly returns an empty list — so this is the protocol
	// version we speak, not this app's own version number.
	ChatGPTClientVersion = "1.0.0"
	chatgptPollTimeout   = 5 * time.Minute
	chatgptMinInterval   = time.Second
	chatgptSafetyWait    = 3 * time.Second
)

type chatgptStored struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"account_id"`
	UpdatedAt int64  `json:"updated_at"`
}

type chatgptPending struct {
	authID   string
	userCode string
	url      string
	status   string
	err      string
	cancel   chan struct{}
}

// ChatGPTOAuth 持有已保存的令牌和一次进行中的登录。
// ChatGPTOAuth holds the stored tokens and at most one in-flight login.
type ChatGPTOAuth struct {
	path   string
	http   *http.Client
	issuer string
	apiURL string

	mu      sync.Mutex
	stored  *chatgptStored
	pending *chatgptPending
}

func NewChatGPTOAuth(path string) *ChatGPTOAuth {
	c := &ChatGPTOAuth{
		path:   path,
		http:   &http.Client{Timeout: 30 * time.Second},
		issuer: chatgptIssuer,
		apiURL: ChatGPTCodexURL,
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var s chatgptStored
	if json.Unmarshal(raw, &s) == nil && s.Access != "" {
		c.stored = &s
	}
	return c
}

func (c *ChatGPTOAuth) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored != nil && c.stored.Access != ""
}

// AccountID 是请求 Codex 后端时要带的账号标识，从 id_token 里解出来。
// AccountID is the account identifier the Codex backend expects, decoded from the id_token.
// APIURL 是登录时服务端指定的接口地址；为空表示用默认的 Codex 端点。
// APIURL is the endpoint the server named at sign-in; empty means the default Codex endpoint.
// Restore 用一份现成的令牌把实例直接置为已登录，不走设备码流程。
// Restore marks the instance signed in from an existing token, bypassing the device-code flow.
func (c *ChatGPTOAuth) Restore(access, accountID string, expires time.Time) {
	c.mu.Lock()
	c.stored = &chatgptStored{Access: access, AccountID: accountID, Expires: expires.Unix()}
	c.mu.Unlock()
}

// SetAPIURL 改写接口地址。登录时服务端会指定一个，指向代理或本地桩也走这条路。
// SetAPIURL overrides the endpoint. The server names one at sign-in; pointing it at a proxy or a
// local stub goes through here too.
func (c *ChatGPTOAuth) SetAPIURL(u string) { c.apiURL = u }

func (c *ChatGPTOAuth) APIURL() string {
	return c.apiURL
}

func (c *ChatGPTOAuth) AccountID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stored == nil {
		return ""
	}
	return c.stored.AccountID
}

func (c *ChatGPTOAuth) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]any{"connected": c.stored != nil && c.stored.Access != ""}
	if c.pending != nil {
		out["pending"] = true
		out["status"] = c.pending.status
		out["user_code"] = c.pending.userCode
		out["url"] = c.pending.url
		if c.pending.err != "" {
			out["error"] = c.pending.err
		}
	} else {
		out["pending"] = false
		out["status"] = map[bool]string{true: "ok", false: "idle"}[c.stored != nil && c.stored.Access != ""]
	}
	return out
}

// Start 发起一次设备码 + PKCE 登录，并在后台轮询。已有进行中的登录会被顶掉。
// Start begins a device-code + PKCE login and polls in the background, superseding any login in flight.
func (c *ChatGPTOAuth) Start() (map[string]any, error) {
	var device struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"`
	}
	if err := c.postJSON(c.issuer+"/api/accounts/deviceauth/usercode", map[string]any{
		"client_id": chatgptClientID,
	}, &device); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to request a ChatGPT login code: %w"), err)
	}
	if device.DeviceAuthID == "" || device.UserCode == "" {
		return nil, errors.New(i18n.T("ChatGPT returned an incomplete login code"))
	}
	interval := durationFromSeconds(atoiDefault(device.Interval, 5), 5*time.Second)
	c.mu.Lock()
	if c.pending != nil && c.pending.cancel != nil {
		close(c.pending.cancel)
	}
	p := &chatgptPending{
		authID: device.DeviceAuthID, userCode: device.UserCode,
		url: c.issuer + "/codex/device", status: "pending", cancel: make(chan struct{}),
	}
	c.pending = p
	c.mu.Unlock()
	go c.poll(p, interval)
	return c.Status(), nil
}

func (c *ChatGPTOAuth) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil && c.pending.cancel != nil {
		select {
		case <-c.pending.cancel:
		default:
			close(c.pending.cancel)
		}
	}
	c.pending = nil
}

func (c *ChatGPTOAuth) Logout() {
	c.Cancel()
	c.mu.Lock()
	c.stored = nil
	c.mu.Unlock()
	_ = os.Remove(c.path)
}

// Bearer 返回可用的 access token，临近过期就先刷新。
// Bearer returns a usable access token, refreshing it first when it is close to expiring.
func (c *ChatGPTOAuth) Bearer() (string, error) {
	c.mu.Lock()
	s := c.stored
	c.mu.Unlock()
	if s == nil || s.Access == "" {
		return "", errors.New(i18n.T("Not signed in to ChatGPT"))
	}
	if s.Expires == 0 || time.Until(time.Unix(s.Expires, 0)) > xaiRefreshSkew {
		return s.Access, nil
	}
	if s.Refresh == "" {
		return s.Access, nil
	}
	st, err := c.refresh(s)
	if err != nil {
		return "", err
	}
	return st.Access, nil
}

func (c *ChatGPTOAuth) refresh(prev *chatgptStored) (*chatgptStored, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {prev.Refresh},
		"client_id":     {chatgptClientID},
	}
	var tr chatgptToken
	if err := c.postForm(c.issuer+"/oauth/token", form, &tr); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to refresh ChatGPT login: %w"), err)
	}
	st := storeChatGPT(tr, prev)
	c.mu.Lock()
	c.stored = st
	c.mu.Unlock()
	if err := c.save(st); err != nil {
		return st, err
	}
	return st, nil
}

func (c *ChatGPTOAuth) poll(p *chatgptPending, interval time.Duration) {
	if interval < chatgptMinInterval {
		interval = chatgptMinInterval
	}
	deadline := time.Now().Add(chatgptPollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.cancel:
			return
		default:
		}
		var auth struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		err := c.postJSON(c.issuer+"/api/accounts/deviceauth/token", map[string]any{
			"device_auth_id": p.authID, "user_code": p.userCode,
		}, &auth)
		if err == nil && auth.AuthorizationCode != "" {
			tr, xerr := c.exchange(auth.AuthorizationCode, auth.CodeVerifier)
			if xerr != nil {
				c.fail(p, xerr.Error())
				return
			}
			st := storeChatGPT(tr, nil)
			c.mu.Lock()
			if c.pending == p {
				c.stored = st
				p.status = "ok"
				c.pending = nil
			}
			c.mu.Unlock()
			_ = c.save(st)
			return
		}
		if status, ok := httpStatusOf(err); ok && (status == 403 || status == 404) {
			select {
			case <-p.cancel:
				return
			case <-time.After(interval + chatgptSafetyWait):
			}
			continue
		}
		if err != nil {
			c.fail(p, err.Error())
			return
		}
		select {
		case <-p.cancel:
			return
		case <-time.After(interval + chatgptSafetyWait):
		}
	}
	c.fail(p, i18n.T("Login timed out — try again"))
}

// exchange 用授权码加 code_verifier 兑换令牌，这是 PKCE 的关键一步：
// 光有授权码换不出东西，必须同时证明自己就是发起请求的那一方。
//
// exchange redeems the authorization code together with the code_verifier — the crux of PKCE:
// the code alone is worthless without proof that the redeemer is the party that started the flow.
func (c *ChatGPTOAuth) exchange(code, verifier string) (chatgptToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.issuer + "/deviceauth/callback"},
		"client_id":     {chatgptClientID},
		"code_verifier": {verifier},
	}
	var tr chatgptToken
	if err := c.postForm(c.issuer+"/oauth/token", form, &tr); err != nil {
		return tr, fmt.Errorf(i18n.T("Failed to exchange ChatGPT login: %w"), err)
	}
	if tr.AccessToken == "" {
		return tr, errors.New(i18n.T("ChatGPT did not return an access token"))
	}
	return tr, nil
}

func (c *ChatGPTOAuth) fail(p *chatgptPending, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != p {
		return
	}
	p.status = "error"
	p.err = msg
}

// save 把令牌写到 0600 的文件里——这等同于账号凭据，不能让同机其他用户读到。
// save writes the tokens to a 0600 file: these are account credentials and must not be readable
// by other users on the machine.
// 编码在锁内、写盘在锁外，理由同 XaiOAuth.save：s 被 c.stored 同时持有，编码要读它全部字段。
// The encoding is under the lock and the write is not, for the same reason as XaiOAuth.save: s is held by
// c.stored at the same time, and encoding reads all of its fields.
func (c *ChatGPTOAuth) save(s *chatgptStored) error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	raw, err := marshalSecret(s)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return writeSecretFile(c.path, raw)
}

type chatgptToken struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func storeChatGPT(tr chatgptToken, prev *chatgptStored) *chatgptStored {
	if tr.RefreshToken == "" && prev != nil {
		tr.RefreshToken = prev.Refresh
	}
	exp := time.Now().Add(time.Hour).Unix()
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	account := chatgptAccountFromJWT(tr.IDToken)
	if account == "" {
		account = chatgptAccountFromJWT(tr.AccessToken)
	}
	if account == "" && prev != nil {
		account = prev.AccountID
	}
	return &chatgptStored{
		Access: tr.AccessToken, Refresh: tr.RefreshToken,
		Expires: exp, AccountID: account, UpdatedAt: time.Now().Unix(),
	}
}

// chatgptAccountFromJWT 只解 JWT 的载荷取 account_id，不验签
// ——令牌是刚从授权服务器 TLS 拿回来的，这里只是读个字段，不作为信任判断。
//
// chatgptAccountFromJWT decodes only the JWT payload to read account_id; it does not verify the
// signature — the token just came back from the authorization server over TLS, and this is a field
// read, not a trust decision.
func chatgptAccountFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		Auth             *struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.ChatGPTAccountID != "" {
		return claims.ChatGPTAccountID
	}
	if claims.Auth != nil && claims.Auth.ChatGPTAccountID != "" {
		return claims.Auth.ChatGPTAccountID
	}
	if len(claims.Organizations) > 0 {
		return claims.Organizations[0].ID
	}
	return ""
}

type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

func httpStatusOf(err error) (int, bool) {
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.status, true
	}
	return 0, false
}

func (c *ChatGPTOAuth) postJSON(endpoint string, body map[string]any, into any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "botbureau/0.1.0")
	return c.do(req, into)
}

func (c *ChatGPTOAuth) postForm(endpoint string, form url.Values, into any) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "botbureau/0.1.0")
	return c.do(req, into)
}

func (c *ChatGPTOAuth) do(req *http.Request, into any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		msg := strings.TrimSpace(e.Error + " " + e.ErrorDescription)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, textutil.Brief(string(body), 200))
		}
		return &httpStatusError{status: resp.StatusCode, msg: msg}
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

func atoiDefault(s string, fallback int) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 && strings.TrimSpace(s) != "0" {
		return fallback
	}
	return n
}

// IsOfficialOpenAIBase 用于兼容没有显式 auth 字段的老配置：靠 base_url 猜是不是该用 ChatGPT 订阅。
// IsOfficialOpenAIBase supports pre-existing configs with no explicit auth field, guessing from the base URL.
func IsOfficialOpenAIBase(base string) bool {
	b := strings.ToLower(base)
	return b == "" || strings.Contains(b, "api.openai.com") || strings.Contains(b, "chatgpt.com")
}
