package model

// Provider catalog and model listing.

// Collapses "which vendor's model" into one table: the client renders it as a dropdown, and the user
// never has to know a base URL or remember an API key's env-var name. Model names are always fetched
// live — a hard-coded list goes stale, and a retired model id only blows up when a message is sent.

import (
	"botbureau/backend/internal/secret"

	"botbureau/backend/internal/i18n"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Auth modes.
const (
	AuthKey     = "key"     // use an API key
	AuthChatGPT = "chatgpt" // use a ChatGPT Plus/Pro subscription
	AuthXai     = "xai"     // use a SuperGrok subscription
	AuthNone    = "none"
)

type ProviderOption struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"` // underlying implementation
	BaseURL  string `json:"base_url"`
	KeyEnv   string `json:"key_env"`
	// available auth modes; the first one is the default
	Auth []string `json:"auth"`
	// Custom means the user supplies the base URL
	Custom bool   `json:"custom"`
	Note   string `json:"note"`
}

func ProviderCatalog() []ProviderOption {
	return []ProviderOption{
		{
			ID: "anthropic", Label: "Anthropic Claude",
			Provider: "anthropic", KeyEnv: "ANTHROPIC_API_KEY", Auth: []string{AuthKey},
			Note: i18n.T("Includes server-side web search"),
		},
		{
			ID: "openai", Label: "OpenAI / ChatGPT",
			Provider: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY",
			Auth: []string{AuthChatGPT, AuthKey},
			Note: i18n.T("You can sign in with a ChatGPT Plus/Pro subscription instead of buying API credit"),
		},
		{
			ID: "xai", Label: "xAI Grok",
			Provider: "openai", BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY",
			Auth: []string{AuthXai, AuthKey},
			Note: i18n.T("You can sign in with a SuperGrok subscription instead of buying API credit"),
		},
		{
			ID: "deepseek", Label: "DeepSeek",
			Provider: "openai", BaseURL: "https://api.deepseek.com/v1", KeyEnv: "DEEPSEEK_API_KEY",
			Auth: []string{AuthKey},
		},
		{
			ID: "moonshot", Label: i18n.T("Kimi (Moonshot)"),
			Provider: "openai", BaseURL: "https://api.moonshot.cn/v1", KeyEnv: "MOONSHOT_API_KEY",
			Auth: []string{AuthKey},
		},
		{
			ID: "opencode", Label: "OpenCode Zen",
			Provider: "openai", BaseURL: "https://opencode.ai/zen/v1", KeyEnv: "OPENCODE_API_KEY",
			Auth: []string{AuthKey},
			Note: i18n.T("One key for many vendors' models; the list below comes from OpenCode"),
		},
		{
			ID: "opencode-go", Label: "OpenCode Go",
			Provider: "openai", BaseURL: "https://opencode.ai/zen/go/v1", KeyEnv: "OPENCODE_API_KEY",
			Auth: []string{AuthKey},
		},
		{
			ID: "ollama", Label: i18n.T("Ollama (local)"),
			Provider: "openai", BaseURL: "http://127.0.0.1:11434/v1", Auth: []string{AuthNone},
			Note: i18n.T("Start ollama serve on this machine first"),
		},
		{
			ID: "custom", Label: i18n.T("Custom OpenAI-compatible service"),
			Provider: "openai", KeyEnv: "OPENAI_API_KEY", Auth: []string{AuthKey}, Custom: true,
			Note: i18n.T("Supply your own base URL and key name"),
		},
		{
			ID: "fake", Label: i18n.T("Fake (offline trial, no network)"),
			Provider: "fake", Auth: []string{AuthNone},
		},
	}
}

func providerOption(id string) *ProviderOption {
	for _, p := range ProviderCatalog() {
		if p.ID == id {
			cp := p
			return &cp
		}
	}
	return nil
}

// CredentialFunc resolves the credential for one auth mode. It is injected by the caller, since the
// key store and subscription tokens live elsewhere and this package only needs to know how to ask.
// It mirrors openAIProvider.resolveKey so a model can never be listable but unusable once an actual
// message is sent.
type Credential struct {
	Token string

	// Headers are the extra headers this credential requires. A ChatGPT subscription talks to the Codex
	// backend, where Authorization alone is not enough: originator and the account id are also checked.
	Headers map[string]string
}

type CredentialFunc func(auth, keyEnv string) (Credential, error)

// Credentials builds a resolver from the key store and the two subscription tokens.
func Credentials(ks *secret.KeyStore, xai *secret.XaiOAuth, chatgpt *secret.ChatGPTOAuth) CredentialFunc {
	return func(auth, keyEnv string) (Credential, error) {
		switch auth {
		case AuthChatGPT:
			if chatgpt == nil || !chatgpt.Connected() {
				return Credential{}, errors.New(i18n.T("Not signed in to a ChatGPT subscription"))
			}
			tok, err := chatgpt.Bearer()
			if err != nil {
				return Credential{}, err
			}
			h := map[string]string{"originator": "botbureau"}
			if id := chatgpt.AccountID(); id != "" {
				h["ChatGPT-Account-Id"] = id
			}
			return Credential{Token: tok, Headers: h}, nil
		case AuthXai:
			if xai == nil || !xai.Connected() {
				return Credential{}, errors.New(i18n.T("Not signed in to a SuperGrok subscription"))
			}
			tok, err := xai.Bearer()
			return Credential{Token: tok}, err
		case AuthNone:
			return Credential{}, nil
		default:
			if keyEnv == "" {
				keyEnv = "OPENAI_API_KEY"
			}
			if ks != nil {
				if v := ks.Get(keyEnv); v != "" {
					return Credential{Token: v}, nil
				}
			}
			return Credential{}, fmt.Errorf(i18n.T("%s has not been set yet"), keyEnv)
		}
	}
}

// withExtra folds a credential's extra headers into the base set; the extras win on a clash.
func withExtra(base, extra map[string]string) map[string]string {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// ListModels asks the vendor which models exist right now. On failure it reports the error verbatim
// and never substitutes an invented list — that is exactly how a user ends up picking a dead model id.
func ListModels(ctx context.Context, cred CredentialFunc, providerID, baseURL, keyEnv, auth string) ([]string, error) {
	opt := providerOption(providerID)
	if opt == nil {
		return nil, fmt.Errorf(i18n.T("Unknown provider %q"), providerID)
	}
	if opt.Provider == "fake" {
		return []string{"fake"}, nil
	}
	if auth == "" {
		auth = opt.Auth[0]
	}
	if baseURL == "" {
		baseURL = opt.BaseURL
	}
	if keyEnv == "" {
		keyEnv = opt.KeyEnv
	}
	c, err := cred(auth, keyEnv)
	if err != nil {
		return nil, err
	}
	key := c.Token

	// The extra headers travel too: listing and sending must present the same identity, or you get the
	// worst kind of inconsistency — a model that lists fine and is refused the moment it is used.
	extra := c.Headers

	if opt.Provider == "anthropic" {
		base := strings.TrimRight(baseURL, "/")
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return fetchModels(ctx, base+"/v1/models", withExtra(map[string]string{
			"x-api-key": key, "anthropic-version": "2023-06-01",
		}, extra))
	}

	// A ChatGPT subscription issues a Codex-scoped token, which cannot open /models on api.openai.com.
	// client_version is a required query parameter — omitting it is a 400. See secret.ChatGPTClientVersion.
	if auth == AuthChatGPT {
		return fetchModels(ctx, chatgptModelsURL(), withExtra(map[string]string{
			"Authorization": "Bearer " + key,
			"originator":    "botbureau",
			"User-Agent":    "botbureau/0.1.0",
		}, extra))
	}
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return nil, errors.New(i18n.T("Enter a base URL first"))
	}
	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	return fetchModels(ctx, base+"/models", withExtra(headers, extra))
}

func chatgptModelsURL() string {
	return secret.ChatGPTModelsURL + "?client_version=" + url.QueryEscape(secret.ChatGPTClientVersion)
}

// fetchModels accepts OpenAI's and Anthropic's {"data":[{"id":…}]} shape, the Codex backend's
// {"models":[{"slug":…}]}, and the bare ["model-a","model-b"] array some services return.
func fetchModels(ctx context.Context, url string, headers map[string]string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// Bounded read: a model list is tens of KB at most; a malformed response must not eat memory
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(i18n.T("The vendor returned %d: %s"),
			res.StatusCode, oneLine(string(raw), 200))
	}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"models"`
	}
	out := []string{}
	if json.Unmarshal(raw, &wrap) == nil {
		for _, m := range wrap.Data {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		for _, m := range wrap.Models {
			if id := m.ID; id != "" {
				out = append(out, id)
			} else if m.Slug != "" {
				out = append(out, m.Slug)
			} else if m.Name != "" {
				out = append(out, m.Name)
			}
		}
	}
	if len(out) == 0 {
		var bare []string
		if json.Unmarshal(raw, &bare) == nil {
			out = append(out, bare...)
		}
	}
	if len(out) == 0 {
		return nil, errors.New(i18n.T("The vendor returned no usable models"))
	}
	sort.Strings(out)
	return out, nil
}

// oneLine squeezes an error body onto one line. Taking the literal first line is not enough: vendors
// like to return indented JSON, which truncates to a lone "{" and tells the user nothing.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
