package api

import (
	"net/http"
	"strings"
	"testing"
)

// createSingleKeyProvider 创建一个单 Key 供应商（上游是内置 stub）。
func createSingleKeyProvider(t *testing.T, h *harness, client *http.Client, key string) (string, string) {
	t.Helper()
	id, body := createMultiKeyProvider(t, h, client, "连通性", []map[string]any{
		{"label": "主账号", "api_key": key, "enabled": true},
	})
	keyID, _ := body["keys"].([]any)[0].(map[string]any)["id"].(string)
	return id, keyID
}

func TestProviderKeyConnectivitySucceeds(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)
	id, keyID := createSingleKeyProvider(t, h, client, "sk-conn-9999zzzz")

	h.upstream.setChat(`{"id":"chatcmpl-abc","choices":[{"message":{"role":"assistant","content":"连接成功"}}],`+
		`"usage":{"prompt_tokens":38,"completion_tokens":20}}`, http.StatusOK)

	code, result, raw := h.do(client, http.MethodPost,
		"/api/providers/"+id+"/keys/"+keyID+"/connectivity",
		map[string]any{"model": "seed-model", "message": "你好，请回复“连接成功”。"})
	if code != http.StatusOK {
		t.Fatalf("连通性测试状态码 = %d（%s）", code, raw)
	}
	if result["ok"] != true {
		t.Fatalf("应调用成功: %s", raw)
	}
	if result["model"] != "seed-model" {
		t.Errorf("model = %v", result["model"])
	}
	responseRaw, _ := result["response_raw"].(string)
	if !strings.Contains(responseRaw, "chatcmpl-abc") {
		t.Errorf("应回传上游原始响应体: %q", responseRaw)
	}
	usage, _ := result["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 38 || usage["output_tokens"].(float64) != 20 {
		t.Errorf("usage = %v", usage)
	}
	if result["key_id"] != keyID {
		t.Errorf("key_id = %v", result["key_id"])
	}
	if hint, _ := result["key_hint"].(string); !strings.Contains(hint, "****") {
		t.Errorf("应回传掩码而非明文: %q", hint)
	}
	if strings.Contains(raw, "sk-conn-9999zzzz") {
		t.Fatal("响应中不得出现明文凭证")
	}

	// 上游收到的请求体应符合约定
	sent, auth := h.upstream.lastChatRequest()
	if auth != "Bearer sk-conn-9999zzzz" {
		t.Errorf("Authorization = %q", auth)
	}
	if sent["model"] != "seed-model" || sent["stream"] != false {
		t.Errorf("上游请求体 = %v", sent)
	}
	messages, _ := sent["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["content"] != "你好，请回复“连接成功”。" {
		t.Errorf("messages = %v", sent["messages"])
	}

	// 请求日志与用量都应记录（计入统计）
	entries := h.logSink.all()
	if len(entries) != 1 {
		t.Fatalf("应写入 1 条请求日志，得到 %d", len(entries))
	}
	entry := entries[0]
	if entry.InboundProtocol != connectivityProtocol {
		t.Errorf("协议 = %q，期望 %q", entry.InboundProtocol, connectivityProtocol)
	}
	if entry.ProviderID != id || entry.StatusCode != http.StatusOK || entry.GatewayKeyID != "" {
		t.Errorf("日志条目 = %+v", entry)
	}
	if entry.Model != "seed-model" || entry.KeyHint == "" {
		t.Errorf("日志应带模型与 Key 掩码: %+v", entry)
	}
	if entry.ErrorMsg != "" {
		t.Errorf("成功调用不应写错误摘要: %q", entry.ErrorMsg)
	}

	records := h.usageSink.all()
	if len(records) != 1 {
		t.Fatalf("应写入 1 条用量，得到 %d", len(records))
	}
	rec := records[0]
	if rec.providerID != id || rec.keyID != keyID || rec.model != "seed-model" || rec.statusCode != http.StatusOK {
		t.Errorf("用量归属 = %+v", rec)
	}
	if rec.gatewayKeyID != "" {
		t.Errorf("控制面调用不属于任何网关 Key，得到 %q", rec.gatewayKeyID)
	}
	if rec.usage.InputTokens != 38 || rec.usage.OutputTokens != 20 {
		t.Errorf("用量 = %+v", rec.usage)
	}
}

func TestProviderKeyConnectivityRejectsUnselectedModel(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)
	id, keyID := createSingleKeyProvider(t, h, client, "sk-conn-0000aaaa")

	code, body, raw := h.do(client, http.MethodPost,
		"/api/providers/"+id+"/keys/"+keyID+"/connectivity",
		map[string]any{"model": "not-selected"})
	if code != http.StatusBadRequest {
		t.Fatalf("未勾选模型应 400，得到 %d（%s）", code, raw)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "不在该供应商已勾选") {
		t.Errorf("错误信息 = %q", msg)
	}
	if len(h.logSink.all()) != 0 || len(h.usageSink.all()) != 0 {
		t.Error("被拒绝的请求不应写日志与用量")
	}
}

func TestProviderKeyConnectivityUpstreamFailure(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)
	id, keyID := createSingleKeyProvider(t, h, client, "sk-conn-1111bbbb")

	h.upstream.setChat(`{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	code, result, raw := h.do(client, http.MethodPost,
		"/api/providers/"+id+"/keys/"+keyID+"/connectivity",
		map[string]any{"model": "seed-model"})
	if code != http.StatusOK {
		t.Fatalf("上游失败仍应 200 返回诊断结果，得到 %d（%s）", code, raw)
	}
	if result["ok"] != false || result["status_code"].(float64) != http.StatusUnauthorized {
		t.Fatalf("应报告上游失败: %s", raw)
	}
	if msg, _ := result["message"].(string); !strings.Contains(msg, "401") {
		t.Errorf("message = %q", msg)
	}

	// 失败调用写日志但不计用量
	entries := h.logSink.all()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusUnauthorized {
		t.Fatalf("失败调用应写日志: %+v", entries)
	}
	if entries[0].ErrorMsg == "" {
		t.Error("失败调用应写错误摘要")
	}
	if len(h.usageSink.all()) != 0 {
		t.Error("失败调用不应计用量")
	}
}

func TestProviderKeyConnectivityUnknownKey(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)
	id, _ := createSingleKeyProvider(t, h, client, "sk-conn-2222cccc")

	code, _, _ := h.do(client, http.MethodPost,
		"/api/providers/"+id+"/keys/pk_nope/connectivity", map[string]any{"model": "seed-model"})
	if code != http.StatusNotFound {
		t.Errorf("未知 Key 应 404，得到 %d", code)
	}
	code, _, _ = h.do(client, http.MethodPost,
		"/api/providers/p_nope/keys/pk_nope/connectivity", map[string]any{"model": "seed-model"})
	if code != http.StatusNotFound {
		t.Errorf("未知供应商应 404，得到 %d", code)
	}
}

func TestDraftConnectivityUsesFormPlaintext(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)
	h.upstream.setChat(`{"usage":{"prompt_tokens":11,"completion_tokens":7}}`, http.StatusOK)

	body := map[string]any{
		"openai_base_url": h.upstreamURL + "/v1",
		"keys": []map[string]any{
			{"label": "第一个", "api_key": "sk-draft-3333dddd"},
		},
		"allow_internal":  true,
		"models_selected": []string{"seed-model"},
		"model":           "seed-model",
	}
	code, result, raw := h.do(client, http.MethodPost, "/api/providers/connectivity", body)
	if code != http.StatusOK || result["ok"] != true {
		t.Fatalf("草稿连通性测试应通过: code=%d %s", code, raw)
	}
	if _, auth := h.upstream.lastChatRequest(); auth != "Bearer sk-draft-3333dddd" {
		t.Errorf("草稿明文应作为 Bearer 发出，得到 %q", auth)
	}
	// 草稿没有库内 Key id，用量归属的 keyID 为空是预期行为
	records := h.usageSink.all()
	if len(records) != 1 || records[0].usage.InputTokens != 11 {
		t.Fatalf("草稿调用也应计用量: %+v", records)
	}
	if entries := h.logSink.all(); len(entries) != 1 || entries[0].ProviderID != "draft" {
		t.Errorf("草稿调用的日志归属 = %+v", entries)
	}

	// 缺少 Key 应 400
	code, _, _ = h.do(client, http.MethodPost, "/api/providers/connectivity", map[string]any{
		"openai_base_url": h.upstreamURL + "/v1",
		"model":           "seed-model",
	})
	if code != http.StatusBadRequest {
		t.Errorf("缺少 Key 应 400，得到 %d", code)
	}
}

func TestConnectivityWithoutSinksStillResponds(t *testing.T) {
	// 未注入 sink（例如命令行工具/最小装配）时不能 panic：New 应接受空依赖。
	s := New(Options{})
	if s.logSink != nil || s.usageSink != nil {
		t.Fatal("默认应为 nil sink")
	}
}
