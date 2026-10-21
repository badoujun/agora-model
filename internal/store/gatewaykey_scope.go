package store

import (
	"context"
	"fmt"
	"strings"

	"agora-model/internal/config"
)

// GatewayKeyScopeRow 是 gateway_key_models 表的一行：某个网关 Key 被允许
// 调用的一个「供应商 × 上游模型」组合。
//
// Model 存的是**上游模型名**（provider.models_selected_json 里的原始名），
// 不是对外别名——别名可以在供应商页随时改，上游名才是稳定的路由目标，
// 而路由决策本身就是用上游名做的（见 route.ResolveScoped）。
type GatewayKeyScopeRow struct {
	ProviderID string
	Model      string
}

// ListGatewayKeyScopesFor 返回单个网关 Key 的模型范围（按 provider_id、model 排序）。
//
// 返回空切片表示该 Key 不限制模型范围（可调用全部模型）。
func (s *Store) ListGatewayKeyScopesFor(ctx context.Context, keyID string) ([]GatewayKeyScopeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_id, model FROM gateway_key_models
		 WHERE gateway_key_id = ? ORDER BY provider_id ASC, model ASC`, keyID)
	if err != nil {
		return nil, fmt.Errorf("查询网关 Key 模型范围失败: %w", err)
	}
	defer rows.Close()

	out := make([]GatewayKeyScopeRow, 0, 8)
	for rows.Next() {
		var row GatewayKeyScopeRow
		if err := rows.Scan(&row.ProviderID, &row.Model); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListAllGatewayKeyScopes 返回全部网关 Key 的模型范围，供配置快照一次性装载。
//
// 键是网关 Key id；没有行的 Key 不出现在返回值里（表示不限制）。
// 请求路径通过快照读取该映射，因此热路径不查数据库。
func (s *Store) ListAllGatewayKeyScopes(ctx context.Context) (map[string]config.ModelScope, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT gateway_key_id, provider_id, model FROM gateway_key_models`)
	if err != nil {
		return nil, fmt.Errorf("查询网关 Key 模型范围失败: %w", err)
	}
	defer rows.Close()

	out := map[string]config.ModelScope{}
	for rows.Next() {
		var keyID, providerID, model string
		if err := rows.Scan(&keyID, &providerID, &model); err != nil {
			return nil, err
		}
		if keyID == "" || providerID == "" || model == "" {
			continue
		}
		scope, ok := out[keyID]
		if !ok {
			scope = config.ModelScope{}
			out[keyID] = scope
		}
		models, ok := scope[providerID]
		if !ok {
			models = map[string]struct{}{}
			scope[providerID] = models
		}
		models[model] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SetGatewayKeyScopes 全量替换某个网关 Key 的模型范围（单事务）。
//
// 传空切片表示「不限制模型范围」（删除全部行）。空白项被丢弃并去重，
// 因此前端重复提交不会产生重复行。调用方负责校验 (provider_id, model)
// 是否仍然存在——store 层不持有主密钥，无法解开供应商配置。
func (s *Store) SetGatewayKeyScopes(ctx context.Context, keyID string, scopes []GatewayKeyScopeRow) error {
	if strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("网关 Key id 为空")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入网关 Key 模型范围开启事务失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_key_models WHERE gateway_key_id = ?`, keyID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("清理网关 Key 模型范围失败: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO gateway_key_models (gateway_key_id, provider_id, model) VALUES (?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备网关 Key 模型范围语句失败: %w", err)
	}
	defer stmt.Close()

	for _, row := range scopes {
		providerID := strings.TrimSpace(row.ProviderID)
		model := strings.TrimSpace(row.Model)
		if providerID == "" || model == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, keyID, providerID, model); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入网关 Key 模型范围失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交网关 Key 模型范围失败: %w", err)
	}
	return nil
}
