package secret

// 用 SuperGrok 订阅代替 API key。
//
// 走 OAuth 设备码流程：引擎跟 auth.x.ai 换一个 user_code，用户在浏览器里输入它完成授权，
// 引擎这边轮询到令牌为止。选设备码而不是回调，是因为它不需要在本机起一个 HTTP 回调服务，
// 也就不用把一个临时端口暴露出去。令牌以 0600 落盘，过期前自动刷新。
//
// Use a SuperGrok subscription instead of an API key.
//
// This is the OAuth device-code flow: the engine trades with auth.x.ai for a user_code, the user
// types it into a browser to authorize, and the engine polls until a token arrives. Device code is
// chosen over a redirect because it needs no local HTTP callback server, so no temporary port is
// ever exposed. Tokens are stored 0600 and refreshed before they expire.
import (
	"botbureau/backend/internal/textutil"

	"botbureau/backend/internal/i18n"

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
	xaiClientID          = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiTokenURL          = "https://auth.x.ai/oauth2/token"
	xaiDeviceURL         = "https://auth.x.ai/oauth2/device/code"
	xaiDeviceGrant       = "urn:ietf:params:oauth:grant-type:device_code"
	xaiOAuthScope        = "openid profile email offline_access grok-cli:access api:access"
	xaiDefaultInterval   = 5 * time.Second
	xaiMinInterval       = time.Second
	xaiSlowDownIncrement = 5 * time.Second
	xaiDefaultExpires    = 5 * time.Minute
	xaiRefreshSkew       = 2 * time.Minute
)

type xaiDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type xaiTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type xaiStored struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	UpdatedAt int64  `json:"updated_at"`
}

// XaiOAuth 持有已保存的令牌和一次进行中的登录。
// XaiOAuth holds the stored tokens and at most one in-flight login.
type XaiOAuth struct {
	path string
	http *http.Client

	deviceURL string
	tokenURL  string

	mu      sync.Mutex
	stored  *xaiStored
	pending *xaiPending
}

type xaiPending struct {
	device xaiDeviceCode
	status string
	err    string
	cancel chan struct{}
}

func NewXaiOAuth(path string) *XaiOAuth {
	x := &XaiOAuth{
		path:      path,
		http:      &http.Client{Timeout: 30 * time.Second},
		deviceURL: xaiDeviceURL,
		tokenURL:  xaiTokenURL,
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return x
	}
	var s xaiStored
	if json.Unmarshal(raw, &s) == nil && s.Access != "" {
		x.stored = &s
	}
	return x
}

// Restore 用一份现成的令牌把实例直接置为已登录，不走设备码流程。
// Restore marks the instance signed in from an existing token, bypassing the device-code flow.
func (x *XaiOAuth) Restore(access string, expires time.Time) {
	x.mu.Lock()
	x.stored = &xaiStored{Access: access, Expires: expires.Unix()}
	x.mu.Unlock()
}

func (x *XaiOAuth) Connected() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.stored != nil && x.stored.Access != ""
}

// Status 给界面用：是否已连接、是否有登录在进行、配对码和用户要去的网址。
// Status feeds the UI: whether it is connected, whether a login is in flight, the pairing code and the URL.
func (x *XaiOAuth) Status() map[string]any {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[string]any{"connected": x.stored != nil && x.stored.Access != ""}
	if x.pending != nil {
		out["pending"] = true
		out["status"] = x.pending.status
		out["user_code"] = x.pending.device.UserCode
		out["url"] = x.pendingURLLocked()
		if x.pending.err != "" {
			out["error"] = x.pending.err
		}
	} else {
		out["pending"] = false
		out["status"] = map[bool]string{true: "ok", false: "idle"}[x.stored != nil && x.stored.Access != ""]
	}
	return out
}

func (x *XaiOAuth) pendingURLLocked() string {
	if x.pending == nil {
		return ""
	}
	if x.pending.device.VerificationURIComplete != "" {
		return x.pending.device.VerificationURIComplete
	}
	return x.pending.device.VerificationURI
}

// Start 发起一次设备码登录，并在后台轮询。已有进行中的登录会被顶掉。
// Start begins a device-code login and polls in the background, superseding any login already in flight.
func (x *XaiOAuth) Start() (map[string]any, error) {
	form := url.Values{
		"client_id": {xaiClientID},
		"scope":     {xaiOAuthScope},
		"referrer":  {"botbureau"},
	}
	var device xaiDeviceCode
	if err := x.postForm(x.deviceURL, form, &device); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to request an xAI login code: %w"), err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return nil, errors.New(i18n.T("xAI returned an incomplete login code"))
	}
	x.mu.Lock()
	if x.pending != nil && x.pending.cancel != nil {
		close(x.pending.cancel)
	}
	p := &xaiPending{device: device, status: "pending", cancel: make(chan struct{})}
	x.pending = p
	x.mu.Unlock()
	go x.poll(p)
	return x.Status(), nil
}

func (x *XaiOAuth) Cancel() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending != nil && x.pending.cancel != nil {
		select {
		case <-x.pending.cancel:
		default:
			close(x.pending.cancel)
		}
	}
	x.pending = nil
}

func (x *XaiOAuth) Logout() {
	x.Cancel()
	x.mu.Lock()
	x.stored = nil
	x.mu.Unlock()
	_ = os.Remove(x.path)
}

// Bearer 返回可用的 access token，临近过期就先刷新。
// Bearer returns a usable access token, refreshing it first when it is close to expiring.
func (x *XaiOAuth) Bearer() (string, error) {
	x.mu.Lock()
	s := x.stored
	x.mu.Unlock()
	if s == nil || s.Access == "" {
		return "", errors.New(i18n.T("Not signed in to SuperGrok"))
	}
	if s.Expires == 0 || time.Until(time.Unix(s.Expires, 0)) > xaiRefreshSkew {
		return s.Access, nil
	}
	if s.Refresh == "" {
		return s.Access, nil
	}
	tok, err := x.refresh(s.Refresh)
	if err != nil {
		return "", err
	}
	return tok.Access, nil
}

func (x *XaiOAuth) refresh(refresh string) (*xaiStored, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {xaiClientID},
	}
	var tr xaiTokenResponse
	if err := x.postForm(x.tokenURL, form, &tr); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to refresh SuperGrok login: %w"), err)
	}
	if tr.RefreshToken == "" {
		tr.RefreshToken = refresh
	}
	st := storeFromToken(tr)
	x.mu.Lock()
	x.stored = st
	x.mu.Unlock()
	if err := x.save(st); err != nil {
		return st, err
	}
	return st, nil
}

// poll 按服务端给的间隔取令牌。authorization_pending 是正常等待；
// 收到 slow_down 必须主动放慢，否则会被限流甚至判定滥用。
//
// poll asks for the token at the interval the server dictates. authorization_pending is the normal
// "still waiting" case; a slow_down must actually back off or the client gets rate-limited or flagged.
func (x *XaiOAuth) poll(p *xaiPending) {
	interval := durationFromSeconds(p.device.Interval, xaiDefaultInterval)
	if interval < xaiMinInterval {
		interval = xaiMinInterval
	}
	deadline := time.Now().Add(durationFromSeconds(p.device.ExpiresIn, xaiDefaultExpires))
	for time.Now().Before(deadline) {
		select {
		case <-p.cancel:
			return
		default:
		}
		form := url.Values{
			"grant_type":  {xaiDeviceGrant},
			"client_id":   {xaiClientID},
			"device_code": {p.device.DeviceCode},
		}
		var tr xaiTokenResponse
		err := x.postForm(x.tokenURL, form, &tr)
		if err == nil && tr.AccessToken != "" {
			st := storeFromToken(tr)
			x.mu.Lock()
			if x.pending == p {
				x.stored = st
				p.status = "ok"
				x.pending = nil
			}
			x.mu.Unlock()
			_ = x.save(st)
			return
		}
		code := oauthErrCode(err)
		switch code {
		case "authorization_pending":
		case "slow_down":
			interval += xaiSlowDownIncrement
		case "access_denied", "authorization_denied":
			x.fail(p, i18n.T("Login was denied"))
			return
		case "expired_token":
			x.fail(p, i18n.T("The login code expired — try again"))
			return
		default:
			if err != nil && code == "" {
				x.fail(p, err.Error())
				return
			}
			if code != "" {
				x.fail(p, err.Error())
				return
			}
		}
		select {
		case <-p.cancel:
			return
		case <-time.After(interval):
		}
	}
	x.fail(p, i18n.T("Login timed out — try again"))
}

func (x *XaiOAuth) fail(p *xaiPending, msg string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending != p {
		return
	}
	p.status = "error"
	p.err = msg
}

// save 把令牌写到 0600 的文件里——这等同于账号凭据，不能让同机其他用户读到。
//
// 编码放在锁内：s 同时挂在 x.stored 上，被别的 goroutine 拿着，而 JSON 编码要读遍它每个字段。
// 调用方是在解锁之后才调 save 的，所以锁在这里取，不在调用方那边——调用方持锁跨过一整段
// 文件 I/O 没有必要。
//
// save writes the tokens to a 0600 file: these are account credentials and must not be readable
// by other users on the machine.
//
// The encoding happens under the lock: s is also held by x.stored, other goroutines have a reference to
// it, and JSON encoding reads every one of its fields. Callers reach save after unlocking, so the lock is
// taken here rather than by them — there is no reason for a caller to hold it across a whole stretch of
// file I/O.
func (x *XaiOAuth) save(s *xaiStored) error {
	if x.path == "" {
		return nil
	}
	x.mu.Lock()
	raw, err := marshalSecret(s)
	x.mu.Unlock()
	if err != nil {
		return err
	}
	return writeSecretFile(x.path, raw)
}

func (x *XaiOAuth) postForm(endpoint string, form url.Values, into any) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "botbureau/0.1.0")
	resp, err := x.http.Do(req)
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
		if e.Error != "" {
			return &oauthError{code: e.Error, msg: strings.TrimSpace(e.Error + " " + e.ErrorDescription)}
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, textutil.Brief(string(body), 200))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

type oauthError struct {
	code string
	msg  string
}

func (e *oauthError) Error() string { return e.msg }

func oauthErrCode(err error) string {
	var oe *oauthError
	if errors.As(err, &oe) {
		return oe.code
	}
	return ""
}

func storeFromToken(tr xaiTokenResponse) *xaiStored {
	exp := time.Now().Add(time.Hour).Unix()
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	return &xaiStored{
		Access: tr.AccessToken, Refresh: tr.RefreshToken,
		Expires: exp, UpdatedAt: time.Now().Unix(),
	}
}

func durationFromSeconds(n int, fallback time.Duration) time.Duration {
	if n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

// IsXAIBase 用于兼容没有显式 auth 字段的老配置：靠 base_url 猜是不是该用 xAI 订阅。
// IsXAIBase supports pre-existing configs that carry no explicit auth field, guessing from the base URL.
func IsXAIBase(base string) bool {
	return strings.Contains(strings.ToLower(base), "api.x.ai")
}
