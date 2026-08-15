package api

import (
	"botbureau/backend/internal/model"

	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 显式选了订阅，就必须用订阅的 token——哪怕钥匙串里躺着一个同名的 API key。
// 旧逻辑是"没存 key 才回退到订阅"，于是存过一个无关的 OPENAI_API_KEY 就会把刚登录的订阅悄悄顶掉。
//
// An explicitly chosen subscription must win over a stored API key of the same name. The old rule only
// fell back to the subscription when no key was stored, so an unrelated stored OPENAI_API_KEY silently
// shadowed the subscription the user had just signed into.
func TestListModelsLiveAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			rw.WriteHeader(401)
			_, _ = rw.Write([]byte(`{"error":"bad key"}`))
			return
		}
		_, _ = rw.Write([]byte(`{"data":[{"id":"zeta-1"},{"id":"alpha-1"}]}`))
	}))
	defer srv.Close()

	app, httpSrv := newTestApp(t)
	defer httpSrv.Close()
	if err := app.deps.KS.Set("OPENAI_API_KEY", "sk-test"); err != nil {
		t.Fatal(err)
	}
	models, err := model.ListModels(context.Background(), app.credentials(), "custom", srv.URL, "OPENAI_API_KEY", model.AuthKey)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	// 排序后返回，界面上下拉框才稳定 / sorted so the dropdown order is stable
	if len(models) != 2 || models[0] != "alpha-1" || models[1] != "zeta-1" {
		t.Fatalf("wrong model list: %v", models)
	}

	// 凭据不对：如实报错，绝不返回一份编出来的型号表
	// Wrong credential: report the error rather than substituting an invented list
	if err := app.deps.KS.Set("OPENAI_API_KEY", "sk-wrong"); err != nil {
		t.Fatal(err)
	}
	if got, err := model.ListModels(context.Background(), app.credentials(), "custom", srv.URL, "OPENAI_API_KEY", model.AuthKey); err == nil {
		t.Fatalf("bad credential still returned %v", got)
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("the error should carry the status code: %v", err)
	}

	// 没登录订阅：连请求都不该发出去
	// Not signed in: the request must not even be attempted
	if _, err := model.ListModels(context.Background(), app.credentials(), "openai", "", "", model.AuthChatGPT); err == nil {
		t.Fatal("should error when not signed in")
	}
	if _, err := model.ListModels(context.Background(), app.credentials(), "nope", "", "", model.AuthKey); err == nil {
		t.Fatal("unknown vendor should error")
	}
}
