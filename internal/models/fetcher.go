package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/config"
)

// DefaultFetchTimeout 是单次拉取的超时。
const DefaultFetchTimeout = 20 * time.Second

// Fetcher 按需从供应商的 OpenAI 兼容端点拉取模型候选列表。
//
// 只提供「拉取」能力：是否启用某个模型由供应商配置里的勾选决定（见 config.Provider.Models），
// 因此这里没有定时任务，也没有聚合/排除语义。
type Fetcher struct {
	client  *http.Client
	timeout time.Duration
}

// NewFetcher 创建模型拉取器。
func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				DisableCompression:  true,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		timeout: DefaultFetchTimeout,
	}
}

// Fetch 拉取单个供应商的模型列表（失败时不返回部分结果）。
func (f *Fetcher) Fetch(ctx context.Context, p config.Provider) ([]string, error) {
	endpoint := p.ModelsEndpoint()
	if endpoint == "" {
		return nil, errors.New("无法推导 /models 地址：请填写 openai_base_url（端点覆盖模式下不支持拉取模型）")
	}

	reqCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	for k, v := range p.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("上游 %s 返回 %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return parseModelIDs(resp.Body)
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
