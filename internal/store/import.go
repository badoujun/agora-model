package store

import (
	"context"
	"fmt"

	"agora-model/internal/config"
)

// ImportProviders 把引导配置（JSON）中的供应商导入数据库。
//
// 仅在库中没有任何供应商时使用（首次启动 bootstrap）：之后以数据库为准，
// 避免每次启动都用 JSON 覆盖 Web UI 的修改。
func (s *Store) ImportProviders(ctx context.Context, master []byte, providers []config.Provider) (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("存储未初始化")
	}
	n := 0
	for _, p := range providers {
		p := p
		if _, err := s.SaveProvider(ctx, master, p); err != nil {
			return n, fmt.Errorf("导入供应商 %s 失败: %w", p.ID, err)
		}
		n++
	}
	return n, nil
}
