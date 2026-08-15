package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"botbureau/backend/internal/secret"
)

// Codex 后端的 /models 有两处和 OpenAI 不一样，任何一处踩空，下拉框里就一个模型都列不出来：
// 必填的 client_version 查询参数，以及用 slug 而不是 id 的返回体。
//
// The Codex /models endpoint differs from OpenAI's in two ways, and missing either empties the model
// dropdown entirely: the required client_version query parameter, and a body keyed by slug, not id.
func TestChatGPTModelsURLCarriesClientVersion(t *testing.T) {
	u, err := url.Parse(chatgptModelsURL())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("client_version"); got != secret.ChatGPTClientVersion || got == "" {
		t.Fatalf("client_version is missing from the models URL; the vendor answers 400 without it (got %q)", got)
	}
	if !strings.HasPrefix(chatgptModelsURL(), secret.ChatGPTModelsURL) {
		t.Fatalf("models URL no longer points at the Codex endpoint: %s", chatgptModelsURL())
	}
}

func TestFetchModelsReadsCodexSlugs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.4-mini"}]}`))
	}))
	defer srv.Close()

	got, err := fetchModels(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "gpt-5.4-mini" || got[1] != "gpt-5.6-sol" {
		t.Fatalf("slug-keyed models were not read back: %v", got)
	}
}

// 服务商的报错常常是缩进过的 JSON。只取第一行等于把报错截成一个 "{"，
// 用户看到的等于没说，排查也无从下手。
//
// Vendor errors often arrive as indented JSON. Cutting at the first line leaves the user with a lone
// "{" — no information, and nothing to debug from.
func TestVendorErrorSurvivesPrettyPrintedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(400)
		rw.Write([]byte("{\n  \"error\": {\n    \"message\": \"Field required: client_version\"\n  }\n}"))
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("a 400 should surface as an error")
	}
	if !strings.Contains(err.Error(), "Field required: client_version") {
		t.Fatalf("the vendor's reason was truncated away: %q", err.Error())
	}
}
