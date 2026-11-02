package iam

import (
	"context"
	"sort"
	"strings"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/types"
)

// Subject 权限裁决主体：必须携带「发起人」身份（L3：自动化不得成为权限旁路）。
type Subject struct {
	UserID          string
	TenantID        string
	Username        string
	Roles           []types.Role
	Grants          []types.DataGrant
	Plan            *config.PlanSpec
	IP              string
	TerminalTrusted bool
	MFAVerified     bool
	Now             time.Time
}

// Request 裁决请求。
type Request struct {
	Subject      Subject
	Action       string // 功能级权限 key
	ResourceType string // 资源级：device|box_purchase...
	ResourceID   string
	Path         string // 页面/元素级裁决所需
}

// Decision 裁决结果。
type Decision struct {
	Allow        bool     `json:"allow"`
	Reason       string   `json:"reason,omitempty"`
	Layer        Layer    `json:"layer,omitempty"`
	MaskFields   []string `json:"mask_fields,omitempty"`   // 字段级遮蔽
	DenyElements []string `json:"deny_elements,omitempty"` // 元素级屏蔽（权限 key）
	DataScope    string   `json:"data_scope,omitempty"`    // none|own|team|all
	Obligations  []string `json:"obligations,omitempty"`   // require_mfa|require_approval|watermark
}

// DOMPolicy 下发给客户端注入层的策略（L6 第三层：DOM 注入层的字段遮蔽与元素屏蔽）。
type DOMPolicy struct {
	Path          string         `json:"path"`
	MaskFields    []string       `json:"mask_fields"`
	MaskSelectors []string       `json:"mask_selectors"`
	DenySelectors []string       `json:"deny_selectors"`
	Watermark     *WatermarkSpec `json:"watermark,omitempty"`
}

// WatermarkSpec 屏幕水印规格（M12：防泄密溯源）。
type WatermarkSpec struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
}

// Engine 策略引擎。
type Engine struct {
	cfg *config.Config
}

// NewEngine 构造策略引擎。
func NewEngine(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// Check 五层裁决：功能 → 页面 → 元素 → 数据 → 资源。
// 返回拒绝时 Layer 标明被拒层级，Reason 供审计（不向客户端泄露内部细节）。
func (e *Engine) Check(_ context.Context, req Request) Decision {
	subj := req.Subject
	perms := unionPermissions(subj.Roles)

	d := Decision{
		DataScope:  maxDataScope(subj.Roles),
		MaskFields: unionMaskFields(subj.Roles),
	}

	// 1) 功能级
	if req.Action != "" {
		if !perms[req.Action] {
			d.Allow = false
			d.Layer = LayerFunction
			d.Reason = "missing_function_permission:" + req.Action
			return d
		}
		// 2) 资源级：功能权限通过后，还要有资源授权（如盒子购买权）
		if req.ResourceType != "" && !hasResourceGrant(subj.Grants, req.ResourceType, req.ResourceID, subj.Now) {
			d.Allow = false
			d.Layer = LayerResource
			d.Reason = "missing_resource_grant:" + req.ResourceType
			return d
		}
		// 高危动作的义务：二次审批 / MFA
		if p, ok := Lookup(req.Action); ok {
			if p.Approval {
				d.Obligations = append(d.Obligations, "require_approval")
			}
			if p.Risk == "high" && !subj.MFAVerified {
				d.Obligations = append(d.Obligations, "require_mfa")
			}
		}
	}

	// 3) 页面级 + 元素级（按路径计算 DOM 屏蔽清单）
	//
	// 双层口径（M2 终审 T15）：
	//   - 带 Action 的裁决 = 资源接口调用，闸门已经由第 1 步的功能/资源码定完，
	//     页面码**不再二次拦截**——否则会出现"环境只读码给了 cs/agency_ops，
	//     却因缺 page:environments 被页面层拦掉"的自相矛盾；
	//   - 不带 Action 的裁决 = 纯页面导航，此时由 page:* 单独把关。
	// 两种情况下元素级屏蔽清单照常计算，不受影响。
	if req.Path != "" {
		if req.Action == "" {
			if pagePerm, ok := RequiredPage[req.Path]; ok && !perms[pagePerm] {
				d.Allow = false
				d.Layer = LayerPage
				d.Reason = "missing_page_permission:" + pagePerm
				return d
			}
		}
		for _, el := range PageElements[req.Path] {
			if !perms[el] {
				d.DenyElements = append(d.DenyElements, el)
			}
		}
	}

	d.Allow = true
	return d
}

// DOMPolicy 生成客户端注入层策略：遮蔽字段 + 屏蔽元素 + 水印。
// 这是 L6 的第三层，与第一层（网关）、第二层（服务层）必须同时存在。
func (e *Engine) DOMPolicy(_ context.Context, subj Subject, path string) DOMPolicy {
	perms := unionPermissions(subj.Roles)
	p := DOMPolicy{Path: path}
	for _, f := range unionMaskFields(subj.Roles) {
		p.MaskFields = append(p.MaskFields, f)
		p.MaskSelectors = append(p.MaskSelectors, FieldSelector(f))
	}
	for _, el := range PageElements[path] {
		if !perms[el] {
			p.DenySelectors = append(p.DenySelectors, ElementSelector(el))
		}
	}
	if e.cfg != nil && e.cfg.Limits.ExportWatermarkRequired {
		p.Watermark = &WatermarkSpec{Enabled: true, Text: subj.Username + " " + subj.IP}
	}
	return p
}

func unionPermissions(roles []types.Role) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		for _, p := range r.Permissions {
			out[p] = true
		}
	}
	return out
}

func unionMaskFields(roles []types.Role) []string {
	set := map[string]bool{}
	for _, r := range roles {
		for _, f := range r.MaskFields {
			set[f] = true
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

var scopeRank = map[string]int{"none": 0, "own": 1, "team": 2, "all": 3}

func maxDataScope(roles []types.Role) string {
	best := "none"
	for _, r := range roles {
		if r.DataScope == "" {
			continue
		}
		if scopeRank[r.DataScope] > scopeRank[best] {
			best = r.DataScope
		}
	}
	return best
}

// hasResourceGrant 资源级授权判定；"*" 表示全部；带 ExpiresAt 的临时授权到期自动失效。
func hasResourceGrant(grants []types.DataGrant, resourceType, resourceID string, now time.Time) bool {
	for _, g := range grants {
		if g.ResourceType != resourceType {
			continue
		}
		if g.ResourceID != "*" && g.ResourceID != resourceID {
			continue
		}
		if g.ExpiresAt != nil && g.ExpiresAt.Before(now) {
			continue
		}
		return true
	}
	return false
}

// NormalizePath 把前端路由归一化为策略路径（首尾斜杠、去查询串）。
func NormalizePath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}
