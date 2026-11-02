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
				"device:read", "device:purchase", "device:import", "device:bind", "device:renew", "device:retire",
				"device:delete",
				"env:create", "env:check", "env:launch", "env:stop", "env:deactivate", "env:delete",
				// M2 启动器内核：分组 / 手工代理 / 环境聚合视图
				"group:read", "group:manage", "proxy:read", "proxy:manage", "env:read", "env:update",
				"shop:credential.manage",
				"egress:read", "approval:route.read", "approval:route.manage",
				"security:policy.read", "security:policy.manage", "security:event.read", "ca:manage",
				"approval:create", "approval:read", "approval:approve", "job:read", "job:run",
				"delegation:apply", "delegation:read", "delegation:approve", "delegation:revoke",
				"page:shops", "page:audit", "page:team", "page:devices", "page:security", "page:approvals",
				"page:groups", "page:proxies", "page:logs", "page:environments",
				"element:shop.delete_button", "element:shop.export_button", "element:device.purchase_button",
				"data:shop", "data:device", "resource:device.purchase",
			},
			DataScope: "all",
		},
		{
			Code: "operator", Name: "运营",
			Permissions: []string{
				"shop:create", "shop:read", "shop:update", "shop:transition",
				"device:read", "env:create", "env:check", "env:launch", "env:stop",
				// M2：运营要用店铺网格页（分组筛选 / 建环境 / 看代理状态），
				// 但**不给** proxy:manage——改代理凭据等于改一批店铺的出口，
				// 那是账号隔离的第一道墙，只能由主管动。
				"group:read", "proxy:read", "env:read", "env:update",
				"approval:create", "approval:read", "approval:approve", "job:read",
				"page:shops", "page:devices", "page:approvals", "page:groups", "page:proxies", "page:logs",
				"page:environments",
				"data:shop", "data:device",
			},
			DataScope: "own", // 只看被授权的店铺
		},
		{
			Code: "cs", Name: "客服",
			Permissions: []string{"shop:read", "env:read", "group:read", "page:shops", "page:groups", "data:shop"},
			DataScope:   "own",
		},
		{
			// 代运营（受托方）：代理机构委托批准后，受托方申请人以该角色进入委托方
			// 团队。刻意最小化：只有查看与环境开关，没有审批/删除/凭证/财务——
			// 店铺主账号口令、批量删除这类不可逆动作绝不随委托下发。
			Code: "agency_ops", Name: "代运营（受托方）",
			Permissions: []string{
				"shop:read", "env:check", "env:launch", "env:stop", "env:read", "group:read",
				"page:shops", "data:shop",
			},
			DataScope: "own", // 只看被委托的店铺（配合店铺级 DataGrant 放行）
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

// EnsureBuiltinRoles 幂等写入系统内置角色，并**把内置定义的权限码并集补齐**到
// 已存在的内置角色上（启动时调用，每次升级都要跑）。
//
// 为什么必须补齐：内置角色在库里只写一次，此前 `existing != nil { continue }`
// 直接跳过 ⇒ 新版本新增的权限码（device:retire / approval:route.* /
// device:delete / env:deactivate / env:delete）老部署永远拿不到——
// 运营主管在控制台上看不到删除按钮、调用才 403，且升级日志里毫无痕迹。
//
// 为什么是并集而不是全量对齐（SetPermissions）：
//   - 内置角色是系统级共享行（tenant_id 为空），收紧的正道是**自定义角色**，
//     不是改内置角色；全量对齐会让「部署侧额外加的码」消失，那是功能倒退；
//   - 并集只补不删：已有的不动、缺的补上、多出来的保留。
//   - 代价（明说）：若有人绕过系统直接改库摘掉了某个内置权限码，升级会把它补回来。
//     这是刻意的——内置角色以代码定义为准，不允许长期停在半同步态。
//   - 自定义角色（Builtin=false）**完全不参与**同步，一个码都不动。
func EnsureBuiltinRoles(ctx context.Context, st store.Store) error {
	_, err := EnsureBuiltinRolesWithReport(ctx, st)
	return err
}

// EnsureBuiltinRolesWithReport 与 EnsureBuiltinRoles 同逻辑，额外返回
// `{角色码: 本次新增的权限条数}`——升级时可观测（启动日志打印哪些角色补了哪些能力），
// 也是「同步到底有没有生效」这件事唯一的证据。
func EnsureBuiltinRolesWithReport(ctx context.Context, st store.Store) (map[string]int, error) {
	report := map[string]int{}
	for _, spec := range BuiltinRoles() {
		existing, err := st.Roles().GetByCode(ctx, "", spec.Code)
		if err == nil && existing != nil {
			if !existing.Builtin {
				// 租户级同名自定义角色：不是我们的内置行，一个字都不动。
				continue
			}
			added, err := st.Roles().AddPermissions(ctx, existing.ID, spec.Permissions)
			if err != nil {
				return report, err
			}
			if added > 0 {
				report[spec.Code] = added
			}
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
			return report, err
		}
		report[spec.Code] = len(spec.Permissions)
	}
	return report, nil
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
