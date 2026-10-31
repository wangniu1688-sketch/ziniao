// Package iam 权限引擎：功能 / 页面 / 元素 / 数据 / 资源 五层统一裁决（L6）。
// 所有模块的读操作都必须经过 PolicyEngine，禁止各模块自行判断权限。
package iam

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
	// 店铺环境（M0.3 / Phase 1）
	{Key: "env:create", Layer: LayerFunction, Group: "环境", Desc: "创建店铺浏览器环境", Risk: "medium"},
	{Key: "env:check", Layer: LayerFunction, Group: "环境", Desc: "运行环境自检", Risk: "low"},
	{Key: "env:launch", Layer: LayerFunction, Group: "环境", Desc: "启动店铺浏览器环境（隔离窗口）", Risk: "medium"},
	// 账密托管（M2.2）
	{Key: "shop:credential.manage", Layer: LayerFunction, Group: "店铺", Desc: "维护店铺托管凭证", Risk: "high"},
	{Key: "shop:credential.reveal", Layer: LayerFunction, Group: "店铺", Desc: "查看凭证明文", Risk: "high", Approval: true},
	{Key: "shop:transition", Layer: LayerFunction, Group: "店铺", Desc: "变更店铺生命周期状态", Risk: "medium"},
	// 安全中心（M14）
	{Key: "security:policy.read", Layer: LayerFunction, Group: "安全", Desc: "查看访问策略", Risk: "low"},
	{Key: "security:policy.manage", Layer: LayerFunction, Group: "安全", Desc: "维护访问策略", Risk: "high"},
	{Key: "security:event.read", Layer: LayerFunction, Group: "安全", Desc: "查看安全事件", Risk: "medium"},
	// 审批工单（Phase 2 / 反例 8）
	{Key: "approval:create", Layer: LayerFunction, Group: "审批", Desc: "提交审批单", Risk: "low"},
	{Key: "approval:read", Layer: LayerFunction, Group: "审批", Desc: "查看审批单", Risk: "low"},
	{Key: "approval:approve", Layer: LayerFunction, Group: "审批", Desc: "审批（通过/拒绝）", Risk: "high"},
	// 定时任务（Phase 2）
	{Key: "job:read", Layer: LayerFunction, Group: "系统", Desc: "查看定时任务", Risk: "low"},
	{Key: "job:run", Layer: LayerFunction, Group: "系统", Desc: "手动触发定时任务", Risk: "medium"},
	// 页面级
	{Key: "page:shops", Layer: LayerPage, Group: "店铺", Desc: "店铺列表页"},
	{Key: "page:finance", Layer: LayerPage, Group: "财务", Desc: "财务管理页"},
	{Key: "page:audit", Layer: LayerPage, Group: "安全", Desc: "审计日志页"},
	{Key: "page:team", Layer: LayerPage, Group: "团队", Desc: "团队管理页"},
	{Key: "page:devices", Layer: LayerPage, Group: "设备", Desc: "设备列表页"},
	{Key: "page:security", Layer: LayerPage, Group: "安全", Desc: "安全中心页"},
	{Key: "page:approvals", Layer: LayerPage, Group: "审批", Desc: "审批工单页"},
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

// RequiredPage 路径 → 页面级权限。
var RequiredPage = map[string]string{
	"/shops":    "page:shops",
	"/finance":  "page:finance",
	"/audit":    "page:audit",
	"/team":     "page:team",
	"/devices":  "page:devices",
	"/security": "page:security",
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
