// Package presets 提供国内常用大模型供应商的预设模板。
//
// 预设只包含「名称、官网、OpenAI Base URL」三个会在创建时直接预填到表单的字段；
// 不带 API Key、不带模型候选（这些由用户继续走原有流程：填 Key、点「拉取模型」）。
//
// 优先级：
//  1. 用户自定义覆盖文件（docs/provider-presets.json，由运行参数 --presets 指定）
//  2. 编译时嵌入的内置预设（internal/presets/builtin.json via go:embed）
//
// 这样新版本发布时升级预设，而用户在不动二进制的情况下也能覆盖。
package presets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Preset 是一个供应商模板：只包含用户必填的部分。
//
// 字段名与 config.Provider 对齐：保存时由 api 层组装成完整的 Provider 记录
// （写入 ID/TimeoutSeconds/Enabled 等默认值）。
type Preset struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OpenAIBaseURL string `json:"openai_base_url"`
	WebsiteURL    string `json:"website_url"`
}

// File 是预设 JSON 文件的顶层结构。
type File struct {
	Version int      `json:"version"`
	Presets []Preset `json:"presets"`
}

// ErrEmpty 表示预设文件不含任何供应商。
var ErrEmpty = errors.New("预设文件不含任何供应商")

// builtinJSON 是编译期嵌入的预设文件（与仓库 docs/provider-presets.json 同步维护）。
//
// 嵌入保证：即使 docs 目录被裁剪，预设接口也能响应。
var builtinJSON []byte

//go:embed builtin.json
var builtinEmbed string

// builtinFingerprint 是 builtinJSON 的 sha256，用于「与本地覆盖文件不同」时给出提示。
var builtinFingerprint string

func init() {
	builtinJSON = []byte(builtinEmbed)
	sum := sha256.Sum256(builtinJSON)
	builtinFingerprint = hex.EncodeToString(sum[:8])
}

// Store 读取预设列表（先看用户覆盖文件，再回落到编译期嵌入版本）。
type Store struct {
	overridePath string
	logger       func(string, ...any)
	mu           sync.Mutex
	cached       *File
	cachedPath   string
}

// Options 控制 Store 行为。
type Options struct {
	// OverridePath 是用户自定义预设文件路径；空字符串表示只用内置版本。
	OverridePath string
	// Logger 是非致命事件的日志回调（可为 nil）。
	Logger func(msg string, args ...any)
}

// New 创建预设 Store。
func New(opts Options) *Store {
	return &Store{
		overridePath: strings.TrimSpace(opts.OverridePath),
		logger:       opts.Logger,
	}
}

// Load 返回当前预设列表；命中用户版本则提示其哈希与内置版本不一致（仅日志，不报错）。
func (s *Store) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil {
		return s.cached, nil
	}

	var (
		raw      []byte
		source   string
		fromPath bool
	)
	if s.overridePath != "" {
		data, err := os.ReadFile(s.overridePath)
		if err == nil {
			raw = data
			source = s.overridePath
			fromPath = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("读取自定义预设 %s 失败: %w", s.overridePath, err)
		}
		// 文件不存在：静默回落，不打扰用户
	}
	if raw == nil {
		raw = builtinJSON
		source = "内置"
	}

	var f File
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("解析预设 JSON 失败: %w", err)
	}
	if len(f.Presets) == 0 {
		return nil, ErrEmpty
	}
	// 兜底：忽略大小写去重 + 规整字段
	seen := make(map[string]bool, len(f.Presets))
	cleaned := make([]Preset, 0, len(f.Presets))
	for i := range f.Presets {
		p := f.Presets[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		p.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
		p.WebsiteURL = strings.TrimSpace(p.WebsiteURL)
		if p.ID == "" {
			if s.logger != nil {
				s.logger("忽略缺少 id 的预设", "index", i)
			}
			continue
		}
		key := strings.ToLower(p.ID)
		if seen[key] {
			if s.logger != nil {
				s.logger("忽略重复 id 的预设", "id", p.ID)
			}
			continue
		}
		seen[key] = true
		if p.Name == "" {
			p.Name = p.ID
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return nil, ErrEmpty
	}
	f.Presets = cleaned

	if fromPath {
		// 让用户知道覆盖文件与内置版本不一致：可能需要升级
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:8]) != builtinFingerprint && s.logger != nil {
			s.logger("自定义预设文件与内置版本不一致，将按自定义版本生效",
				"path", source, "local_prefix", hex.EncodeToString(sum[:8]),
				"builtin_prefix", builtinFingerprint)
		}
	}

	s.cached = &f
	s.cachedPath = source
	return s.cached, nil
}

// Source 返回当前生效的预设来源（"内置" 或文件路径）；未 Load 过时返回空串。
func (s *Store) Source() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cachedPath
}

// BuiltinFingerprint 返回编译期内置预设的指纹前缀，便于控制台展示「与你的覆盖文件是否一致」。
func BuiltinFingerprint() string { return builtinFingerprint }