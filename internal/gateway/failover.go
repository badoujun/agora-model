package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
	"agora-model/internal/route"
)

// observe 在"请求路径第一次看到新快照"时同步运行时状态。
//
// 出现在新快照里的 Key 一定是库中启用的（LoadProviders 只装载启用的 Key），
// 因此清掉它的"自动停用"标记——这就是"用户在界面上重新启用后立即生效"。
// 自动停用的 Key 不会出现在新快照里（库已 enabled=0），标记得以保留。
// 冷却时间不在此清除：与配置无关的变更触发快照重建时，不应顺手解除
// "最后一个可用 Key" 的冷却。
func (g *Gateway) observe(snap *config.Snapshot) {
	if snap == nil || g.lastSnap.Load() == snap {
		return
	}
	g.lastSnap.Store(snap)
	for _, p := range snap.Providers() {
		for _, k := range p.APIKeys {
			g.keyRuntime.forget(k)
		}
	}
}

// keyFailure 描述一次凭证失败的尝试。
//
// 它同时用于两处落地：进程日志（slog）与请求日志（logs.error_msg），
// 这样即使后续 Key 成功、客户端看到 200，失败信息也不会丢失。
type keyFailure struct {
	keyHint string // 脱敏后的 Key（sk-****abcd）
	status  int    // 上游 HTTP 状态码；连接失败时为 0
	reason  string // 已脱敏、截断的失败原因
	action  string // 网关采取的动作（已自动停用 / 已冷却未停用 / 已换下一个 Key）
}

func (f keyFailure) describe() string {
	reason := f.reason
	if reason == "" {
		reason = fmt.Sprintf("HTTP %d", f.status)
	}
	return fmt.Sprintf("Key %s: %s（%s）", f.keyHint, reason, f.action)
}

// describeKeyFailures 把多次失败拼成一条可落库的摘要（空切片返回空串）。
func describeKeyFailures(failures []keyFailure) string {
	if len(failures) == 0 {
		return ""
	}
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, f.describe())
	}
	return strings.Join(parts, "；")
}

// sendWithFailover 依次尝试供应商的可用 Key，返回最终响应、实际使用的 Key 与失败明细。
//
// 规则（DESIGN §7.10）：
//   - 健康 Key 优先，按轮询起点旋转；不足 maxKeyAttempts 个时用冷却中的 Key 补齐；
//   - 401/402/403/429/431：该 Key 立即自动停用（内存生效 + 异步落库），换下一个 Key；
//     该供应商已无可用 Key 时联动停用供应商（无冷却、无自动重启，恢复须人工）；
//   - 连接失败：换下一个 Key（不改变 Key 状态）；
//   - 客户端断开/整体超时：立即停止重试；
//   - 最后一次仍是凭证失败时，把该上游响应原样交给调用方透传。
//
// 返回的失败明细包含每一次失败的 Key 与原因（含最后一次），由调用方写入请求日志。
func (g *Gateway) sendWithFailover(ctx context.Context, in *http.Request, d route.Result, body []byte, requestID string) (*http.Response, config.ProviderKey, []keyFailure, error) {
	candidates := g.keyRuntime.order(d.Provider.ID, d.Provider.UsableKeys(), maxKeyAttempts)
	if len(candidates) == 0 {
		return nil, config.ProviderKey{}, nil, errNoUsableKey
	}

	var (
		lastErr  error
		failures []keyFailure
	)
	for i, key := range candidates {
		last := i == len(candidates)-1
		resp, err := g.attempt(ctx, in, d, body, key)
		if err != nil {
			// 客户端断开 / 整体超时：没有任何重试价值
			if ctx.Err() != nil || in.Context().Err() != nil {
				return nil, key, failures, err
			}
			lastErr = err
			action := "已换下一个 Key"
			if last {
				action = "无更多 Key 可试"
			}
			failures = append(failures, keyFailure{
				keyHint: crypto.Mask(key.Key),
				reason:  "连接失败: " + logging.Redact(err.Error(), 200),
				action:  action,
			})
			g.logger.Warn("上游连接失败，尝试下一个 API Key",
				"request_id", requestID, "provider", d.Provider.Name,
				"key", crypto.Mask(key.Key), "err", err, "last_attempt", last)
			continue
		}

		status := resp.StatusCode
		if !isKeyFailureStatus(status) {
			g.recordKeySuccess(key.ID)
			return resp, key, failures, nil
		}

		reason := fmt.Sprintf("HTTP %d", status)
		if !last {
			// 非最后一次：可以直接读掉错误体用于记录（客户端拿到的是后续 Key 的结果）
			reason = keyFailureReason(status, drainBody(resp))
		}

		// 凭证类失败一律自动停用，包括该供应商最后一个可用 Key：
		// Key 全部停用后供应商会被一并停用（落库阶段完成），恢复完全由人工触发。
		g.keyRuntime.markAutoDisabled(key)
		g.recordKeyFailure(d.Provider.ID, key.ID, reason)
		action := "已自动停用"
		if g.keyRuntime.exhausted(d.Provider.UsableKeys()) {
			action = "已自动停用（该供应商已无可用 Key，已联动停用供应商）"
		}
		failures = append(failures, keyFailure{
			keyHint: crypto.Mask(key.Key), status: status, reason: reason, action: action,
		})
		g.logger.Warn("供应商凭证失败",
			"request_id", requestID, "provider", d.Provider.Name,
			"key", crypto.Mask(key.Key), "status", status, "action", action,
			"reason", reason, "last_attempt", last)
		if last {
			// 最后一次：不消费 body，交给调用方原样透传（保持上游错误语义）
			return resp, key, failures, nil
		}
	}

	if lastErr != nil {
		return nil, config.ProviderKey{}, failures, lastErr
	}
	return nil, config.ProviderKey{}, failures, errNoUsableKey
}

// attempt 用指定 Key 构造并发送一次上游请求（body 可安全复用）。
func (g *Gateway) attempt(ctx context.Context, in *http.Request, d route.Result, body []byte, key config.ProviderKey) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = buildUpstreamHeaders(in.Header, d.Provider, key.Key)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.client.Do(req)
}

// isKeyFailureStatus 报告该状态码是否属于"凭证/配额类失败"（可切换 Key 重试）。
//
// 429 与 401/402/403 同等对待：该 Key 的额度/限速已不可用，继续轮询它没有意义，
// 因此同样自动停用（用户可在供应商页重新启用）。
//
// 431 也一并纳入：部分上游用 431 Request Header Fields Too Large 表示
// 账号被封停（响应体形如 {"error":{"message":"Account Suspension"}}），
// 本质上是该凭证不可再用，与 401/403 同级处理。
func isKeyFailureStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, // 401 凭证无效
		http.StatusPaymentRequired,             // 402 欠费
		http.StatusForbidden,                   // 403 无权限
		http.StatusTooManyRequests,             // 429 限速/配额耗尽
		http.StatusRequestHeaderFieldsTooLarge: // 431 账号封停（部分上游如此返回）
		return true
	}
	return false
}

// drainBody 读掉（最多 4KB）并关闭响应体，返回读到的前缀。
func drainBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return snippet
}

// keyFailureReason 生成可落库的失败原因（已脱敏、截断）。
func keyFailureReason(status int, body []byte) string {
	reason := fmt.Sprintf("HTTP %d", status)
	if len(body) == 0 {
		return reason
	}
	return fmt.Sprintf("HTTP %d: %s", status, logging.Redact(strings.TrimSpace(string(body)), 200))
}

func (g *Gateway) recordKeySuccess(keyID string) {
	if g.keyStats != nil {
		g.keyStats.TouchProviderKey(keyID)
	}
}

func (g *Gateway) recordKeyFailure(providerID, keyID, reason string) {
	if g.keyStats == nil || keyID == "" {
		return
	}
	g.keyStats.DisableProviderKey(providerID, keyID, reason)
}
