package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
)

// chatUpstream 是一个只服务 /v1/chat/completions 的上游，记录收到的请求。
type chatUpstream struct {
	path       string
	auth       string
	body       map[string]any
	extraCheck func(*http.Request) error
}

func newChatUpstream(t *testing.T, status int, response string) (*chatUpstream, *httptest.Server) {
	t.Helper()
	up := &chatUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.path = r.URL.Path
		up.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		up.body = map[string]any{}
		_ = json.Unmarshal(raw, &up.body)
		if up.extraCheck != nil {
			if err := up.extraCheck(r); err != nil {
				w.WriteHeader(http.StatusTeapot)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return up, srv
}

func connectivityProvider(t *testing.T, srv *httptest.Server, models ...string) config.Provider {
	t.Helper()
	enabled := true
	p := config.Provider{
		ID:            "p1",
		Name:          "供应商 p1",
		OpenAIBaseURL: srv.URL + "/v1",
		APIKeys:       []config.ProviderKey{{ID: "k1", Label: "主", Key: "sk-connect-1234", Enabled: &enabled}},
		Models:        models,
		Enabled:       &enabled,
	}
	p.CleanSelection()
	return p
}

func TestCallSendsRealChatRequest(t *testing.T) {
	up, srv := newChatUpstream(t, http.StatusOK,
		`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"连接成功"}}],`+
			`"usage":{"prompt_tokens":38,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":8}}}`)
	p := connectivityProvider(t, srv, "DeepSeek-V4.1-Flash-1017")

	res, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "DeepSeek-V4.1-Flash-1017", Message: "你好，请回复“连接成功”。"},
		"req-1", &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Call 返回错误: %v", err)
	}
	if !res.OK || res.StatusCode != http.StatusOK {
		t.Fatalf("应调用成功: %+v", res)
	}
	if up.path != "/v1/chat/completions" {
		t.Errorf("请求路径 = %q", up.path)
	}
	if up.auth != "Bearer sk-connect-1234" {
		t.Errorf("Authorization = %q", up.auth)
	}
	if up.body["model"] != "DeepSeek-V4.1-Flash-1017" {
		t.Errorf("model = %v", up.body["model"])
	}
	if up.body["stream"] != false {
		t.Errorf("stream 应为 false，得到 %v", up.body["stream"])
	}
	if int(up.body["max_tokens"].(float64)) != ConnectivityMaxTokens {
		t.Errorf("max_tokens = %v", up.body["max_tokens"])
	}
	messages, _ := up.body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v", up.body["messages"])
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "你好，请回复“连接成功”。" {
		t.Errorf("messages[0] = %v", first)
	}
	if res.Usage == nil || res.Usage.InputTokens != 38 || res.Usage.OutputTokens != 20 ||
		res.Usage.CachedInputTokens != 8 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if !strings.Contains(res.ResponseRaw, "chatcmpl-1") {
		t.Errorf("应回传原始响应体，得到 %q", res.ResponseRaw)
	}
	if res.RequestID != "req-1" || res.Model != "DeepSeek-V4.1-Flash-1017" {
		t.Errorf("request_id / model 未透传: %+v", res)
	}
	if res.CheckedAt.IsZero() {
		t.Error("CheckedAt 应被填充")
	}
}

func TestCallRejectsUnselectedModel(t *testing.T) {
	up, srv := newChatUpstream(t, http.StatusOK, `{}`)
	p := connectivityProvider(t, srv, "seed-model")

	_, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "not-selected"}, "req", &http.Client{Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "不在该供应商已勾选") {
		t.Fatalf("未勾选模型应被拒绝，得到 %v", err)
	}
	if up.path != "" {
		t.Errorf("被拒绝时不应发出请求，实际打到 %q", up.path)
	}
}

func TestCallAcceptsAlias(t *testing.T) {
	up, srv := newChatUpstream(t, http.StatusOK, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	enabled := true
	p := config.Provider{
		ID: "p1", Name: "p1", OpenAIBaseURL: srv.URL + "/v1",
		APIKeys:      []config.ProviderKey{{ID: "k1", Key: "sk", Enabled: &enabled}},
		Models:       []string{"upstream-model"},
		ModelAliases: map[string]string{"upstream-model": "对外别名"},
		Enabled:      &enabled,
	}
	p.CleanSelection()

	res, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "对外别名"}, "req", &http.Client{Timeout: time.Second})
	if err != nil || !res.OK {
		t.Fatalf("别名应能探测: err=%v res=%+v", err, res)
	}
	if up.body["model"] != "upstream-model" {
		t.Errorf("应改用上游真实模型名，得到 %v", up.body["model"])
	}
}

func TestCallReportsUpstreamError(t *testing.T) {
	up, srv := newChatUpstream(t, http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)
	p := connectivityProvider(t, srv, "seed-model")

	res, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "seed-model"}, "req", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatalf("上游 401 不算调用错误: %v", err)
	}
	if res.OK || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应报告失败状态码: %+v", res)
	}
	if !strings.Contains(res.Message, "401") {
		t.Errorf("Message 应含状态码: %q", res.Message)
	}
	if !strings.Contains(res.ResponseRaw, "invalid api key") {
		t.Errorf("失败时也应回传原始响应体: %q", res.ResponseRaw)
	}
	if res.Usage != nil {
		t.Errorf("失败调用不应解析 usage: %+v", res.Usage)
	}
	if up.path != "/v1/chat/completions" {
		t.Errorf("请求路径 = %q", up.path)
	}
}

func TestCallWithoutUsageStillSucceeds(t *testing.T) {
	_, srv := newChatUpstream(t, http.StatusOK, `{"choices":[{"message":{"content":"pong"}}]}`)
	p := connectivityProvider(t, srv, "seed-model")

	res, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "seed-model"}, "req", &http.Client{Timeout: time.Second})
	if err != nil || !res.OK {
		t.Fatalf("无 usage 仍应视为调用成功: err=%v res=%+v", err, res)
	}
	if res.Usage != nil {
		t.Errorf("上游没给 usage 时 Usage 应为 nil: %+v", res.Usage)
	}
	if !strings.Contains(res.Message, "未返回 usage") {
		t.Errorf("Message = %q", res.Message)
	}
}

func TestCallUsesDefaultMessageAndExtraBody(t *testing.T) {
	up, srv := newChatUpstream(t, http.StatusOK, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	enabled := true
	p := config.Provider{
		ID: "p1", Name: "p1", OpenAIBaseURL: srv.URL + "/v1",
		APIKeys:   []config.ProviderKey{{ID: "k1", Key: "sk", Enabled: &enabled}},
		Models:    []string{"m"},
		ExtraBody: map[string]any{"temperature": 0.1},
		ExtraHeaders: map[string]string{
			"X-Tenant": "me",
		},
		Enabled: &enabled,
	}
	p.CleanSelection()
	up.extraCheck = func(r *http.Request) error {
		if r.Header.Get("X-Tenant") != "me" {
			return errString("缺少额外请求头")
		}
		return nil
	}

	res, err := Call(context.Background(), p, p.APIKeys[0], ConnectivityRequest{Model: "m"},
		"req", &http.Client{Timeout: time.Second})
	if err != nil || !res.OK {
		t.Fatalf("应调用成功: err=%v res=%+v", err, res)
	}
	if up.body["temperature"] != 0.1 {
		t.Errorf("extra_body 应合并进请求体: %v", up.body["temperature"])
	}
	messages, _ := up.body["messages"].([]any)
	if content := messages[0].(map[string]any)["content"]; content != DefaultConnectivityMessage {
		t.Errorf("空消息应使用默认文案，得到 %v", content)
	}
}

func TestCallRejectsOverlongMessage(t *testing.T) {
	_, srv := newChatUpstream(t, http.StatusOK, `{}`)
	p := connectivityProvider(t, srv, "m")
	_, err := Call(context.Background(), p, p.APIKeys[0],
		ConnectivityRequest{Model: "m", Message: strings.Repeat("啊", MaxConnectivityMessageBytes)},
		"req", &http.Client{Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "过长") {
		t.Fatalf("超长消息应被拒绝，得到 %v", err)
	}
}

// errString 是给 extraCheck 用的简单错误类型。
type errString string

func (e errString) Error() string { return string(e) }
