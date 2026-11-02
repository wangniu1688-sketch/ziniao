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

		{Method: "GET", Path: "/api/v1/notifications", Tag: "通知", Summary: "站内消息列表（items/total/unread）", Permission: "-"},
		{Method: "POST", Path: "/api/v1/notifications/{id}/read", Tag: "通知", Summary: "标记已读", Permission: "-"},
		{Method: "GET", Path: "/api/v1/notifications/preferences", Tag: "通知", Summary: "我的通知偏好（channels=已启用外发通道，available=可勾选清单）", Permission: "-"},
		{Method: "PUT", Path: "/api/v1/notifications/preferences", Tag: "通知", Summary: "设置我的通知偏好（站内信恒开；未知通道名直接 400）", Permission: "-"},

		// Phase 1
		{Method: "GET", Path: "/api/v1/devices", Tag: "设备", Summary: "设备列表", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/purchase", Tag: "设备", Summary: "购买云网络设备（金额超阈值须 device.purchase 审批单，大额双人复核）", Permission: "device:purchase"},
		{Method: "POST", Path: "/api/v1/devices/import", Tag: "设备", Summary: "导入自有设备/紫鸟盒子", Permission: "device:import"},
		{Method: "POST", Path: "/api/v1/devices/{id}/bind", Tag: "设备", Summary: "绑定店铺到设备（配额模型，非互斥锁）", Permission: "device:bind"},
		{Method: "POST", Path: "/api/v1/devices/{id}/acquire", Tag: "设备", Summary: "占用设备（并发人数按人去重）", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/{id}/release", Tag: "设备", Summary: "释放占用", Permission: "device:read"},
		{Method: "POST", Path: "/api/v1/devices/{id}/renew", Tag: "设备", Summary: "设备续费", Permission: "device:renew"},
		{Method: "POST", Path: "/api/v1/devices/{id}/retire", Tag: "设备", Summary: "设备退役（归还出口并停用，不可逆）", Permission: "device:retire"},
		{Method: "DELETE", Path: "/api/v1/devices/{id}", Tag: "设备", Summary: "删除设备（仅限已退役/已过期；台账出清并释放 device_count 配额）", Permission: "device:delete"},
		{Method: "GET", Path: "/api/v1/devices/{id}/health", Tag: "设备", Summary: "设备健康评分", Permission: "device:read"},

		{Method: "POST", Path: "/api/v1/shops/{id}/status", Tag: "店铺", Summary: "生命周期状态迁移", Permission: "shop:transition"},
		{Method: "PUT", Path: "/api/v1/shops/{id}/group", Tag: "店铺", Summary: "设置分组与标签", Permission: "shop:update"},
		{Method: "GET", Path: "/api/v1/shops/trash", Tag: "店铺", Summary: "回收站", Permission: "shop:read"},
		{Method: "POST", Path: "/api/v1/shops/{id}/restore", Tag: "店铺", Summary: "恢复店铺", Permission: "shop:update"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment", Tag: "环境", Summary: "创建店铺浏览器环境（画像+内核+TLS基线）", Permission: "env:create"},
		{Method: "GET", Path: "/api/v1/shops/{id}/environment", Tag: "环境", Summary: "查询环境", Permission: "env:check"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/check", Tag: "环境", Summary: "环境自检报告（自洽性+漂移+基线校验）", Permission: "env:check"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/launch", Tag: "环境", Summary: "下发环境启动单（UA/注入脚本/代理端口/内核基线，客户端原样执行）", Permission: "env:launch"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/stop", Tag: "环境", Summary: "关闭环境（窗口关闭上报，状态 running→idle，幂等）", Permission: "env:stop"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/deactivate", Tag: "环境", Summary: "停用环境（退出服务并释放出口绑定，幂等）", Permission: "env:deactivate"},
		{Method: "POST", Path: "/api/v1/shops/{id}/environment/activate", Tag: "环境", Summary: "重新启用已停用的环境（stopped→idle）", Permission: "env:deactivate"},
		{Method: "DELETE", Path: "/api/v1/shops/{id}/environment", Tag: "环境", Summary: "删除环境（释放出口+物理删除行，不可逆）", Permission: "env:delete"},
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
		{Method: "GET", Path: "/api/v1/approvals/rules", Tag: "审批", Summary: "审批路由规则列表（含内置规则）", Permission: "approval:route.read"},
		{Method: "POST", Path: "/api/v1/approvals/rules", Tag: "审批", Summary: "新建/更新审批路由规则", Permission: "approval:route.manage"},
		{Method: "DELETE", Path: "/api/v1/approvals/rules/{id}", Tag: "审批", Summary: "删除审批路由规则（回落内置）", Permission: "approval:route.manage"},
		{Method: "GET", Path: "/api/v1/jobs", Tag: "系统", Summary: "定时任务列表", Permission: "job:read"},
		{Method: "POST", Path: "/api/v1/jobs/run", Tag: "系统", Summary: "手动触发定时任务", Permission: "job:run"},
		// 出口代理商路由与历史（响应不含任何凭据：L4）
		{Method: "GET", Path: "/api/v1/egress/vendors", Tag: "系统", Summary: "出口代理商路由表与占用", Permission: "egress:read"},
		{Method: "GET", Path: "/api/v1/egress/history", Tag: "系统", Summary: "出口绑定历史（只追加，可按 env/ASN/动作/时间过滤）", Permission: "egress:read"},
		{Method: "GET", Path: "/api/v1/egress/asn-sharing", Tag: "系统", Summary: "同一 ASN 被多个环境使用过的关联线索", Permission: "egress:read"},
		{Method: "POST", Path: "/api/v1/shops/batch/import", Tag: "店铺", Summary: "批量导入（逐行配额校验，部分成功返回 207）", Permission: "shop:create"},
		{Method: "POST", Path: "/api/v1/shops/batch/delete", Tag: "店铺", Summary: "批量删除（须持 shop.bulk_delete 审批单，单次核销）", Permission: "shop:delete"},

		// 代理机构委托审批（approval_flows，跨租户两方流程）
		{Method: "GET", Path: "/api/v1/delegations", Tag: "委托", Summary: "委托单列表（party=agency 查自己发出的，默认查收到的）", Permission: "delegation:read"},
		{Method: "POST", Path: "/api/v1/delegations", Tag: "委托", Summary: "提交委托申请（受托方指定委托方 slug + 店铺范围 + 有效期）", Permission: "delegation:apply"},
		{Method: "POST", Path: "/api/v1/delegations/{id}/approve", Tag: "委托", Summary: "批准（委托方 owner/ops_manager；授权同事务落地）", Permission: "delegation:approve"},
		{Method: "POST", Path: "/api/v1/delegations/{id}/reject", Tag: "委托", Summary: "拒绝（委托方）", Permission: "delegation:approve"},
		{Method: "POST", Path: "/api/v1/delegations/{id}/cancel", Tag: "委托", Summary: "撤回（仅受托方申请人本人）", Permission: "delegation:apply"},
		{Method: "POST", Path: "/api/v1/delegations/{id}/revoke", Tag: "委托", Summary: "撤销生效中的委托（授权即时回收）", Permission: "delegation:revoke"},

		// M1 启动器自描述（登录即可；不含任何凭据：DSN / JWT secret / 上游口令一律不返回）
		{Method: "GET", Path: "/api/v1/system/info", Tag: "系统", Summary: "启动器自描述：数据目录 / 浏览器探测 / CA 就绪 / 能力位 / 监听地址", Permission: "-"},
		{Method: "GET", Path: "/api/v1/system/stats", Tag: "系统", Summary: "首页静态统计（店铺/运行中的环境/设备/代理/分组/待审批）", Permission: "-", Idempotent: true},

		// M3：本机 MITM 根证书的装/删（改当前用户信任库，属高危管理动作）。
		{Method: "GET", Path: "/api/v1/system/ca", Tag: "系统", Summary: "本地 MITM 根证书状态（ca_ready 的只读来源）", Permission: "-"},
		{Method: "POST", Path: "/api/v1/system/ca/install", Tag: "系统", Summary: "生成并安装本地 MITM 根证书到当前用户信任库（不需要管理员权限）", Permission: "ca:manage"},
		{Method: "POST", Path: "/api/v1/system/ca/uninstall", Tag: "系统", Summary: "从当前用户信任库移除本地 MITM 根证书（卸载后启动会被阻断）", Permission: "ca:manage"},

		// M2 启动器内核：手工代理台账 / 分组 / 环境聚合视图 / 聚合日志。
		// 代理相关接口一律不返回口令（只回 has_password），并在响应体层面剔除凭据字段。
		{Method: "GET", Path: "/api/v1/proxies/{id}", Tag: "代理", Summary: "代理详情（唯一带 password_mask 的接口，掩码不是明文）", Permission: "proxy:read"},
		{Method: "GET", Path: "/api/v1/proxies", Tag: "代理", Summary: "代理台账列表（含 has_password、bound_env_count 与拨测结果 check_status/exit_ip/exit_country/latency_ms，不含口令；可按 check_status 过滤）", Permission: "proxy:read"},
		{Method: "POST", Path: "/api/v1/proxies", Tag: "代理", Summary: "新建代理（password 只进不出，落库即信封加密；type=isp|static_residential|residential|datacenter）", Permission: "proxy:manage"},
		{Method: "PUT", Path: "/api/v1/proxies/{id}", Tag: "代理", Summary: "修改代理（password：nil 不改 / 空串清空 / 非空设置；未传 password 时保留原口令）", Permission: "proxy:manage"},
		{Method: "DELETE", Path: "/api/v1/proxies/{id}", Tag: "代理", Summary: "删除代理（被环境引用则 409）", Permission: "proxy:manage"},
		{Method: "POST", Path: "/api/v1/proxies/{id}/test", Tag: "代理", Summary: "服务端连通性拨测并落库（失败返回 200 + ok=false，不回传凭据；ZINIAO_PROXY_TEST=off 时返回 skipped=true 且不出网）", Permission: "proxy:manage"},

		{Method: "GET", Path: "/api/v1/groups", Tag: "分组", Summary: "分组列表（含 environment_count）", Permission: "group:read"},
		{Method: "POST", Path: "/api/v1/groups", Tag: "分组", Summary: "新建分组", Permission: "group:manage"},
		{Method: "PUT", Path: "/api/v1/groups/{id}", Tag: "分组", Summary: "改名 / 改排序（改名同事务同步成员店铺 group_name）", Permission: "group:manage"},
		{Method: "DELETE", Path: "/api/v1/groups/{id}", Tag: "分组", Summary: "删除分组（仅空分组，非空 409）", Permission: "group:manage"},

		{Method: "GET", Path: "/api/v1/environments", Tag: "环境", Summary: "环境聚合列表（跨店铺；不含注入脚本正文与代理口令）", Permission: "env:read"},
		{Method: "POST", Path: "/api/v1/environments", Tag: "环境", Summary: "原子创建店铺 + 环境 + profile 元数据（本期不拉起浏览器）", Permission: "shop:create + env:create"},
		{Method: "GET", Path: "/api/v1/environments/runtimes", Tag: "环境", Summary: "设备与网络只读台账（仅数据库侧事实，不含 PID/CDP/运行时长）", Permission: "env:read"},
		{Method: "GET", Path: "/api/v1/environments/{id}", Tag: "环境", Summary: "环境聚合详情", Permission: "env:read"},
		{Method: "PUT", Path: "/api/v1/environments/{id}/proxy", Tag: "环境", Summary: "环境绑定 / 解绑手工代理（运行中禁止换出口；ZINIAO_PROXY_TEST=on 时要求代理 check_status=ok，返回 warnings 为软校验告警）", Permission: "env:update"},
		{Method: "PUT", Path: "/api/v1/environments/{id}/group", Tag: "环境", Summary: "环境移动分组（group_id 为空 = 移出分组）", Permission: "env:update"},
		{Method: "GET", Path: "/api/v1/environments/local-runtime", Tag: "环境", Summary: "本机进程态（PID / CDP 端口 / 本地代理端口 / 运行时长，换机器即失效）", Permission: "env:read", Idempotent: true},
		// M3：真实启停（响应不含代理地址/账号/口令）。
		{Method: "POST", Path: "/api/v1/environments/{id}/start", Tag: "环境", Summary: "拉起该环境的浏览器窗口（CA 就绪 + CDP 连通后才提交 running，失败自动复位）", Permission: "env:launch"},
		{Method: "POST", Path: "/api/v1/environments/{id}/stop", Tag: "环境", Summary: "关闭该环境的浏览器窗口并把状态收回 idle（幂等）", Permission: "env:stop"},

		{Method: "GET", Path: "/api/v1/logs", Tag: "日志", Summary: "聚合日志（audit/security/egress 三源归并，detail 默认不返回）", Permission: "audit:read"},
	}
	for _, op := range ops {
		openapi.Register(op)
	}
}
