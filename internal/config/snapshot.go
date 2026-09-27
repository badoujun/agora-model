package config

import (
	"sort"
	"sync/atomic"
)

// Snapshot 是不可变的运行时配置视图。
//
// 请求路径只读取快照，不持有锁；配置变更时整体替换（见 Holder），
// 正在处理的请求继续使用旧快照，不会读到半更新状态。
type Snapshot struct {
	gateway Gateway
	byID    map[string]*Provider
	// byName 以归一化名称（去空白 + 忽略大小写）为键，供命名空间路由使用。
	byName  map[string]*Provider
	ordered []*Provider // 仅启用项，按名称升序
}

// NewSnapshot 由配置构建快照（会执行规范化与校验）。
//
// 允许没有任何启用的供应商：全新安装时数据库为空是合法初始状态——
// 用户必须先能启动网关，才能通过 Web UI 添加第一个供应商。
// （引导配置的严格校验在 Normalize / LoadFile 里，不受影响。）
func NewSnapshot(f File) (*Snapshot, error) {
	normalized, err := f.NormalizeAllowEmpty()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		gateway: normalized.Gateway,
		byID:    make(map[string]*Provider, len(normalized.Providers)),
		byName:  make(map[string]*Provider, len(normalized.Providers)),
	}
	for i := range normalized.Providers {
		p := normalized.Providers[i]
		if _, dup := s.byID[p.ID]; dup {
			continue // Normalize 已保证不重复，这里仅防御
		}
		s.byID[p.ID] = &p
		s.byName[normalizeName(p.Name)] = &p
		if p.IsEnabled() {
			s.ordered = append(s.ordered, &p)
		}
	}
	// 默认路由顺序：按供应商名称升序（同名已被 Normalize 拒绝）
	sort.SliceStable(s.ordered, func(i, j int) bool {
		return s.ordered[i].Name < s.ordered[j].Name
	})
	return s, nil
}

// Gateway 返回网关设置。
func (s *Snapshot) Gateway() Gateway { return s.gateway }

// Provider 按 id 查找供应商（含已停用项）。
func (s *Snapshot) Provider(id string) (*Provider, bool) {
	p, ok := s.byID[id]
	return p, ok
}

// ProviderByName 按供应商名称查找（去空白 + 忽略大小写），含已停用项。
func (s *Snapshot) ProviderByName(name string) (*Provider, bool) {
	p, ok := s.byName[normalizeName(name)]
	return p, ok
}

// HasProvider 报告 id 是否存在（含已停用项）。
func (s *Snapshot) HasProvider(id string) bool {
	_, ok := s.byID[id]
	return ok
}

// Providers 返回启用的供应商，按名称升序（即默认路由优先级）。
func (s *Snapshot) Providers() []*Provider { return s.ordered }

// Holder 以原子方式持有当前配置快照，读取无锁。
type Holder struct {
	ptr atomic.Pointer[Snapshot]
}

// NewHolder 创建持有指定快照的 Holder。
func NewHolder(s *Snapshot) *Holder {
	h := &Holder{}
	h.ptr.Store(s)
	return h
}

// Get 返回当前快照（永不为 nil，前提是构造时传入了非 nil 快照）。
func (h *Holder) Get() *Snapshot { return h.ptr.Load() }

// Store 原子替换当前快照，新的请求立即生效。
func (h *Holder) Store(s *Snapshot) { h.ptr.Store(s) }
