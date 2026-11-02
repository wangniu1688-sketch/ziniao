// Package iam 权限引擎：功能 / 页面 / 元素 / 数据 / 资源 五层统一裁决（L6）。
// 所有模块的读操作都必须经过 PolicyEngine，禁止各模块自行判断权限。
package iam

import "ziniao/platform/types"

// Layer 权限层级。
type Layer string

const (
	LayerFunction Layer = "function" // 功能级：能不能做这个动作
	LayerPage     Layer = "page"     // 页面级：能不能进这个页面
	LayerElement  Layer = "element"  // 元素级：按钮/字段能不能看到
	LayerData     Layer = "data"     // 数据级：能看到哪些行
	LayerResource Layer = "resource" // 资源级：如紫鸟盒子购买权
)

// Permission 权限标识定义。
type Permission struct {
	Key      string `json:"key"`
	Layer    Layer  `json:"layer"`
	Group    string `json:"group"`
	Desc     string `json:"desc"`
	Risk     string `json:"risk"` // low|medium|high
	Approval bool   `json:"approval_required"`
}

// Catalog 权限目录（Phase 0 覆盖店铺/团队/安全/财务主干；后续模块按同一规范追加）。
var Catalog = []Permission{
	// 店铺（M2）
	{Key: "shop:create", Layer: LayerFunction, Group: "店铺", Desc: "新建店铺", Risk: "medium"},
	{Key: "shop:read", Layer: LayerFunction, Group: "店铺", Desc: "查看店铺", Risk: "low"},
	{Key: "shop:update", Layer: LayerFunction, Group: "店铺", Desc: "修改店铺", Risk: "medium"},
	{Key: "shop:delete", Layer: LayerFunction, Group: "店铺", Desc: "删除/回收店铺", Risk: "high"},
	// 团队（M12）
	{Key: "member:invite", Layer: LayerFunction, Group: "团队", Desc: "邀请/添加成员", Risk: "medium"},
	{Key: "member:list", Layer: LayerFunction, Group: "团队", Desc: "查看成员列表", Risk: "low"},
	{Key: "role:grant", Layer: LayerFunction, Group: "团队", Desc: "授予角色", Risk: "high"},
	{Key: "role:manage", Layer: LayerFunction, Group: "团队", Desc: "角色与权限管理", Risk: "high"},
	// 安全与审计（M14/M15）
	{Key: "audit:read", Layer: LayerFunction, Group: "安全", Desc: "查看审计日志", Risk: "medium"},
	{Key: "audit:export", Layer: LayerFunction, Group: "安全", Desc: "导出审计日志", Risk: "high", Approval: true},
	// 财务（M13/M7）
	{Key: "finance:read", Layer: LayerFunction, Group: "财务", Desc: "查看财务数据", Risk: "high"},
	// 设备与网络（M3 / Phase 1）
	{Key: "device:read", Layer: LayerFunction, Group: "设备", Desc: "查看设备", Risk: "low"},
	{Key: "device:purchase", Layer: LayerFunction, Group: "设备", Desc: "购买云网络设备", Risk: "high"},
	{Key: "device:import", Layer: LayerFunction, Group: "设备", Desc: "导入自有设备/紫鸟盒子", Risk: "medium"},
	{Key: "device:bind", Layer: LayerFunction, Group: "设备", Desc: "绑定店铺到设备", Risk: "medium"},
	{Key: "device:renew", Layer: LayerFunction, Group: "设备", Desc: "设备续费", Risk: "medium"},
	// 退役 = 归还出口并停用设备：绑在这台设备上的店铺会一起失去出口，
	// 所以它和申购同属高风险，不给普通运营。
	{Key: "device:retire", Layer: LayerFunction, Group: "设备", Desc: "设备退役（归还出口并停用）", Risk: "high"},
	// 删除 = 台账出清 + 回退 device_count 计量，不可逆，与退役同级。
	{Key: "device:delete", Layer: LayerFunction, Group: "设备", Desc: "删除设备（台账出清并释放配额）", Risk: "high"},
	// 店铺环境（M0.3 / Phase 1）
	{Key: "env:create", Layer: LayerFunction, Group: "环境", Desc: "创建店铺浏览器环境", Risk: "medium"},
	{Key: "env:check", Layer: LayerFunction, Group: "环境", Desc: "运行环境自检", Risk: "low"},
	{Key: "env:launch", Layer: LayerFunction, Group: "环境", Desc: "启动店铺浏览器环境（隔离窗口）", Risk: "medium"},
	{Key: "env:stop", Layer: LayerFunction, Group: "环境", Desc: "关闭店铺浏览器环境（窗口关闭上报）", Risk: "low"},
	// 停用/启用/删除环境：停用会释放出口绑定（再启用可能换 IP），删除不可逆，
	// 都属管理动作，与设备退役同级，不给普通运营与代运营方。
	{Key: "env:deactivate", Layer: LayerFunction, Group: "环境", Desc: "停用/启用店铺环境（释放出口绑定）", Risk: "high"},
	{Key: "env:delete", Layer: LayerFunction, Group: "环境", Desc: "删除店铺环境（不可逆）", Risk: "high"},
	// 出口代理商路由（多代理商）：只读可见性。供应商名单算采购信息，
	// 只给老板与运营主管，不给普通运营与代运营方。
	{Key: "egress:read", Layer: LayerFunction, Group: "出口", Desc: "查看出口代理商路由表与占用情况", Risk: "low"},
	// 账密托管（M2.2）
	{Key: "shop:credential.manage", Layer: LayerFunction, Group: "店铺", Desc: "维护店铺托管凭证", Risk: "high"},
	{Key: "shop:credential.reveal", Layer: LayerFunction, Group: "店铺", Desc: "查看凭证明文", Risk: "high", Approval: true},
	{Key: "shop:transition", Layer: LayerFunction, Group: "店铺", Desc: "变更店铺生命周期状态", Risk: "medium"},
	// 安全中心（M14）
	{Key: "security:policy.read", Layer: LayerFunction, Group: "安全", Desc: "查看访问策略", Risk: "low"},
	{Key: "security:policy.manage", Layer: LayerFunction, Group: "安全", Desc: "维护访问策略", Risk: "high"},
	{Key: "security:event.read", Layer: LayerFunction, Group: "安全", Desc: "查看安全事件", Risk: "medium"},
	// 本地 MITM 根证书（M3/T7）：装/删的是**当前用户信任库**，影响本机所有浏览器
	// 对 HTTPS 的判定，属高危管理动作——只给老板与运营主管，运营不给。
	{Key: "ca:manage", Layer: LayerFunction, Group: "安全", Desc: "安装/卸载本机 MITM 根证书", Risk: "high"},
	// 审批工单（Phase 2 / 反例 8）
	{Key: "approval:create", Layer: LayerFunction, Group: "审批", Desc: "提交审批单", Risk: "low"},
	{Key: "approval:read", Layer: LayerFunction, Group: "审批", Desc: "查看审批单", Risk: "low"},
	{Key: "approval:approve", Layer: LayerFunction, Group: "审批", Desc: "审批（通过/拒绝）", Risk: "high"},
	// 审批路由规则（approval_route_rules）：改的是「谁能批什么」，属控制变更，
	// 只给老板与运营主管——普通运营若能把级别调低，审批本身就是一道可绕过的门。
	{Key: "approval:route.read", Layer: LayerFunction, Group: "审批", Desc: "查看审批路由规则", Risk: "low"},
	{Key: "approval:route.manage", Layer: LayerFunction, Group: "审批", Desc: "维护审批路由规则（谁批/是否会签）", Risk: "high"},
	// 代理机构委托（approval_flows，跨租户两方流程）
	{Key: "delegation:apply", Layer: LayerFunction, Group: "委托", Desc: "提交代理机构委托申请（受托方）", Risk: "medium"},
	{Key: "delegation:read", Layer: LayerFunction, Group: "委托", Desc: "查看委托审批单（双方各自视角）", Risk: "low"},
	{Key: "delegation:approve", Layer: LayerFunction, Group: "委托", Desc: "审批委托申请（委托方）", Risk: "high"},
	{Key: "delegation:revoke", Layer: LayerFunction, Group: "委托", Desc: "撤销生效中的委托（委托方，授权即时回收）", Risk: "high"},
	// 定时任务（Phase 2）
	{Key: "job:read", Layer: LayerFunction, Group: "系统", Desc: "查看定时任务", Risk: "low"},
	{Key: "job:run", Layer: LayerFunction, Group: "系统", Desc: "手动触发定时任务", Risk: "medium"},
	// 启动器内核（M2）：分组 / 手工代理 / 环境聚合视图。
	// group:manage 与 proxy:manage 都是中高危：改分组会让一批店铺的归类瞬间变化，
	// 改代理凭据等于改一批店铺的出口——出口是账号隔离的第一道墙。
	{Key: "group:read", Layer: LayerFunction, Group: "店铺", Desc: "查看店铺分组", Risk: "low"},
	{Key: "group:manage", Layer: LayerFunction, Group: "店铺", Desc: "分组增删改", Risk: "medium"},
	{Key: "proxy:read", Layer: LayerFunction, Group: "网络", Desc: "查看手工代理台账（不含口令）", Risk: "low"},
	{Key: "proxy:manage", Layer: LayerFunction, Group: "网络", Desc: "代理增删改与连通性拨测", Risk: "high"},
	{Key: "env:read", Layer: LayerFunction, Group: "环境", Desc: "环境聚合列表与详情", Risk: "low"},
	{Key: "env:update", Layer: LayerFunction, Group: "环境", Desc: "改环境配置（绑定代理 / 改分组）", Risk: "medium"},
	// 页面级
	{Key: "page:shops", Layer: LayerPage, Group: "店铺", Desc: "店铺列表页"},
	{Key: "page:finance", Layer: LayerPage, Group: "财务", Desc: "财务管理页"},
	{Key: "page:audit", Layer: LayerPage, Group: "安全", Desc: "审计日志页"},
	{Key: "page:team", Layer: LayerPage, Group: "团队", Desc: "团队管理页"},
	{Key: "page:devices", Layer: LayerPage, Group: "设备", Desc: "设备列表页"},
	{Key: "page:security", Layer: LayerPage, Group: "安全", Desc: "安全中心页"},
	{Key: "page:approvals", Layer: LayerPage, Group: "审批", Desc: "审批工单页"},
	{Key: "page:groups", Layer: LayerPage, Group: "店铺", Desc: "分组管理页"},
	{Key: "page:proxies", Layer: LayerPage, Group: "网络", Desc: "代理台账页"},
	{Key: "page:logs", Layer: LayerPage, Group: "安全", Desc: "日志中心页（审计与安全事件聚合）"},
	{Key: "page:environments", Layer: LayerPage, Group: "环境", Desc: "环境页（店铺网格内的环境视图）"},
	// 元素级（DOM 注入层据此屏蔽，L6 第三层）
	{Key: "element:shop.delete_button", Layer: LayerElement, Group: "店铺", Desc: "删除店铺按钮"},
	{Key: "element:shop.export_button", Layer: LayerElement, Group: "店铺", Desc: "导出店铺按钮"},
	{Key: "element:finance.export_button", Layer: LayerElement, Group: "财务", Desc: "财务报表导出按钮"},
	{Key: "element:finance.profit_field", Layer: LayerElement, Group: "财务", Desc: "利润字段"},
	{Key: "element:device.purchase_button", Layer: LayerElement, Group: "设备", Desc: "购买设备按钮"},
	// 数据级
	{Key: "data:shop", Layer: LayerData, Group: "店铺", Desc: "店铺数据范围"},
	{Key: "data:device", Layer: LayerData, Group: "设备", Desc: "设备数据范围"},
	// 资源级
	{Key: "resource:device.purchase", Layer: LayerResource, Group: "设备", Desc: "购买云网络设备"},
	{Key: "resource:box.purchase", Layer: LayerResource, Group: "设备", Desc: "紫鸟盒子购买权（官方资源级权限实例）"},
}

var catalogIndex = func() map[string]Permission {
	m := make(map[string]Permission, len(Catalog))
	for _, p := range Catalog {
		m[p.Key] = p
	}
	return m
}()

// Lookup 查询权限定义。
func Lookup(key string) (Permission, bool) {
	p, ok := catalogIndex[key]
	return p, ok
}

// LayerOf 返回权限所属层级。
func LayerOf(key string) Layer {
	if p, ok := catalogIndex[key]; ok {
		return p.Layer
	}
	return LayerFunction
}

// AllKeys 返回全部权限标识（用于 owner 角色与权限目录接口）。
func AllKeys() []string {
	keys := make([]string, 0, len(Catalog))
	for _, p := range Catalog {
		keys = append(keys, p.Key)
	}
	return keys
}

// PageElements 页面 → 该页存在的元素级权限（用于计算 DOM 屏蔽清单）。
var PageElements = map[string][]string{
	"/shops":   {"element:shop.delete_button", "element:shop.export_button"},
	"/finance": {"element:finance.export_button", "element:finance.profit_field"},
	"/devices": {"element:device.purchase_button"},
}

// RequiredPage 路径 → 页面级权限（M2 终审 T15 补齐：代理台账 / 分组 / 日志 / 环境四处登记）。
//
// 权限双层口径（终审定稿，禁止各层自行发挥）：
//  1. 资源接口（带 Action 的裁决，如 GET /api/v1/proxies → proxy:read）由**后端资源/功能权限码**
//     强制拦截，这是唯一的准入闸门；缺失 page:* 不得改变这一判定。
//  2. page:* **仅用于前端导航可见性与纯页面导航裁决**（Action 为空的裁决），
//     不单独承担网关拦截责任。见 Engine.Check 的页面层分支与 NavSections。
var RequiredPage = map[string]string{
	"/shops":        "page:shops",
	"/finance":      "page:finance",
	"/audit":        "page:audit",
	"/team":         "page:team",
	"/devices":      "page:devices",
	"/security":     "page:security",
	"/groups":       "page:groups",
	"/proxies":      "page:proxies",
	"/logs":         "page:logs",
	"/environments": "page:environments",
}

// NavSection 侧边导航项。可见性 = 资源码 AND 页面码（两者都为空 = 恒可见）：
//   - Perm：资源/功能码，决定「能不能」；缺失 ⇒ 导航不可见（终审第 2.2 节口径）。
//   - Page：page:* 页面码，决定「这个页面给不给你看」；缺失同样不可见。
//     两者都要，是为了避免「有只读码但页面没登记」或「页面登记了但资源码没给」的半可见态。
type NavSection struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Perm  string `json:"perm,omitempty"`
	Page  string `json:"page,omitempty"`
}

// NavSections 启动器侧边栏导航清单（单一事实源：后端定义，前端按 visible 渲染，
// 不再由前端自行决定画不画）。
var NavSections = []NavSection{
	{ID: "dashboard", Title: "工作台"},
	{ID: "shops", Title: "店铺管理", Perm: "shop:read", Page: "page:shops"},
	{ID: "proxies", Title: "代理管理", Perm: "proxy:read", Page: "page:proxies"},
	{ID: "groups", Title: "分组管理", Perm: "group:read", Page: "page:groups"},
	{ID: "devices", Title: "设备与网络", Perm: "device:read", Page: "page:devices"},
	{ID: "logs", Title: "日志中心", Perm: "audit:read", Page: "page:logs"},
	{ID: "settings", Title: "系统设置"},
}

// NavFor 按主体持有的权限码计算导航可见性：资源码与页面码**同时**具备才可见。
func NavFor(roles []types.Role) []NavItem {
	perms := unionPermissions(roles)
	out := make([]NavItem, 0, len(NavSections))
	for _, s := range NavSections {
		visible := (s.Perm == "" || perms[s.Perm]) && (s.Page == "" || perms[s.Page])
		out = append(out, NavItem{
			ID:      s.ID,
			Title:   s.Title,
			Perm:    s.Perm,
			Page:    s.Page,
			Visible: visible,
		})
	}
	return out
}

// NavItem 下发给前端的导航项（visible=false 时前端不渲染）。
type NavItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Perm    string `json:"perm,omitempty"`
	Page    string `json:"page,omitempty"`
	Visible bool   `json:"visible"`
}

// ElementSelector 元素级权限 → DOM 选择器（客户端注入层据此屏蔽）。
func ElementSelector(key string) string {
	if layer := LayerOf(key); layer != LayerElement {
		return ""
	}
	name := key[len("element:"):]
	return "[data-el=\"" + name + "\"]"
}

// FieldSelector 字段 → DOM 选择器（字段级遮蔽，M13）。
func FieldSelector(field string) string {
	return "[data-field=\"" + field + "\"]"
}
