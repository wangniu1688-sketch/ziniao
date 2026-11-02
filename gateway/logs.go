package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// routesLogs M2 聚合日志（审计 + 安全事件 + 出口历史，读侧聚合，不新增表）。
func (s *Server) routesLogs() {
	s.mux.HandleFunc("GET /api/v1/logs", s.authed(s.handleLogs))
}

// logItem 聚合日志条目（三源统一形状）。
//
// detail 默认不返回：审计与安全事件的 detail 是自由 JSON，里面可能有上游地址、
// 手机号一类东西；日志中心页面会被截图外传，默认只给「看得懂发生了什么」的字段，
// 需要细节时显式带 include_detail=1 且服务端再过一遍遮蔽。
type logItem struct {
	ID         string         `json:"id"`
	Source     string         `json:"source"`
	OccurredAt time.Time      `json:"occurred_at"`
	Category   string         `json:"category,omitempty"`
	Level      string         `json:"level"`
	Actor      string         `json:"actor,omitempty"`
	ShopID     string         `json:"shop_id,omitempty"`
	EnvID      string         `json:"env_id,omitempty"`
	Action     string         `json:"action"`
	Summary    string         `json:"summary"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// sensitiveKeys include_detail=1 时必须剔除的凭据键（不区分大小写、子串匹配）。
//
// 覆盖代理用户名/口令、令牌、Cookie、Authorization、密钥与私钥；子串匹配让
// access_token / client_secret / proxy_authorization / set_cookie / private-key
// 这些变体一并命中。清单只增不减：detail 是自由 JSON，宁可少给一项。
var sensitiveKeys = []string{
	"password", "passwd", "pwd", // 口令
	"username", "user_name", "login", // 代理用户名 / 登录名
	"secret", "token", "credential", // 令牌与凭据
	"api_key", "apikey", "access_key", "app_key", // 各类密钥
	"private_key", "privatekey", "private-key", // 私钥
	"cookie", "authorization", "auth", "bearer", "session", // 会话与鉴权头
}

func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range sensitiveKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

// maskMap 三源统一的 detail 过滤：剔除敏感键，嵌套对象递归过滤。
func maskMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if sensitiveKey(k) {
			continue
		}
		out[k] = redactValue(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// redactValue 值层面只做两件事：嵌套结构递归；URL 里带 userinfo
// （scheme://user:pass@host）整段打码。不做宽泛的口令字符串猜测，
// 那会把正常的 URL / 备注打码，反而掩盖排障线索。
func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return maskMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = redactValue(x[i])
		}
		return out
	case string:
		if hasURLUserInfo(x) {
			return "[redacted]"
		}
		return x
	default:
		return v
	}
}

func hasURLUserInfo(s string) bool {
	i := strings.Index(s, "://")
	if i < 0 {
		return false
	}
	authority := s[i+3:]
	if j := strings.IndexByte(authority, '/'); j >= 0 {
		authority = authority[:j]
	}
	at := strings.LastIndexByte(authority, '@')
	return at > 0 && strings.Contains(authority[:at], ":")
}

// maskDetail audit / security 事件的 detail 是 JSON 裸串，解析失败整段丢弃。
func maskDetail(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	in := map[string]any{}
	if err := json.Unmarshal(raw, &in); err != nil {
		// 宁可少给，也不能把一段来源不明的内容渲染到页面上。
		return nil
	}
	return maskMap(in)
}

// handleLogs 聚合日志查询。
//
// 权限是**叠加**的（缺权直接 403，不静默过滤）：
// 静默过滤会让「为什么少了一半日志」无从解释——运营看到的是一份看起来完整的列表，
// 而其中一类事件从来没出现过。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "audit:read", "/logs")
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	sources := parseSources(q.Get("sources"))
	if len(sources) == 0 {
		writeErr(w, apierr.Validation("sources 只能是 audit / security / egress 的组合"))
		return
	}
	for _, src := range sources {
		switch src {
		case "audit":
		case "security":
			if _, _, _, err := s.authorize(r, "security:event.read", "/logs"); err != nil {
				writeErr(w, err)
				return
			}
		case "egress":
			if _, _, _, err := s.authorize(r, "egress:read", "/logs"); err != nil {
				writeErr(w, err)
				return
			}
		default:
			writeErr(w, apierr.Validation("sources 只能是 audit / security / egress 的组合"))
			return
		}
	}
	limit := queryInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		writeErr(w, apierr.Validation("limit 需在 1-200 之间"))
		return
	}
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		writeErr(w, apierr.Validation("offset 不能为负"))
		return
	}
	var from, to *time.Time
	if v := q.Get("from"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			writeErr(w, apierr.Validation("from 必须是 RFC3339 时间"))
			return
		}
		from = &t
	}
	if v := q.Get("to"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			writeErr(w, apierr.Validation("to 必须是 RFC3339 时间"))
			return
		}
		to = &t
	}
	// category 与 type 同义：前端两种叫法都在用，统一到一个筛选条件上，
	// 别让「按类型筛」出现两个都填才生效的怪现象。
	category := q.Get("category")
	if category == "" {
		category = q.Get("type")
	}
	level := strings.ToLower(q.Get("level"))
	shopID := q.Get("shop_id")
	envID := q.Get("env_id")
	actorID := q.Get("actor_id")
	action := q.Get("action")
	keyword := strings.ToLower(strings.TrimSpace(q.Get("keyword")))
	includeDetail := queryBool(r, "include_detail")

	// 三源各查同一窗口（limit+offset 条），再内存归并排序：
	// 跨源没法在 SQL 里做一次 ORDER BY，而「按时间倒序」是这个页面唯一的分页语义。
	window := limit + offset
	if window > 200 {
		window = 200
	}
	tctx := store.WithTenant(r.Context(), p.TenantID)
	merged := make([]logItem, 0, window*len(sources))
	total := 0
	for _, src := range sources {
		switch src {
		case "audit":
			items, n, aerr := s.deps.Audit.List(tctx, store.AuditFilter{
				Category:    types.AuditCategory(category),
				ActorUserID: actorID, ShopID: shopID, Action: action,
				From: from, To: to, Keyword: keyword, Limit: window, Offset: 0,
			})
			if aerr != nil {
				writeErr(w, aerr)
				return
			}
			total += n
			for _, e := range items {
				it := logItem{
					ID: e.ID, Source: "audit", OccurredAt: e.OccurredAt,
					Category: string(e.Category), Level: auditLevel(e.Result),
					Actor: e.ActorName, ShopID: e.ShopID, Action: e.Action,
					Summary: e.Action + " · " + e.Result,
				}
				if it.Actor == "" {
					it.Actor = e.ActorUserID
				}
				if includeDetail {
					it.Detail = maskDetail(e.Data)
				}
				merged = append(merged, it)
			}
		case "security":
			items, n, serr := s.deps.Security.ListEvents(r.Context(), p.TenantID, store.SecurityEventFilter{
				Category: category, Level: level, ShopID: shopID, From: from,
				Limit: window, Offset: 0,
			})
			if serr != nil {
				writeErr(w, serr)
				return
			}
			for _, e := range items {
				if !logFilterPass(action, keyword, to, e.CreatedAt, e.Action, e.Category, string(e.Detail)) {
					continue
				}
				it := logItem{
					ID: e.ID, Source: "security", OccurredAt: e.CreatedAt,
					Category: e.Category, Level: e.Level,
					Actor: e.ActorUserID, ShopID: e.ShopID, Action: e.Action,
					Summary: e.Action + " · " + e.Level,
				}
				if includeDetail {
					it.Detail = maskDetail(e.Detail)
				}
				merged = append(merged, it)
			}
			total += n
		case "egress":
			items, n, eerr := s.deps.Store.EgressBindings().ListEvents(tctx, store.EgressHistoryFilter{
				EnvID: envID, Action: action, From: from, To: to,
				Limit: window, Offset: 0,
			})
			if eerr != nil {
				writeErr(w, eerr)
				return
			}
			total += n
			for _, e := range items {
				summary := e.Action
				if e.ExitAddr != "" {
					summary += " · " + e.ExitAddr
				}
				it := logItem{
					ID: e.ID, Source: "egress", OccurredAt: e.OccurredAt,
					Category: "egress", Level: "info", EnvID: e.EnvID,
					Action: e.Action, Summary: summary,
				}
				if keyword != "" && !strings.Contains(strings.ToLower(summary+" "+e.Provider+" "+e.Reason), keyword) {
					continue
				}
				if includeDetail {
					// 固定白名单字段本身不含凭据，仍过一遍 maskMap：三源共用同一份
					// 遮蔽规则，以后往白名单加字段也不会绕过它。
					it.Detail = maskMap(map[string]any{
						"provider": e.Provider, "resource_id": e.ResourceID,
						"exit_addr": e.ExitAddr, "prev_exit": e.PrevExit,
						"reason": e.Reason, "asn": e.ASN, "country": e.Country,
					})
				}
				merged = append(merged, it)
			}
		}
	}
	sortLogItems(merged)
	if offset > 0 && offset < len(merged) {
		merged = merged[offset:]
	} else if offset >= len(merged) {
		merged = nil
	}
	if limit < len(merged) {
		merged = merged[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": merged, "total": total, "limit": limit, "offset": offset,
		"next_offset": nextOffset(offset, limit, total),
		"sources":     sources,
	})
}

// auditLevel 审计条目没有 level 字段，按 result 映射：
// 失败与拦截需要被运营看见，成功只是流水。
func auditLevel(result string) string {
	switch result {
	case "failure", "blocked":
		return "medium"
	}
	return "info"
}

// logFilterPass 安全事件的补充过滤（仓储过滤器的字段比聚合接口少）。
func logFilterPass(action, keyword string, to *time.Time, at time.Time, act, category, detail string) bool {
	if to != nil && at.After(*to) {
		return false
	}
	if action != "" && act != action {
		return false
	}
	if keyword != "" && !strings.Contains(strings.ToLower(act+" "+category+" "+detail), keyword) {
		return false
	}
	return true
}

// sortLogItems 倒序归并（时间相同按 ID 兜底，保证分页稳定）。
func sortLogItems(items []logItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			if b.OccurredAt.After(a.OccurredAt) || (b.OccurredAt.Equal(a.OccurredAt) && b.ID > a.ID) {
				items[j-1], items[j] = b, a
				continue
			}
			break
		}
	}
}

// parseSources 解析 ?sources=audit,security（空 = 默认审计 + 安全事件）。
func parseSources(v string) []string {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return []string{"audit", "security"}
	}
	out := make([]string, 0, 3)
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		s := strings.ToLower(strings.TrimSpace(part))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
