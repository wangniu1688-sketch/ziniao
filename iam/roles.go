package iam

import (
	"context"
	"time"

	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// RoleSpec 内置角色定义（M12：老板 / 运营主管 / 运营 / 客服 / 财务 + 自定义）。
type RoleSpec struct {
	Code        string
	Name        string
	Permissions []string
	MaskFields  []string // 字段级遮蔽（M13：财务角色看不到利润明文）
	DenyElement []string // 元素级屏蔽（M14 事前拦截）
	DataScope   string   // none|own|team|all
}

// BuiltinRoles 系统内置角色（TenantID 为空 = 系统级，对所有租户可见）。
func BuiltinRoles() []RoleSpec {
	return []RoleSpec{
		{
			Code: "owner", Name: "老板",
			Permissions: AllKeys(),
			DataScope:   "all",
		},
		{
			Code: "ops_manager", Name: "运营主管",
			Permissions: []string{
				"shop:create", "shop:read", "shop:update", "shop:delete", "shop:transition",
				"member:invite", "member:list", "audit:read",
				"device:read", "device:purchase", "device:import", "device:bind", "device:renew",
				"env:create", "env:check", "env:launch", "shop:credential.manage",
				"security:policy.read", "security:policy.manage", "security:event.read",
				"approval:create", "approval:read", "approval:approve", "job:read", "job:run",
				"page:shops", "page:audit", "page:team", "page:devices", "page:security", "page:approvals",
				"element:shop.delete_button", "element:shop.export_button", "element:device.purchase_button",
				"data:shop", "data:device", "resource:device.purchase",
			},
			DataScope: "all",
		},
		{
			Code: "operator", Name: "运营",
			Permissions: []string{
				"shop:create", "shop:read", "shop:update", "shop:transition",
				"device:read", "env:create", "env:check", "env:launch",
				"approval:create", "approval:read", "approval:approve", "job:read",
				"page:shops", "page:devices", "page:approvals", "data:shop", "data:device",
			},
			DataScope: "own", // 只看被授权的店铺
		},
		{
			Code: "cs", Name: "客服",
			Permissions: []string{"shop:read", "page:shops", "data:shop"},
			DataScope:   "own",
		},
		{
			Code: "finance", Name: "财务",
			// 财务能看财务页，但利润/成本/余额在 DOM 层替换为掩码（M13.2）
			Permissions: []string{"finance:read", "shop:read", "page:finance", "page:shops", "data:shop"},
			MaskFields:  []string{"profit", "cost", "balance"},
			DenyElement: []string{"element:finance.export_button", "element:finance.profit_field"},
			DataScope:   "all",
		},
	}
}

// EnsureBuiltinRoles 幂等写入系统内置角色（启动时调用）。
func EnsureBuiltinRoles(ctx context.Context, st store.Store) error {
	for _, spec := range BuiltinRoles() {
		existing, err := st.Roles().GetByCode(ctx, "", spec.Code)
		if err == nil && existing != nil {
			continue
		}
		role := &types.Role{
			ID:          ids.New(),
			Code:        spec.Code,
			Name:        spec.Name,
			Builtin:     true,
			Permissions: spec.Permissions,
			MaskFields:  spec.MaskFields,
			DenyElement: spec.DenyElement,
			DataScope:   spec.DataScope,
			CreatedAt:   time.Now().UTC(),
		}
		if err := st.Roles().Create(ctx, role); err != nil {
			return err
		}
	}
	return nil
}

// SpecOf 按 code 取内置角色定义（未命中返回 nil）。
func SpecOf(code string) *RoleSpec {
	for i := range BuiltinRoles() {
		if BuiltinRoles()[i].Code == code {
			spec := BuiltinRoles()[i]
			return &spec
		}
	}
	return nil
}
