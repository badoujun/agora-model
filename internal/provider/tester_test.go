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
		ID:               id,
		OpenAIBaseURL:    upstreamURL + "/v1",
		AnthropicBaseURL: upstreamURL + "/v1",
		APIKey:           "sk-up-" + id,
		Models:           models,
		Enabled:          &enabled,
	}
}

func TestTestBothProtocolsOK(t *testing.T) {
	var (
		gotOpenAIAuth    string
		gotAnthropicKey  string
		gotAnthropicVer  string
		gotAnthropicPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			gotOpenAIAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`))
		case "/v1/messages":
			gotAnthropicKey = r.Header.Get("x-api-key")
			gotAnthropicVer = r.Header.Get("anthropic-version")
			gotAnthropicPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"pong"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	res := Test(context.Background(), providerFor("p1", upstream.URL, "m1"), &http.Client{Timeout: 5 * time.Second})

	if !res.OpenAI.OK || res.OpenAI.ModelCount != 2 {
		t.Fatalf("OpenAI 探测应成功并返回模型数，得到 %+v", res.OpenAI)
	}
	if gotOpenAIAuth != "Bearer sk-up-p1" {
		t.Errorf("OpenAI 探测的 Authorization = %q", gotOpenAIAuth)
	}
	if !res.Anthropic.OK {
		t.Fatalf("Anthropic 探测应成功，得到 %+v", res.Anthropic)
	}
	if gotAnthropicKey != "sk-up-p1" {
		t.Errorf("Anthropic 探测的 x-api-key = %q", gotAnthropicKey)
	}
	if gotAnthropicVer == "" {
		t.Error("Anthropic 探测应带 anthropic-version 头")
	}
	if gotAnthropicPath != "/v1/messages" {
		t.Errorf("Anthropic 探测路径 = %q", gotAnthropicPath)
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
	if !res.OpenAI.OK {
		t.Fatalf("应回退到最小对话探测并成功，得到 %+v", res.OpenAI)
	}
	if !chatCalled {
		t.Fatal("/models 返回 404 后应尝试 /chat/completions")
	}
}

func TestTestReportsMissingProtocols(t *testing.T) {
	onlyOpenAI := config.Provider{
		ID:            "p1",
		OpenAIBaseURL: "http://127.0.0.1:1/v1",
		APIKey:        "sk",
		Models:        []string{"m"},
	}
	res := Test(context.Background(), onlyOpenAI, &http.Client{Timeout: time.Second})
	if res.Anthropic.OK {
		t.Fatal("未配置 Anthropic 地址时该项应失败")
	}
	if !strings.Contains(res.Anthropic.Message, "未配置 Anthropic") {
		t.Errorf("Anthropic 结果应说明未配置: %q", res.Anthropic.Message)
	}
}

func TestTestReportsUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	// 只配了 OpenAI 地址，避免 Anthropic 侧的空模型提示干扰
	p := config.Provider{ID: "p1", OpenAIBaseURL: upstream.URL + "/v1", APIKey: "sk-bad", Models: []string{"m"}}
	res := Test(context.Background(), p, &http.Client{Timeout: 5 * time.Second})
	if res.OpenAI.OK {
		t.Fatal("401 不应被视为成功")
	}
	if res.OpenAI.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d", res.OpenAI.StatusCode)
	}
	if !strings.Contains(res.OpenAI.Message, "401") {
		t.Errorf("Message 应包含状态码: %q", res.OpenAI.Message)
	}
}

func TestTestWithoutModelsReportsHint(t *testing.T) {
	// Anthropic 侧需要模型名才能探测
	p := config.Provider{ID: "p1", AnthropicBaseURL: "http://127.0.0.1:1/v1", APIKey: "sk"}
	res := Test(context.Background(), p, &http.Client{Timeout: time.Second})
	if res.Anthropic.OK || !strings.Contains(res.Anthropic.Message, "未配置任何模型") {
		t.Fatalf("无模型时应给出提示，得到 %+v", res.Anthropic)
	}
}
