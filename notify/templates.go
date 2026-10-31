package notify

// Template 通知模板（统一模板管理，各通道共用）。
type Template struct {
	Title string
	Body  string
}

var templates = map[string]Template{
	"auth.login_failed": {"登录失败告警", "检测到账号登录失败，请确认是否为本人操作"},
	"auth.login":        {"新终端登录提醒", "账号在新终端登录，如非本人操作请立即冻结账号"},
	"shop.created":      {"店铺已创建", "新建店铺操作已完成"},
	"shop.deleted":      {"高危操作：店铺删除", "店铺已被删除，请确认操作合规"},
	"iam.role.granted":  {"权限变更", "成员角色发生变更"},
	"iam.role.revoked":  {"权限回收", "成员角色已被回收"},
	"member.invited":    {"新成员加入", "团队成员发生变化"},
	"risk.alert":        {"风险告警", "系统检测到风险事件"},
	"usage.recorded":    {"用量提醒", "资源用量发生变化"},

	// 审批待办：由 approval 服务算好的合格审批人定向推送（见 approverIDsOf），
	// 避免全团队都收到、大多数人点进去才发现无权处理。
	"iam.approval.created":  {"待办：审批单待处理", "有一条审批单等待你处理，请尽快裁决"},
	"iam.approval.signed":   {"审批进度：已签署", "审批单收到一次签署，仍需剩余签署人确认"},
	"iam.approval.approved": {"审批结果：已通过", "你关注的审批单已批准"},
	"iam.approval.rejected": {"审批结果：已驳回", "你关注的审批单已被驳回"},
	"iam.approval.expired":  {"审批结果：已过期", "审批单超时未处理，已自动失效"},
	"iam.approval.used":     {"高危提示：审批单已核销", "审批单已被用于高危操作"},
}

// TemplateOf 取模板，未命中返回通用模板。
func TemplateOf(eventType string) Template {
	if t, ok := templates[eventType]; ok {
		return t
	}
	return Template{Title: "系统通知", Body: "事件类型: " + eventType}
}
