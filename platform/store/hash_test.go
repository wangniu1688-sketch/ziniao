package store_test

import (
	"testing"
	"time"

	"ziniao/platform/store"
	"ziniao/platform/types"
)

func entry(id, action, prev string) types.AuditEntry {
	return types.AuditEntry{
		ID: id, TenantID: "t1", Category: types.AuditCategoryOperation,
		ActorUserID: "u1", Action: action, Result: "success",
		OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), PrevHash: prev,
	}
}

// TestVerifyChain 哈希链：正常链应通过；篡改任一条应立刻发现。
func TestVerifyChain(t *testing.T) {
	e1 := entry("1", "shop.create", "")
	e1.Hash = store.AuditHash("", &e1)
	e2 := entry("2", "shop.delete", e1.Hash)
	e2.Hash = store.AuditHash(e1.Hash, &e2)
	e3 := entry("3", "auth.login", e2.Hash)
	e3.Hash = store.AuditHash(e2.Hash, &e3)

	if ok, at := store.VerifyChain([]types.AuditEntry{e1, e2, e3}); !ok || at != -1 {
		t.Fatalf("正常链应通过，实际 ok=%v brokenAt=%d", ok, at)
	}

	// 篡改中间一条的 action
	tampered := e2
	tampered.Action = "shop.delete_all"
	if ok, at := store.VerifyChain([]types.AuditEntry{e1, tampered, e3}); ok {
		t.Fatal("篡改后应校验失败")
	} else if at != 1 {
		t.Fatalf("应定位到被篡改的第 1 条，实际 %d", at)
	}

	// 删除中间一条（PrevHash 断裂）
	if ok, at := store.VerifyChain([]types.AuditEntry{e1, e3}); ok {
		t.Fatal("缺失记录应校验失败")
	} else if at != 1 {
		t.Fatalf("应定位到断裂位置，实际 %d", at)
	}
}

// TestAuditHashStable 相同输入必须得到相同哈希（保证可复核）。
func TestAuditHashStable(t *testing.T) {
	e := entry("1", "shop.create", "")
	h1 := store.AuditHash("", &e)
	h2 := store.AuditHash("", &e)
	if h1 != h2 {
		t.Fatal("哈希不稳定")
	}
	other := e
	other.Result = "failure"
	if store.AuditHash("", &other) == h1 {
		t.Fatal("不同内容不应产生相同哈希")
	}
}
