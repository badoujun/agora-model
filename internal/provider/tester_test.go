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

// TestTestFallsOverAcrossKeys 覆盖多凭证：首个 Key 401 时自动切换到下一个并成功。
func TestTestFallsOverAcrossKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-good" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer upstream.Close()

	enabled := true
	p := config.Provider{
		ID:            "p",
		Name:          "p",
		OpenAIBaseURL: upstream.URL + "/v1",
		Models:        []string{"m1"},
		APIKeys: []config.ProviderKey{
			{ID: "k1", Label: "坏的", Key: "sk-bad", Enabled: &enabled},
			{ID: "k2", Label: "好的", Key: "sk-good", Enabled: &enabled},
		},
	}
	p.CleanSelection()

	res := Test(context.Background(), p, &http.Client{Timeout: 5 * time.Second})
	if !res.OK {
		t.Fatalf("应回退到第二个 Key，得到 %+v", res)
	}
	if len(res.Keys) != 2 {
		t.Fatalf("应返回每个尝试过的 Key 的结果，得到 %+v", res.Keys)
	}
	if res.Keys[0].OK || !res.Keys[1].OK {
		t.Fatalf("Key 结果应分别是失败/成功：%+v", res.Keys)
	}
	if res.Keys[0].KeyID != "k1" || res.Keys[1].KeyID != "k2" {
		t.Errorf("KeyID 未透传：%+v", res.Keys)
	}
	if !strings.Contains(res.Message, "切换到第 2 个") {
		t.Errorf("消息应说明发生了切换，得到 %q", res.Message)
	}
}

// TestTestKeySingleKey 覆盖「只测某一个 Key」：即使其他 Key 不可用也不影响。
func TestTestKeySingleKey(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		if seen == "Bearer sk-good" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	enabled := true
	p := config.Provider{ID: "p", Name: "p", OpenAIBaseURL: upstream.URL + "/v1", Models: []string{"m1"}}

	res := TestKey(context.Background(), p, config.ProviderKey{ID: "k2", Label: "好的", Key: "sk-good", Enabled: &enabled},
		&http.Client{Timeout: 5 * time.Second})
	if !res.OK || seen != "Bearer sk-good" {
		t.Fatalf("单 Key 测试应只使用该 Key：res=%+v auth=%q", res, seen)
	}
	if len(res.Keys) != 1 || res.Keys[0].KeyID != "k2" {
		t.Fatalf("结果应只含该 Key：%+v", res.Keys)
	}
}

// TestTestWithoutAnyUsableKey 覆盖没有任何可用凭证时的提示。
func TestTestWithoutAnyUsableKey(t *testing.T) {
	disabled := false
	p := config.Provider{
		ID: "p", Name: "p", OpenAIBaseURL: "http://x/v1",
		APIKeys: []config.ProviderKey{{ID: "k1", Key: "sk-1", Enabled: &disabled}},
	}
	p.CleanSelection()
	res := Test(context.Background(), p, &http.Client{Timeout: time.Second})
	if res.OK || !strings.Contains(res.Message, "没有可用的 API Key") {
		t.Fatalf("应提示没有可用 Key，得到 %+v", res)
	}
}
