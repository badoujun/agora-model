package gateway

import (
	"context"
	"testing"
	"time"

	"agora-model/internal/config"
)

// touchRecorder 记录 TouchGatewayKey 收到了哪些 Key id。
type touchRecorder struct {
	ch chan string
}

func (t *touchRecorder) VerifyGatewayKey(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (t *touchRecorder) TouchGatewayKey(_ context.Context, id string) error {
	select {
	case t.ch <- id:
	default:
	}
	return nil
}

// TestTouchKeyThrottlesPerGatewayKey 覆盖按 Key 独立节流：
// 同一分钟内多个不同的网关 Key 都应各自触发一次 last_used_at 更新；
// 而同一个 Key 的重复请求仍被节流（只写一次）。旧的全局节流下，
// 第二个 Key 会被第一个占掉的窗口吞掉、永远不更新。
func TestTouchKeyThrottlesPerGatewayKey(t *testing.T) {
	rec := &touchRecorder{ch: make(chan string, 8)}
	upstream := providerFor("p", "http://127.0.0.1:1", "m")
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: []config.Provider{upstream},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := New(config.NewHolder(snap), testLogger()).WithKeyStore(rec)

	g.touchKey("gk_a")
	g.touchKey("gk_a") // 同一 Key 的第二次应被节流
	g.touchKey("gk_b") // 不同的 Key 必须独立计时
	g.touchKey("")

	seen := map[string]int{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-rec.ch:
			seen[id]++
		case <-deadline:
			t.Fatalf("超时：期望 gk_a 与 gk_b 各触发一次，实际 %v", seen)
		}
	}
	// 给重复的 gk_a 一点时间，确认它没有被再次写入
	time.Sleep(100 * time.Millisecond)
	if seen["gk_a"] != 1 {
		t.Errorf("同一个 Key 的重复调用应被节流，写入次数 = %d", seen["gk_a"])
	}
	if seen["gk_b"] != 1 {
		t.Errorf("不同 Key 应各自更新，gk_b 写入次数 = %d", seen["gk_b"])
	}
	if _, ok := seen[""]; ok {
		t.Error("空 Key id 不应触发更新")
	}
}
