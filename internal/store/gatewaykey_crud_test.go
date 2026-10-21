package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGatewayKeyCRUDMulti 覆盖多 Key 的创建 / 改名 / 启停 / 软删除 / 按 id 查看与重置。
func TestGatewayKeyCRUDMulti(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	first, firstPlain, err := st.CreateGatewayKey(ctx, master, "Claude Code", "张三的笔记本", true)
	if err != nil {
		t.Fatalf("CreateGatewayKey: %v", err)
	}
	if !strings.HasPrefix(firstPlain, GatewayKeyPrefix) || !first.Enabled || !first.Revealable {
		t.Fatalf("新建结果异常: %+v plain=%q", first, firstPlain)
	}

	second, secondPlain, err := st.CreateGatewayKey(ctx, master, "Cursor", "", true)
	if err != nil {
		t.Fatalf("CreateGatewayKey: %v", err)
	}
	if second.ID == first.ID || secondPlain == firstPlain {
		t.Fatal("两次创建应得到不同的 Key")
	}

	// 两个 Key 都能通过校验（多 Key 同时启用）
	for _, plain := range []string{firstPlain, secondPlain} {
		if id, ok, err := st.VerifyGatewayKey(ctx, plain); err != nil || !ok || id == "" {
			t.Fatalf("Key %q 应可鉴权: id=%q ok=%v err=%v", plain, id, ok, err)
		}
	}

	// 默认 Key = 最早创建者
	active, err := st.ActiveGatewayKey(ctx)
	if err != nil || active.ID != first.ID {
		t.Fatalf("默认 Key 应为最早创建者: %+v err=%v", active, err)
	}

	// 按 id 查看明文
	revealed, err := st.RevealGatewayKeyByID(ctx, master, second.ID)
	if err != nil || revealed != secondPlain {
		t.Fatalf("RevealGatewayKeyByID = %q err=%v", revealed, err)
	}

	// 改名 + 备注
	renamed, err := st.UpdateGatewayKey(ctx, second.ID, "Cursor-桌面", "李四", true)
	if err != nil {
		t.Fatalf("UpdateGatewayKey: %v", err)
	}
	if renamed.Name != "Cursor-桌面" || renamed.Note != "李四" {
		t.Fatalf("改名结果 = %+v", renamed)
	}

	// 停用第二个：仍有一个启用，允许
	disabled, err := st.UpdateGatewayKey(ctx, second.ID, renamed.Name, renamed.Note, false)
	if err != nil {
		t.Fatalf("停用非最后一个 Key 应成功: %v", err)
	}
	if disabled.Enabled {
		t.Fatal("停用后应为禁用状态")
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, secondPlain); ok {
		t.Fatal("停用的 Key 不应再通过鉴权")
	}

	// 停用最后一个启用 Key：拒绝
	if _, err := st.UpdateGatewayKey(ctx, first.ID, first.Name, first.Note, false); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("停用最后一个启用 Key 应被拒绝，得到 %v", err)
	}

	// 重置：id/别名/备注保留，明文换新，旧明文失效
	newPlain, err := st.ResetGatewayKeyByID(ctx, master, first.ID)
	if err != nil {
		t.Fatalf("ResetGatewayKeyByID: %v", err)
	}
	if newPlain == firstPlain {
		t.Fatal("重置后明文应不同")
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, firstPlain); ok {
		t.Fatal("重置后旧明文必须失效")
	}
	if id, ok, _ := st.VerifyGatewayKey(ctx, newPlain); !ok || id != first.ID {
		t.Fatalf("重置后新明文应可用且指向同一 id: %q ok=%v", id, ok)
	}
	afterReset, err := st.GetGatewayKey(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterReset.Name != first.Name || afterReset.Note != first.Note {
		t.Errorf("重置不应改动别名/备注: %+v", afterReset)
	}

	// 启用第二个后删除它（软删除）
	if _, err := st.UpdateGatewayKey(ctx, second.ID, renamed.Name, renamed.Note, true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteGatewayKey(ctx, second.ID); err != nil {
		t.Fatalf("DeleteGatewayKey: %v", err)
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, secondPlain); ok {
		t.Fatal("软删除的 Key 不应再通过鉴权")
	}
	gone, err := st.GetGatewayKey(ctx, second.ID)
	if err != nil {
		t.Fatalf("软删除记录仍应可按 id 查到（供历史展示）: %v", err)
	}
	if gone.DeletedAt.IsZero() {
		t.Error("软删除记录应带 DeletedAt")
	}
	if _, err := st.RevealGatewayKeyByID(ctx, master, second.ID); err != nil {
		t.Errorf("软删除记录仍应可查看明文（历史排障用）: %v", err)
	}
	if _, err := st.ResetGatewayKeyByID(ctx, master, second.ID); !errors.Is(err, ErrGatewayKeyNotFound) {
		t.Errorf("已删除的 Key 不应能重置，得到 %v", err)
	}

	// 删除最后一个启用 Key：拒绝
	if err := st.DeleteGatewayKey(ctx, first.ID); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("删除最后一个启用 Key 应被拒绝，得到 %v", err)
	}

	// 名称会拼进展示名（别名 + 备注），供日志/下拉使用
	names, err := st.GatewayKeyNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names[first.ID] != "Claude Code（张三的笔记本）" {
		t.Errorf("带备注的展示名 = %q", names[first.ID])
	}
	if names[second.ID] != "Cursor-桌面（李四）" {
		t.Errorf("带备注的展示名 = %q", names[second.ID])
	}
}

// TestCreateGatewayKeyDisabledWithoutEnabled 覆盖「不能建出一个全部禁用的库」：
// 库里没有启用 Key 时，创建时就禁用的 Key 会被拒绝。
func TestCreateGatewayKeyDisabledWithoutEnabled(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.CreateGatewayKey(ctx, master, "仅禁用", "", false); !errors.Is(err, ErrLastGatewayKey) {
		t.Fatalf("无启用 Key 时创建禁用 Key 应被拒绝，得到 %v", err)
	}

	// 先建一个启用的，再建一个禁用的即可
	if _, _, err := st.CreateGatewayKey(ctx, master, "启用", "", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateGatewayKey(ctx, master, "禁用", "", false); err != nil {
		t.Fatalf("已有启用 Key 时允许创建禁用 Key: %v", err)
	}
}

// TestGatewayKeyScopesTiedToKey 覆盖 scope 与 Key 的配合：按 id 读写，
// 并验证 ResetGatewayKeyByID 不会清掉模型范围。
func TestGatewayKeyScopesTiedToKey(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	key, _, err := st.CreateGatewayKey(ctx, master, "受限", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetGatewayKeyScopes(ctx, key.ID, []GatewayKeyScopeRow{{ProviderID: "p1", Model: "gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListGatewayKeyScopesFor(ctx, key.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListGatewayKeyScopesFor = %v err=%v", rows, err)
	}

	if _, err := st.ResetGatewayKeyByID(ctx, master, key.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListGatewayKeyScopesFor(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ProviderID != "p1" {
		t.Errorf("重置明文不应清掉模型范围: %v", rows)
	}
}
