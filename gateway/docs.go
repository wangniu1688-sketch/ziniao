package gateway

import "ziniao/platform/openapi"

// docs 登记全部对外接口的 OpenAPI 描述（路由声明即文档）。
// 新增路由必须同时在这里登记，否则 /openapi.json 会缺项（CI 有测试覆盖）。
func (s *Server) docs() {
	s.mux.HandleFunc("GET /openapi.json", openapi.Handler("紫鸟浏览器管理系统 API", "phase0"))

	ops := []openapi.Op{
		{Method: "GET", Path: "/healthz", Tag: "系统", Summary: "健康检查"},
		{Method: "GET", Path: "/openapi.json", Tag: "系统", Summary: "OpenAPI 文档"},

		{Method: "POST", Path: "/api/v1/auth/register", Tag: "身份", Summary: "注册团队与主账号", Request: "RegisterInput", Response: "CreateResult"},
		{Method: "POST", Path: "/api/v1/auth/login", Tag: "身份", Summary: "登录（含登录保护判定）"},
		{Method: "POST", Path: "/api/v1/auth/logout", Tag: "身份", Summary: "登出并写登出日志", Permission: "-"},
		{Method: "GET", Path: "/api/v1/me", Tag: "身份", Summary: "当前用户与权限概览", Permission: "-"},

		{Method: "GET", Path: "/api/v1/tenants/{id}", Tag: "租户", Summary: "团队详情", Permission: "member:list"},
		{Method: "GET", Path: "/api/v1/tenants/{id}/members", Tag: "租户", Summary: "成员列表", Permission: "member:list"},
		{Method: "POST", Path: "/api/v1/tenants/{id}/members", Tag: "租户", Summary: "邀请/添加成员", Permission: "member:invite"},
		{Method: "POST", Path: "/api/v1/tenants/{id}/members/{userID}/roles", Tag: "租户", Summary: "授予角色（支持临时授权）", Permission: "role:grant"},

		{Method: "GET", Path: "/api/v1/iam/roles", Tag: "权限", Summary: "角色列表", Permission: "member:list"},
		{Method: "GET", Path: "/api/v1/iam/permissions", Tag: "权限", Summary: "权限目录（五层）", Permission: "-"},
		{Method: "POST", Path: "/api/v1/iam/check", Tag: "权限", Summary: "策略裁决调试", Permission: "-"},
		{Method: "GET", Path: "/api/v1/iam/dom-policy", Tag: "权限", Summary: "DOM 注入层策略（字段遮蔽/元素屏蔽/水印）", Permission: "-"},

		{Method: "POST", Path: "/api/v1/shops", Tag: "店铺", Summary: "新建店铺（带审计与计量）", Permission: "shop:create"},
		{Method: "GET", Path: "/api/v1/shops", Tag: "店铺", Summary: "店铺列表（受数据范围与字段遮蔽约束）", Permission: "shop:read"},
		{Method: "GET", Path: "/api/v1/shops/{id}", Tag: "店铺", Summary: "店铺详情", Permission: "shop:read"},
		{Method: "DELETE", Path: "/api/v1/shops/{id}", Tag: "店铺", Summary: "删除店铺（软删+高危告警）", Permission: "shop:delete"},

		{Method: "GET", Path: "/api/v1/audit/logs", Tag: "审计", Summary: "审计日志查询", Permission: "audit:read"},
		{Method: "GET", Path: "/api/v1/audit/logs/export", Tag: "审计", Summary: "审计导出（需二次审批）", Permission: "audit:export"},
		{Method: "GET", Path: "/api/v1/audit/verify", Tag: "审计", Summary: "哈希链完整性校验", Permission: "audit:read"},

		{Method: "GET", Path: "/api/v1/notifications", Tag: "通知", Summary: "站内消息列表", Permission: "-"},
		{Method: "POST", Path: "/api/v1/notifications/{id}/read", Tag: "通知", Summary: "标记已读", Permission: "-"},

		// Phase 1
		{Method: "GET", Path: "/api/v1/devices", Tag: "设备", Summary: "设备列表", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/purchase", Tag: "设备", Summary: "购买云网络设备", Permission: "device:purchase"},
		{Method: "POST", Path: "/api/v1/devices/import", Tag: "设备", Summary: "导入自有设备/紫鸟盒子", Permission: "device:import"},
		{Method: "POST", Path: "/api/v1/devices/{id}/bind", Tag: "设备", Summary: "绑定店铺到设备（配额模型，非互斥锁）", Permission: "device:bind"},
		{Method: "POST", Path: "/api/v1/devices/{id}/acquire", Tag: "设备", Summary: "占用设备（并发人数按人去重）", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/{id}/release", Tag: "设备", Summary: "释放占用", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/{id}/renew", Tag: "设备", Summary: "设备续费", Permission: "device:renew"},
		{Method: "GET", Path: "/api/v1/devices/{id}/health", Tag: "设备", Summary: "设备健康评分", Permission: "device:read"},

		{Method: "POST", Path: "/api/v1/shops/{id}/status", Tag: "店铺", Summary: "生命周期状态迁移", Permission: "shop:transition"},
		{Method: "PUT", Path: "/api/v1/shops/{id}/group", Tag: "店铺", Summary: "设置分组与标签", Permission: "shop:update"},
		{Method: "GET", Path: "/api/v1/shops/trash", Tag: "店铺", Summary: "回收站", Permission: "shop:read"},
		{Method: "POST", Path: "/api/v1/shops/{id}/restore", Tag: "店铺", Summary: "恢复店铺", Permission: "shop:update"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment", Tag: "环境", Summary: "创建店铺浏览器环境（画像+内核+TLS基线）", Permission: "env:create"},
		{Method: "GET", Path: "/api/v1/shops/{id}/environment", Tag: "环境", Summary: "查询环境", Permission: "env:check"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/check", Tag: "环境", Summary: "环境自检报告（自洽性+漂移+基线校验）", Permission: "env:check"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/launch", Tag: "环境", Summary: "下发环境启动单（UA/注入脚本/代理端口/内核基线，客户端原样执行）", Permission: "env:launch"},
		{Method: "PUT", Path: "/api/v1/shops/{id}/credentials", Tag: "店铺", Summary: "写入托管凭证（信封加密）", Permission: "shop:credential.manage"},
		{Method: "GET", Path: "/api/v1/shops/{id}/credentials", Tag: "店铺", Summary: "已托管凭证类型（不含密文）", Permission: "shop:credential.manage"},
		{Method: "GET", Path: "/api/v1/shops/{id}/credentials/{kind}", Tag: "店铺", Summary: "查看凭证明文（高危+审批+告警）", Permission: "shop:credential.reveal"},

		{Method: "GET", Path: "/api/v1/security/policies", Tag: "安全", Summary: "访问策略列表", Permission: "security:policy.read"},
		{Method: "POST", Path: "/api/v1/security/policies", Tag: "安全", Summary: "新建访问策略", Permission: "security:policy.manage"},
		{Method: "PUT", Path: "/api/v1/security/policies/{id}", Tag: "安全", Summary: "更新访问策略", Permission: "security:policy.manage"},
		{Method: "DELETE", Path: "/api/v1/security/policies/{id}", Tag: "安全", Summary: "删除访问策略", Permission: "security:policy.manage"},
		{Method: "GET", Path: "/api/v1/security/templates", Tag: "安全", Summary: "策略模板库", Permission: "security:policy.read"},
		{Method: "GET", Path: "/api/v1/security/events", Tag: "安全", Summary: "安全事件（事前/事中/事后）", Permission: "security:event.read"},
		{Method: "POST", Path: "/api/v1/security/intercept", Tag: "安全", Summary: "事前拦截判定（页面/元素/动作）", Permission: "shop:read"},

		{Method: "GET", Path: "/api/v1/fingerprint/tls-baselines", Tag: "指纹", Summary: "TLS 指纹基线（JA3/JA4/HTTP2）", Permission: "env:check"},
		{Method: "GET", Path: "/api/v1/fingerprint/layers", Tag: "指纹", Summary: "字段→注入层→可检测性对照表", Permission: "env:check"},

		// Phase 2
		{Method: "GET", Path: "/api/v1/approvals", Tag: "审批", Summary: "审批单列表", Permission: "approval:read"},
		{Method: "POST", Path: "/api/v1/approvals", Tag: "审批", Summary: "提交审批单", Permission: "approval:create"},
		{Method: "POST", Path: "/api/v1/approvals/{id}/approve", Tag: "审批", Summary: "通过（审批人≠申请人）", Permission: "approval:approve"},
		{Method: "POST", Path: "/api/v1/approvals/{id}/reject", Tag: "审批", Summary: "拒绝", Permission: "approval:approve"},
		{Method: "POST", Path: "/api/v1/approvals/{id}/cancel", Tag: "审批", Summary: "撤销（仅申请人本人）", Permission: "approval:create"},
		{Method: "GET", Path: "/api/v1/jobs", Tag: "系统", Summary: "定时任务列表", Permission: "job:read"},
		{Method: "POST", Path: "/api/v1/jobs/run", Tag: "系统", Summary: "手动触发定时任务", Permission: "job:run"},
		{Method: "POST", Path: "/api/v1/shops/batch/import", Tag: "店铺", Summary: "批量导入（逐行配额校验，部分成功返回 207）", Permission: "shop:create"},
	}
	for _, op := range ops {
		openapi.Register(op)
	}
}
