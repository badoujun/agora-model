package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
)

func providerFor(id, upstreamURL string, models ...string) config.Provider {
	enabled := true
	return config.Provider{
		ID:            id,
		Name:          "供应商 " + id,
		OpenAIBaseURL: upstreamURL + "/v1",
		APIKey:        "sk-up-" + id,
		Models:        models,
		Enabled:       &enabled,
	}
}

func TestTestModelsEndpointOK(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	res := Test(context.Background(), providerFor("p1", upstream.URL, "m1"), &http.Client{Timeout: 5 * time.Second})

	if !res.OK || res.ModelCount != 2 {
		t.Fatalf("探测应成功并返回模型数，得到 %+v", res)
	}
	if gotAuth != "Bearer sk-up-p1" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if res.CheckedAt.IsZero() {
		t.Error("CheckedAt 应被填充")
	}
}

func TestTestFallsBackToChatWhenModelsUnavailable(t *testing.T) {
	var chatCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/chat/completions":
			chatCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	res := Test(context.Background(), providerFor("p1", upstream.URL, "m1"), &http.Client{Timeout: 5 * time.Second})
	if !res.OK {
		t.Fatalf("应回退到最小对话探测并成功，得到 %+v", res)
	}
	if !chatCalled {
		t.Fatal("/models 返回 404 后应尝试 /chat/completions")
	}
	if !strings.Contains(res.Message, "最小对话请求") {
		t.Errorf("回退提示缺失: %q", res.Message)
	}
}

func TestTestEndpointOverrideUsesChatProbe(t *testing.T) {
	var chatCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom/completions" {
			chatCalled = true
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	p := config.Provider{
		ID:                     "p1",
		OpenAIEndpointOverride: upstream.URL + "/custom/completions",
		APIKey:                 "sk-up",
		Models:                 []string{"m1"},
	}
	res := Test(context.Background(), p, &http.Client{Timeout: 5 * time.Second})
	if !res.OK || !chatCalled {
		t.Fatalf("端点覆盖模式应直接用最小对话探测，得到 %+v", res)
	}
}

func TestTestReportsUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	p := config.Provider{ID: "p1", OpenAIBaseURL: upstream.URL + "/v1", APIKey: "sk-bad", Models: []string{"m"}}
	res := Test(context.Background(), p, &http.Client{Timeout: 5 * time.Second})
	if res.OK {
		t.Fatal("401 不应被视为成功")
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d", res.StatusCode)
	}
	if !strings.Contains(res.Message, "401") {
		t.Errorf("Message 应包含状态码: %q", res.Message)
	}
}

func TestTestWithoutModelsReportsHint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	// 未勾选任何模型时无法做最小对话探测
	p := config.Provider{ID: "p1", OpenAIBaseURL: upstream.URL + "/v1", APIKey: "sk"}
	res := Test(context.Background(), p, &http.Client{Timeout: time.Second})
	if res.OK || !strings.Contains(res.Message, "未勾选任何模型") {
		t.Fatalf("无模型时应给出提示，得到 %+v", res)
	}
}

func TestTestWithoutBaseURLReportsHint(t *testing.T) {
	res := Test(context.Background(), config.Provider{ID: "p1", APIKey: "sk"}, &http.Client{Timeout: time.Second})
	if res.OK || !strings.Contains(res.Message, "openai_base_url") {
		t.Fatalf("未配置地址时应给出提示，得到 %+v", res)
	}
}
