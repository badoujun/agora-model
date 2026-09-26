package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"agora-model/internal/config"
)

// DefaultRefreshInterval 是模型聚合的默认刷新间隔。
const DefaultRefreshInterval = 10 * time.Minute

// DefaultFetchTimeout 是单次拉取的超时。
const DefaultFetchTimeout = 20 * time.Second

// SettingRefreshSeconds 是刷新间隔（秒）的设置键。
const SettingRefreshSeconds = "model_refresh_seconds"

// Store 是聚合器需要的持久化能力（由 store 实现，避免包间循环依赖）。
type Store interface {
	LoadProviders(ctx context.Context, master []byte) ([]config.Provider, error)
	ReplaceModelCache(ctx context.Context, providerID string, models []string, fetchedAt time.Time) error
	SetProviderFetchError(ctx context.Context, providerID, message string) error
	GetSetting(ctx context.Context, key string) (string, bool, error)
}

// Options 配置聚合器。
type Options struct {
	// Master 是主密钥，用于解密供应商凭证。
	Master []byte
	// DefaultInterval 覆盖默认刷新间隔。
	DefaultInterval time.Duration
	// Timeout 覆盖单次拉取超时。
	Timeout time.Duration
	// OnRefresh 在每轮刷新结束后调用（用于重建配置快照）。
	OnRefresh func(context.Context)
}

// Aggregator 定时从各供应商的 OpenAI 兼容端点拉取可用模型列表。
//
// 失败隔离：单个供应商失败只记录 last_fetch_error 并保留旧缓存，不影响其他供应商（DESIGN §7.5）。
type Aggregator struct {
	store  Store
	logger *slog.Logger
	client *http.Client
	opts   Options
	// writeMu 串行化 model_cache 写入：modernc 驱动在极端并发写下可能报 locked，
	// busy_timeout 之外再加一层保险。
	writeMu sync.Mutex
}

// NewAggregator 创建聚合器。
func NewAggregator(st Store, logger *slog.Logger, opts Options) *Aggregator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Aggregator{
		store:  st,
		logger: logger,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				DisableCompression:  true,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		opts: opts,
	}
}

// Start 立即刷新一次，随后按间隔循环；ctx 取消即停止。
func (a *Aggregator) Start(ctx context.Context) {
	a.RefreshAll(ctx)
	go func() {
		interval := a.interval(ctx)
		a.logger.Info("模型聚合已启动", "interval", interval.String())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.RefreshAll(ctx)
			}
		}
	}()
}

// RefreshAll 并发刷新所有启用且允许自动拉取的供应商。
func (a *Aggregator) RefreshAll(ctx context.Context) {
	providers, err := a.store.LoadProviders(ctx, a.opts.Master)
	if err != nil {
		a.logger.Warn("模型聚合：加载供应商失败", "err", err)
		return
	}

	var (
		wg      sync.WaitGroup
		changed int
		mu      sync.Mutex
	)
	for _, p := range providers {
		if !p.IsEnabled() || !p.FetchModels() {
			continue
		}
		wg.Add(1)
		go func(p config.Provider) {
			defer wg.Done()
			if err := a.RefreshProvider(ctx, p); err != nil {
				a.logger.Warn("模型聚合：拉取失败（保留旧缓存）", "provider", p.ID, "err", err)
				return
			}
			mu.Lock()
			changed++
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if changed > 0 && a.opts.OnRefresh != nil {
		a.opts.OnRefresh(ctx)
	}
}

// RefreshProvider 拉取单个供应商的模型列表并写入缓存。
func (a *Aggregator) RefreshProvider(ctx context.Context, p config.Provider) error {
	if !p.FetchModels() {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	if base == "" {
		return a.markFailed(ctx, p.ID, errors.New("未配置 openai_base_url，无法拉取模型列表"))
	}

	reqCtx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return a.markFailed(ctx, p.ID, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	for k, v := range p.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return a.markFailed(ctx, p.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return a.markFailed(ctx, p.ID,
			fmt.Errorf("上游 %s 返回 %d: %s", base+"/models", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	ids, err := parseModelIDs(resp.Body)
	if err != nil {
		return a.markFailed(ctx, p.ID, err)
	}

	a.writeMu.Lock()
	err = a.store.ReplaceModelCache(ctx, p.ID, ids, time.Now().UTC())
	a.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("写入模型缓存失败: %w", err)
	}
	a.logger.Info("模型聚合完成", "provider", p.ID, "models", len(ids))
	return nil
}

func (a *Aggregator) markFailed(ctx context.Context, providerID string, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.store.SetProviderFetchError(writeCtx, providerID, cause.Error()); err != nil {
		a.logger.Debug("记录拉取失败原因失败", "provider", providerID, "err", err)
	}
	return cause
}

func (a *Aggregator) interval(ctx context.Context) time.Duration {
	if a.opts.DefaultInterval > 0 {
		return a.opts.DefaultInterval
	}
	if raw, ok, err := a.store.GetSetting(ctx, SettingRefreshSeconds); err == nil && ok {
		if seconds, perr := time.ParseDuration(raw + "s"); perr == nil && seconds > 0 {
			return seconds
		}
	}
	return DefaultRefreshInterval
}

func (a *Aggregator) timeout() time.Duration {
	if a.opts.Timeout > 0 {
		return a.opts.Timeout
	}
	return DefaultFetchTimeout
}

// parseModelIDs 兼容三种常见返回：
//
//	{"object":"list","data":[{"id":"gpt-4o"}]}
//	{"data":["gpt-4o"]}
//	["gpt-4o"]
func parseModelIDs(r io.Reader) ([]string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	var objectForm struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &objectForm); err == nil && len(objectForm.Data) > 0 {
		return collectIDs(objectForm.Data), nil
	}

	var arrayForm []json.RawMessage
	if err := json.Unmarshal(raw, &arrayForm); err == nil {
		ids := collectIDs(arrayForm)
		if len(ids) == 0 {
			return nil, errors.New("上游 /models 返回的模型列表为空")
		}
		return ids, nil
	}
	return nil, errors.New("无法解析上游 /models 响应")
}

func collectIDs(items []json.RawMessage) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		var asObject struct {
			ID string `json:"id"`
		}
		var id string
		if err := json.Unmarshal(item, &asObject); err == nil && asObject.ID != "" {
			id = asObject.ID
		} else {
			var asString string
			if err := json.Unmarshal(item, &asString); err == nil {
				id = asString
			}
		}
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Available 计算供应商对外的可用模型：(手动 ∪ 自动) − 排除。
//
// 手动列表在前（保持人工顺序），自动拉取的模型按字典序追加，避免每次刷新顺序抖动。
func Available(manual, cached, excluded []string) []string {
	skip := make(map[string]bool, len(excluded))
	for _, m := range excluded {
		if m = strings.TrimSpace(m); m != "" {
			skip[m] = true
		}
	}

	seen := make(map[string]bool, len(manual)+len(cached))
	out := make([]string, 0, len(manual)+len(cached))

	appendModel := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || skip[m] || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}

	for _, m := range manual {
		appendModel(m)
	}
	sorted := append([]string(nil), cached...)
	sort.Strings(sorted)
	for _, m := range sorted {
		appendModel(m)
	}
	return out
}
