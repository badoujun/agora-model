package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agora-model/internal/logging"
)

// TestParseOpenAIUsageNonStream 覆盖 OpenAI 协议的非流式 usage 解析。
func TestParseOpenAIUsageNonStream(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    Usage
		wantOK  bool
	}{
		{
			name: "完整 usage 块 + cached",
			body: `{"id":"x","usage":{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,"prompt_tokens_details":{"cached_tokens":80}}}`,
			want: Usage{InputTokens: 120, CachedInputTokens: 80, OutputTokens: 40}, wantOK: true,
		},
		{
			name: "无 cached_tokens",
			body: `{"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`,
			want: Usage{InputTokens: 50, CachedInputTokens: 0, OutputTokens: 10}, wantOK: true,
		},
		{
			name:   "无 usage 字段",
			body:   `{"id":"x","choices":[]}`,
			wantOK: false,
		},
		{
			name:   "usage 全 0",
			body:   `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
			wantOK: false,
		},
		{
			name: "cached > input（异常数据，clamp）",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":100}}}`,
			want: Usage{InputTokens: 10, CachedInputTokens: 10, OutputTokens: 5}, wantOK: true,
		},
		{
			name:   "非法 JSON",
			body:   `{`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseOpenAIUsageNonStream([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Fatalf("usage = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestParseAnthropicUsageNonStream 覆盖 Anthropic 协议的非流式 usage 解析。
func TestParseAnthropicUsageNonStream(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    Usage
		wantOK  bool
	}{
		{
			name: "完整 usage 块 + cache_read",
			body: `{"id":"x","usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":40}}`,
			want: Usage{InputTokens: 120, CachedInputTokens: 40, OutputTokens: 20}, wantOK: true,
		},
		{
			name: "无 cache 字段",
			body: `{"usage":{"input_tokens":50,"output_tokens":10}}`,
			want: Usage{InputTokens: 50, CachedInputTokens: 0, OutputTokens: 10}, wantOK: true,
		},
		{
			name: "全 0",
			body: `{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
			wantOK: false,
		},
		{
			name:   "无 usage",
			body:   `{"id":"x","content":[]}`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseAnthropicUsageNonStream([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Fatalf("usage = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestOpenAIUsageAccumulator 验证流式 chunk 的 OpenAI usage 聚合：
// 最后写入的 chunk 覆盖前值（OpenAI 协议约定只有最终 chunk 含完整 usage）。
func TestOpenAIUsageAccumulator(t *testing.T) {
	acc := &openAIUsageAccumulator{}
	acc.Feed([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
	if u, ok := acc.Result(); ok {
		t.Fatalf("未触发 usage 解析时应 ok=false，实际 got=%+v", u)
	}
	acc.Feed([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":30}}}`))
	want := Usage{InputTokens: 100, CachedInputTokens: 30, OutputTokens: 50}
	got, ok := acc.Result()
	if !ok {
		t.Fatal("应 ok=true")
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	// 第二次喂的相同 chunk（OpenAI 偶尔会重发尾部）—— 后续不出现更新；当前实现覆盖旧值
	acc.Feed([]byte(`{"usage":{"prompt_tokens":200,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":60}}}`))
	want2 := Usage{InputTokens: 200, CachedInputTokens: 60, OutputTokens: 80}
	got2, _ := acc.Result()
	if got2 != want2 {
		t.Fatalf("usage = %+v, want %+v", got2, want2)
	}
}

// TestAnthropicUsageAccumulator 验证流式 Anthropic usage 聚合：
// message_start 提供 input + cache_read，message_delta 提供 output。
func TestAnthropicUsageAccumulator(t *testing.T) {
	acc := &anthropicUsageAccumulator{}
	if u, ok := acc.Result(); ok {
		t.Fatalf("未喂事件时应 ok=false，实际 got=%+v", u)
	}
	acc.Feed("message_start", []byte(`{"message":{"usage":{"input_tokens":80,"cache_read_input_tokens":40}}}`))
	acc.Feed("message_delta", []byte(`{"usage":{"output_tokens":20}}`))
	want := Usage{InputTokens: 120, CachedInputTokens: 40, OutputTokens: 20}
	got, ok := acc.Result()
	if !ok {
		t.Fatal("应 ok=true")
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// TestSplitSSEEvent 验证 Anthropic SSE 事件块解析（含 / 不含 event 行、多 data 行）。
func TestSplitSSEEvent(t *testing.T) {
	cases := []struct {
		name      string
		block     string
		wantEvent string
		wantData  string
	}{
		{
			name:      "标准 event + data",
			block:     "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10}}}",
			wantEvent: "message_start",
			wantData:  `{"message":{"usage":{"input_tokens":10}}}`,
		},
		{
			name:      "无 event 行（默认空）",
			block:     "data: {\"foo\":1}",
			wantEvent: "",
			wantData:  `{"foo":1}`,
		},
		{
			name:      "空行被忽略",
			block:     "\n\ndata: {\"x\":1}\n\n",
			wantEvent: "",
			wantData:  `{"x":1}`,
		},
		{
			name:      "完全空",
			block:     "",
			wantEvent: "",
			wantData:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			event, data := splitSSEEvent([]byte(c.block))
			if event != c.wantEvent {
				t.Errorf("event = %q, want %q", event, c.wantEvent)
			}
			if string(data) != c.wantData {
				t.Errorf("data = %q, want %q", string(data), c.wantData)
			}
		})
	}
}

// TestParseUsageNonStream 验证分发逻辑（按 Content-Type 选 OpenAI / Anthropic）。
func TestParseUsageNonStream(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0}}`)

	// 空 body 一定不解析
	if _, ok := parseUsageNonStream("application/json", nil); ok {
		t.Error("空 body 应返回 ok=false")
	}

	// Anthropic content-type 走 Anthropic 解析
	got, ok := parseUsageNonStream("application/json; charset=utf-8", body)
	// 因为我们这里 content-type 不含 anthropic，应该走 OpenAI 解析，而 OpenAI envelope 不匹配
	if ok {
		t.Errorf("OpenAI envelope 应不匹配 Anthropic body，got=%+v", got)
	}

	// 改成 anthropic content-type
	got2, ok2 := parseUsageNonStream("application/json; profile=anthropic", body)
	if !ok2 {
		t.Error("anthropic content-type 应能解析 Anthropic body")
	}
	if got2.InputTokens != 10 || got2.OutputTokens != 5 {
		t.Errorf("usage = %+v", got2)
	}
}

// TestRecordUsage 验证 statusCode>=400 跳过、零值跳过、nil sink 不 panic。
func TestRecordUsage(t *testing.T) {
	var got Usage
	var gotProvider, gotModel string
	var gotStatus int
	var gotTS time.Time
	sink := logging.UsageSinkFunc(func(p, m string, s int, u Usage, ts time.Time) {
		gotProvider, gotModel, gotStatus = p, m, s
		got = u
		gotTS = ts
	})

	// nil sink 不能 panic
	recordUsage(nil, "p", "m", 200, Usage{InputTokens: 1, OutputTokens: 1}, time.Now())

	// 失败请求跳过
	recordUsage(sink, "p", "m", 500, Usage{InputTokens: 1, OutputTokens: 1}, time.Now())
	if gotStatus == 500 {
		t.Error("statusCode>=400 应跳过")
	}

	// 零值跳过
	recordUsage(sink, "p", "m", 200, Usage{}, time.Now())
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("零值应跳过；got=%+v", got)
	}

	// 正常调用
	now := time.Unix(1700000000, 0)
	recordUsage(sink, "providerA", "modelB", 200, Usage{InputTokens: 100, OutputTokens: 50}, now)
	if gotProvider != "providerA" || gotModel != "modelB" || gotStatus != 200 {
		t.Errorf("provider/model/status 未透传：%s/%s/%d", gotProvider, gotModel, gotStatus)
	}
	if got.InputTokens != 100 || got.OutputTokens != 50 || gotTS != now {
		t.Errorf("usage/ts 未透传：%+v ts=%v", got, gotTS)
	}
}

// TestTeeNonStreamBody 验证 io.TeeReader 透传 + 副本缓冲内容一致。
func TestTeeNonStreamBody(t *testing.T) {
	src := strings.NewReader(`{"hello":"world"}`)
	var buf bytes.Buffer
	w := &nopWriter{}
	n, err := teeNonStreamBody(w, src, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(`{"hello":"world"}`)) {
		t.Errorf("n=%d", n)
	}
	if buf.String() != `{"hello":"world"}` {
		t.Errorf("buf=%q", buf.String())
	}
	if w.String() != `{"hello":"world"}` {
		t.Errorf("writer=%q", w.String())
	}
}

// nopWriter 是最小 Writer 实现，专给 teeNonStreamBody 测试用。
type nopWriter struct{ bytes.Buffer }

func (n *nopWriter) Write(p []byte) (int, error) { return n.Buffer.Write(p) }

// ---------------------------------------------------------------------------
// openAIUsageAccumulator Feed 的 SSE 拆包兼容：覆盖 pumpUpstream 把多事件
// 拼到一个 Read 时，feedOne 仍能拿到含 usage 的事件。
// ---------------------------------------------------------------------------

// TestOpenAIUsageAccumulatorConcatenatedChunks 验证当 pumpUpstream 把 "choices
// chunk + usage chunk + [DONE]" 拼在一次 Read 里时，accumulator 仍能解析出 usage。
//
// 这是 mock-upstream 与部分 OpenAI 兼容厂商实际观察到的拼接模式。
func TestOpenAIUsageAccumulatorConcatenatedChunks(t *testing.T) {
	acc := &openAIUsageAccumulator{}
	chunk := []byte(
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	acc.Feed(chunk)

	want := Usage{InputTokens: 12, OutputTokens: 8}
	got, ok := acc.Result()
	if !ok {
		t.Fatalf("应 ok=true（usage 被含在拼接字节内）")
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// TestOpenAIUsageAccumulatorFinalUsageWins 验证多次 Feed 时"最后一次出现 usage
// 覆盖前值"——OpenAI 协议约定：尾部 chunk 含完整 usage，前序 choices chunk 不带。
func TestOpenAIUsageAccumulatorFinalUsageWins(t *testing.T) {
	acc := &openAIUsageAccumulator{}
	acc.Feed([]byte(`data: {"choices":[{"delta":{"content":"chunk-1"}}]}` + "\n\n"))
	// 此时没有 usage；acc.Result() 应 ok=false
	if _, ok := acc.Result(); ok {
		t.Fatalf("仅 choices chunk 时应 ok=false")
	}
	acc.Feed([]byte(`data: {"choices":[{"delta":{"content":"chunk-2"}}]}` + "\n\n"))
	if _, ok := acc.Result(); ok {
		t.Fatalf("仍只有 choices 时应 ok=false")
	}
	acc.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n"))
	want := Usage{InputTokens: 100, CachedInputTokens: 30, OutputTokens: 50}
	got, ok := acc.Result()
	if !ok {
		t.Fatalf("尾部 usage chunk 应被解析")
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// TestOpenAIUsageAccumulatorIgnoresNonDataLines 验证仅含 event:/id:/注释行的
// SSE 块不会被当成数据负载，且空块、不带 data: 行的块都不会让 feedOne 报错。
func TestOpenAIUsageAccumulatorIgnoresNonDataLines(t *testing.T) {
	acc := &openAIUsageAccumulator{}
	chunk := []byte(
		`: keep-alive comment` + "\n\n" + // SSE 注释行
			`event: ping` + "\n" +
			`id: 42` + "\n\n" +
			`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"usage":{"prompt_tokens":7,"completion_tokens":3}}` + "\n\n",
	)
	acc.Feed(chunk)

	want := Usage{InputTokens: 7, OutputTokens: 3}
	got, ok := acc.Result()
	if !ok {
		t.Fatalf("应能跨注释/event 行后仍解析出 usage")
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// TestSplitOpenAISSEChunks 直接覆盖按 \n\n 分块 + 提取 data 行的工具函数，
// 防止 pumpUpstream 拼包后 split 行为变化而引入回归。
func TestSplitOpenAISSEChunks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "单 chunk + DONE",
			in:   `data: {"choices":[]}` + "\n\n" + `data: [DONE]` + "\n\n",
			want: []string{`{"choices":[]}`, `[DONE]`},
		},
		{
			name: "三 chunk 拼一块（含 usage）",
			in: `data: {"choices":[{"delta":{}}]}` + "\n\n" +
				`data: {"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
			want: []string{
				`{"choices":[{"delta":{}}]}`,
				`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
				`[DONE]`,
			},
		},
		{
			name: "块内仅 event: 行 → 跳过",
			in:   `event: ping` + "\n" + `id: 1` + "\n\n" + `data: {"usage":{"prompt_tokens":4}}` + "\n\n",
			want: []string{`{"usage":{"prompt_tokens":4}}`},
		},
		{
			name: "空输入",
			in:   "",
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, b := range splitOpenAISSEChunks([]byte(c.in)) {
				got = append(got, string(b))
			}
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d (%v vs %v)", len(got), len(c.want), got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// JSON 编解码小工具：测试用例里手动写 JSON 字符串易出错，这里只用来做对照。
var _ = json.Marshal
