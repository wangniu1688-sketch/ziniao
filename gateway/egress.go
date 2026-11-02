package gateway

import (
	"net/http"
	"time"

	"ziniao/platform/apierr"
	"ziniao/platform/store"
)

// routesEgress 出口路由与出口历史的可观测接口（只读）。
func (s *Server) routesEgress() {
	s.mux.HandleFunc("GET /api/v1/egress/vendors", s.authed(s.handleEgressVendors))
	s.mux.HandleFunc("GET /api/v1/egress/history", s.authed(s.handleEgressHistory))
	s.mux.HandleFunc("GET /api/v1/egress/asn-sharing", s.authed(s.handleEgressASNSharing))
}

// handleEgressVendors 返回多代理商路由表与当前占用。
//
// 为什么要有这个接口：多代理商之后「这个店铺的出口到底走的哪家」成了排障第一问。
// 没有可见性时，运营只能靠猜——而猜错方向（比如以为已经切到备用线路）比不知道更糟。
//
// 只读且不返回任何凭据：供应商名单是采购信息，所以按 egress:read 裁决，
// 普通运营与代运营方看不到。
func (s *Server) handleEgressVendors(w http.ResponseWriter, r *http.Request) {
	if _, _, _, err := s.authorize(r, "egress:read", "/system"); err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Environments == nil {
		s.notWired(w)
		return
	}
	pool := s.deps.Environments.IPPool()
	if pool == nil {
		// 未接代理商不等于错误：单租户/未采购出口时本来就没有路由表可看。
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "vendors": []any{}, "stats": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"vendors": pool.Router().Snapshot(),
		"stats":   pool.Stats(),
	})
}

// handleEgressHistory 出口绑定历史（倒序时间线）。
//
// 「当前绑定」回答不了历史问题：一次释放就把证据抹掉了。而风控要问的恰恰是
// 「这两个店铺**先后**有没有用过同一 ASN / 同一出口」——只能查历史。
func (s *Server) handleEgressHistory(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "egress:read", "/system")
	if err != nil {
		writeErr(w, err)
		return
	}
	f := store.EgressHistoryFilter{
		EnvID:    r.URL.Query().Get("env_id"),
		Provider: r.URL.Query().Get("provider"),
		ASN:      r.URL.Query().Get("asn"),
		Host:     r.URL.Query().Get("host"),
		Action:   r.URL.Query().Get("action"),
		Limit:    queryInt(r, "limit", 50),
		Offset:   queryInt(r, "offset", 0),
	}
	if v := r.URL.Query().Get("from"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			// 静默忽略非法时间会让人以为筛过了：结果是「看起来什么都没发生」。
			writeErr(w, apierr.Validation("from 必须是 RFC3339 时间"))
			return
		}
		f.From = &t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			writeErr(w, apierr.Validation("to 必须是 RFC3339 时间"))
			return
		}
		f.To = &t
	}
	if f.Action != "" && !validEgressAction(f.Action) {
		writeErr(w, apierr.Validation("action 只能是 bound/rebound/released/expired/dropped"))
		return
	}
	list, total, err := s.deps.Store.EgressBindings().
		ListEvents(store.WithTenant(r.Context(), p.TenantID), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total})
}

// handleEgressASNSharing 找出「同一 ASN 被多个环境用过」的组合。
//
// 这是关联线索，不是判决：住宅代理商把同一 ASN 下的不同 IP 卖给同一租户的不同店铺
// 属于正常商业行为，平台却可能据此把两个账号串起来。让运营看见，比让系统假装没有更安全。
func (s *Server) handleEgressASNSharing(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := s.authorize(r, "egress:read", "/system")
	if err != nil {
		writeErr(w, err)
		return
	}
	days := queryInt(r, "days", 30)
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	minEnvs := queryInt(r, "min_envs", 2)
	if minEnvs < 2 {
		minEnvs = 2
	}
	since := time.Now().UTC().AddDate(0, 0, -days)
	items, err := s.deps.Store.EgressBindings().
		ASNSharing(store.WithTenant(r.Context(), p.TenantID), since, minEnvs)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": len(items),
		"since": since, "days": days, "min_envs": minEnvs,
	})
}

func validEgressAction(a string) bool {
	switch a {
	case "bound", "rebound", "released", "expired", "dropped":
		return true
	}
	return false
}
