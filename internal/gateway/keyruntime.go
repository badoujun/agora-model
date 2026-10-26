package gateway

import (
	"sync"
	"sync/atomic"

	"agora-model/internal/config"
)

// maxKeyAttempts 是单个请求最多尝试的 API Key 数（含首次）。
const maxKeyAttempts = 3

// keyState 是单个供应商 Key 的运行时健康状态（全原子字段，热路径无锁）。
type keyState struct {
	autoDisabled atomic.Bool // 网关已判定为"自动停用"，内存中立即生效
}

// keyRuntime 是网关进程内的凭证运行时状态。
//
// 请求热路径（轮询起点 + 健康判定）只做 atomic/sync.Map 读，既不查库也不加锁；
// 数据库写入与快照重建由 store.ProviderKeyStatsRecorder 异步完成（§7.10）。
type keyRuntime struct {
	cursors sync.Map // providerID -> *atomic.Uint64
	states  sync.Map // keyID      -> *keyState
}

func newKeyRuntime() *keyRuntime { return &keyRuntime{} }

// next 返回某供应商的下一个轮询起点（从 0 开始单调递增）。
func (r *keyRuntime) next(providerID string) uint64 {
	if v, ok := r.cursors.Load(providerID); ok {
		return v.(*atomic.Uint64).Add(1) - 1
	}
	v, _ := r.cursors.LoadOrStore(providerID, new(atomic.Uint64))
	return v.(*atomic.Uint64).Add(1) - 1
}

// keyIdentity 返回运行时状态的键。
//
// 生产路径下 Key 一定有数据库 id；占位/草稿（未保存）的 Key 用 label+明文
// 兜底，避免所有空 id 的 Key 共用一份运行时状态。
func keyIdentity(k config.ProviderKey) string {
	if k.ID != "" {
		return k.ID
	}
	return "\x00" + k.Label + "\x00" + k.Key
}

func (r *keyRuntime) state(k config.ProviderKey) *keyState {
	id := keyIdentity(k)
	if v, ok := r.states.Load(id); ok {
		return v.(*keyState)
	}
	v, _ := r.states.LoadOrStore(id, &keyState{})
	return v.(*keyState)
}

// usable 报告某 Key 当前是否健康（未被自动停用）。
func (r *keyRuntime) usable(k config.ProviderKey) bool {
	v, ok := r.states.Load(keyIdentity(k))
	if !ok {
		return true
	}
	return !v.(*keyState).autoDisabled.Load()
}

// autoDisabled 报告某 Key 是否已被网关判定为自动停用。
func (r *keyRuntime) autoDisabled(k config.ProviderKey) bool {
	v, ok := r.states.Load(keyIdentity(k))
	if !ok {
		return false
	}
	return v.(*keyState).autoDisabled.Load()
}

func (r *keyRuntime) markAutoDisabled(k config.ProviderKey) {
	r.state(k).autoDisabled.Store(true)
}

// forget 清掉某个 Key 的"自动停用"标记。
//
// 调用时机：新快照里重新出现了该 Key（说明用户在界面上重新启用了它）。
func (r *keyRuntime) forget(k config.ProviderKey) {
	v, ok := r.states.Load(keyIdentity(k))
	if !ok {
		return
	}
	v.(*keyState).autoDisabled.Store(false)
}

// exhausted 报告该供应商是否已没有任何可用 Key（全部被自动停用）。
//
// keys 必须是当前快照里启用的 Key 集合（Provider.UsableKeys）。只要还有
// 一个未标记自动停用的 Key，就返回 false。
func (r *keyRuntime) exhausted(keys []config.ProviderKey) bool {
	for _, k := range keys {
		if r.usable(k) {
			return false
		}
	}
	return true
}

// order 返回本次请求的尝试顺序：未自动停用的 Key 按轮询起点旋转，
// 已"自动停用"的 Key 不会出现在结果里（它们已经确认不可用，重试只会白打上游）；
// 全部 Key 都被停用时返回空切片，由调用方回 503。
//
// 返回的切片长度不超过 limit（= maxKeyAttempts），每个请求一次小分配。
func (r *keyRuntime) order(providerID string, keys []config.ProviderKey, limit int) []config.ProviderKey {
	if len(keys) == 0 || limit <= 0 {
		return nil
	}
	start := int(r.next(providerID) % uint64(len(keys)))
	out := make([]config.ProviderKey, 0, limit)
	for offset := 0; offset < len(keys) && len(out) < limit; offset++ {
		k := keys[(start+offset)%len(keys)]
		if !r.usable(k) {
			continue
		}
		// 状态可能被并发修改，去重避免同一个 Key 被尝试两次
		id := keyIdentity(k)
		dup := false
		for i := 0; i < len(out); i++ {
			if keyIdentity(out[i]) == id {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, k)
	}
	return out
}
