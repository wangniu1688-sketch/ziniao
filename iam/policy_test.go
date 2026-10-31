package iam

import (
	"context"
	"testing"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/types"
)

func timeNow() time.Time { return time.Now() }

// roleSet 取内置角色定义并转成裁决用的 Role。
func roleSet(t *testing.T, codes ...string) []types.Role {
	t.Helper()
	out := []types.Role{}
	for _, c := range codes {
		spec := SpecOf(c)
		if spec == nil {
			t.Fatalf("内置角色缺失: %s", c)
		}
		out = append(out, types.Role{
			ID: c, Code: spec.Code, Name: spec.Name,
			Permissions: spec.Permissions, MaskFields: spec.MaskFields,
			DenyElement: spec.DenyElement, DataScope: spec.DataScope,
		})
	}
	return out
}

// TestPermissionMatrix 权限矩阵表驱动测试（角色 × 动作 × 页面 → 期望裁决）。
func TestPermissionMatrix(t *testing.T) {
	cfg := config.Default()
	engine := NewEngine(cfg)
	ctx := context.Background()

	cases := []struct {
		name        string
		roles       []string
		action      string
		path        string
		wantAllow   bool
		wantScope   string
		wantMask    []string
		wantDenyCnt int
	}{
		{name: "老板可建店", roles: []string{"owner"}, action: "shop:create", path: "/shops", wantAllow: true, wantScope: "all"},
		{name: "老板可删店", roles: []string{"owner"}, action: "shop:delete", path: "/shops", wantAllow: true, wantScope: "all"},
		// 运营可建店，但删除/导出按钮被元素级策略屏蔽（M14 事前拦截）
		{name: "运营可建店", roles: []string{"operator"}, action: "shop:create", path: "/shops",
			wantAllow: true, wantScope: "own", wantDenyCnt: 2},
		{name: "运营不可删店", roles: []string{"operator"}, action: "shop:delete", path: "/shops", wantAllow: false},
		{name: "客服只读", roles: []string{"cs"}, action: "shop:read", path: "/shops",
			wantAllow: true, wantScope: "own", wantDenyCnt: 2},
		{name: "客服不可建店", roles: []string{"cs"}, action: "shop:create", path: "/shops", wantAllow: false},
		{name: "财务可看财务页", roles: []string{"finance"}, action: "finance:read", path: "/finance", wantAllow: true, wantScope: "all",
			wantMask: []string{"balance", "cost", "profit"}, wantDenyCnt: 2},
		{name: "运营不可看财务页", roles: []string{"operator"}, action: "finance:read", path: "/finance", wantAllow: false},
		{name: "运营主管可看审计", roles: []string{"ops_manager"}, action: "audit:read", path: "/audit", wantAllow: true, wantScope: "all"},
		{name: "运营不可看审计", roles: []string{"operator"}, action: "audit:read", path: "/audit", wantAllow: false},
		{name: "多角色取并集：运营+财务可看财务", roles: []string{"operator", "finance"}, action: "finance:read", path: "/finance",
			wantAllow: true, wantScope: "all", wantMask: []string{"balance", "cost", "profit"}, wantDenyCnt: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Check(ctx, Request{
				Subject: Subject{Roles: roleSet(t, tc.roles...), Now: timeNow()},
				Action:  tc.action, Path: tc.path,
			})
			if d.Allow != tc.wantAllow {
				t.Fatalf("裁决不符: 期望 allow=%v，实际 %v（原因 %s）", tc.wantAllow, d.Allow, d.Reason)
			}
			if !tc.wantAllow {
				return
			}
			if tc.wantScope != "" && d.DataScope != tc.wantScope {
				t.Fatalf("数据范围不符: 期望 %s，实际 %s", tc.wantScope, d.DataScope)
			}
			if len(tc.wantMask) > 0 {
				if len(d.MaskFields) != len(tc.wantMask) {
					t.Fatalf("遮蔽字段不符: 期望 %v，实际 %v", tc.wantMask, d.MaskFields)
				}
				for i := range tc.wantMask {
					if d.MaskFields[i] != tc.wantMask[i] {
						t.Fatalf("遮蔽字段顺序不符: 期望 %v，实际 %v", tc.wantMask, d.MaskFields)
					}
				}
			}
			if len(d.DenyElements) != tc.wantDenyCnt {
				t.Fatalf("屏蔽元素数量不符: 期望 %d，实际 %d（%v）", tc.wantDenyCnt, len(d.DenyElements), d.DenyElements)
			}
		})
	}
}

// TestResourceLayerRequiresGrant 资源级权限：功能权限通过还不够，必须有资源授权。
func TestResourceLayerRequiresGrant(t *testing.T) {
	engine := NewEngine(config.Default())
	ctx := context.Background()

	withoutGrant := engine.Check(ctx, Request{
		Subject:      Subject{Roles: roleSet(t, "ops_manager"), Now: timeNow()},
		Action:       "shop:create",
		ResourceType: "box.purchase",
	})
	if withoutGrant.Allow {
		t.Fatal("缺少资源授权时应拒绝（第五层不能缺位）")
	}
	if withoutGrant.Layer != LayerResource {
		t.Fatalf("拒绝层级应为 resource，实际 %s", withoutGrant.Layer)
	}

	withGrant := engine.Check(ctx, Request{
		Subject: Subject{
			Roles:  roleSet(t, "ops_manager"),
			Grants: []types.DataGrant{{ResourceType: "box.purchase", ResourceID: "*"}},
			Now:    timeNow(),
		},
		Action:       "shop:create",
		ResourceType: "box.purchase",
	})
	if !withGrant.Allow {
		t.Fatalf("有资源授权时应放行，实际原因 %s", withGrant.Reason)
	}
}

// TestDOMPolicy 第三层（DOM 注入层）策略：字段遮蔽 + 元素屏蔽 + 水印。
func TestDOMPolicy(t *testing.T) {
	engine := NewEngine(config.Default())
	p := engine.DOMPolicy(context.Background(), Subject{
		Roles: roleSet(t, "finance"), Username: "finance", IP: "1.2.3.4", Now: timeNow(),
	}, "/finance")
	if len(p.MaskSelectors) != 3 {
		t.Fatalf("财务角色应有 3 个字段遮蔽选择器，实际 %v", p.MaskSelectors)
	}
	if len(p.DenySelectors) != 2 {
		t.Fatalf("财务角色应屏蔽 2 个元素，实际 %v", p.DenySelectors)
	}
	if p.Watermark == nil || !p.Watermark.Enabled {
		t.Fatal("默认应开启屏幕水印")
	}
}

// TestHighRiskRequiresObligation 高危动作必须带义务（审批 / MFA），不能只判 allow。
func TestHighRiskRequiresObligation(t *testing.T) {
	engine := NewEngine(config.Default())
	d := engine.Check(context.Background(), Request{
		Subject: Subject{Roles: roleSet(t, "owner"), Now: timeNow()},
		Action:  "audit:export", Path: "/audit",
	})
	if !d.Allow {
		t.Fatal("老板应可导出")
	}
	if !has(d.Obligations, "require_approval") {
		t.Fatalf("高危导出应触发二次审批义务，实际 %v", d.Obligations)
	}
}

func has(list []string, v string) bool {
	for _, it := range list {
		if it == v {
			return true
		}
	}
	return false
}
