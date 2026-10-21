package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGatewayKeyNameAndNoteLimits 覆盖别名/备注的边界校验：
// 别名必填、别名 ≤64 rune、备注 ≤256 rune（按 rune 而非字节计）。
func TestGatewayKeyNameAndNoteLimits(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.CreateGatewayKey(ctx, master, "   ", "", true); err == nil {
		t.Fatal("空别名应被拒绝")
	}
	if _, _, err := st.CreateGatewayKey(ctx, master, strings.Repeat("名", MaxGatewayKeyNameLen+1), "", true); err == nil {
		t.Fatal("超长别名应被拒绝")
	}
	if _, _, err := st.CreateGatewayKey(ctx, master, "ok", strings.Repeat("备", MaxGatewayKeyNoteLen+1), true); err == nil {
		t.Fatal("超长备注应被拒绝")
	}

	// 边界值应当被接受（rune 计数）
	name := strings.Repeat("名", MaxGatewayKeyNameLen)
	note := strings.Repeat("备", MaxGatewayKeyNoteLen)
	info, _, err := st.CreateGatewayKey(ctx, master, name, note, true)
	if err != nil {
		t.Fatalf("边界长度应被接受: %v", err)
	}
	if info.Name != name || info.Note != note {
		t.Errorf("别名/备注未原样保存")
	}

	// 更新走同一套校验
	if _, err := st.UpdateGatewayKey(ctx, info.ID, "", "", true); err == nil {
		t.Fatal("更新为空别名应被拒绝")
	}
}

// TestVerifyGatewayKeyExcludesDeleted 覆盖软删除后的鉴权行为：
// 已删除的 Key 即便 enabled 仍被写成 1 也不能通过校验。
func TestVerifyGatewayKeyExcludesDeleted(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	key, plain, err := st.CreateGatewayKey(ctx, master, "将删除", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, plain); !ok {
		t.Fatal("删除前应可通过鉴权")
	}

	// 构造「enabled=1 但已软删除」的异常状态，确认查询本身排除了它
	if _, err := st.db.ExecContext(ctx,
		`UPDATE gateway_keys SET deleted_at = '2024-03-01T00:00:00Z' WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.VerifyGatewayKey(ctx, plain); err != nil || ok {
		t.Fatalf("已软删除的 Key 不应通过鉴权: ok=%v err=%v", ok, err)
	}
}

// TestDeleteAndDisableProtectLastEnabledKey 覆盖「最后一个启用 Key」保护：
// 409 语义由 API 层映射，store 层统一返回 ErrLastGatewayKey。
func TestDeleteAndDisableProtectLastEnabledKey(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	only, _, err := st.CreateGatewayKey(ctx, master, "唯一", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateGatewayKey(ctx, only.ID, only.Name, only.Note, false); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("停用唯一启用 Key 应返回 ErrLastGatewayKey，得到 %v", err)
	}
	if err := st.DeleteGatewayKey(ctx, only.ID); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("删除唯一启用 Key 应返回 ErrLastGatewayKey，得到 %v", err)
	}

	// 建第二个之后，第一个就可以停用了
	second, _, err := st.CreateGatewayKey(ctx, master, "第二个", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateGatewayKey(ctx, only.ID, only.Name, only.Note, false); err != nil {
		t.Fatalf("非最后一个应允许停用: %v", err)
	}
	// 此时只剩 second 启用，删除 only 仍允许（它本来就已停用）
	if err := st.DeleteGatewayKey(ctx, only.ID); err != nil {
		t.Fatalf("停用后的 Key 应允许删除: %v", err)
	}
	if _, err := st.UpdateGatewayKey(ctx, second.ID, second.Name, second.Note, false); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("此时 second 是最后一个启用 Key，停用应被拒绝，得到 %v", err)
	}
}
