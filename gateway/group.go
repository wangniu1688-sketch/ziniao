package gateway

import (
	"net/http"

	"ziniao/platform/apierr"
	"ziniao/shop"
)

// routesGroups M2 店铺分组。
func (s *Server) routesGroups() {
	m := s.mux
	m.HandleFunc("GET /api/v1/groups", s.authed(s.handleGroupList))
	m.HandleFunc("POST /api/v1/groups", s.authed(s.handleGroupCreate))
	m.HandleFunc("PUT /api/v1/groups/{id}", s.authed(s.handleGroupUpdate))
	m.HandleFunc("DELETE /api/v1/groups/{id}", s.authed(s.handleGroupDelete))
	// 环境移动分组（店铺网格页「改组」入口）
	m.HandleFunc("PUT /api/v1/environments/{id}/group", s.authed(s.handleEnvGroup))
}

// ---------- GET /api/v1/groups ----------

func (s *Server) handleGroupList(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "group:read", "/groups")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Groups == nil {
		s.notWired(w)
		return
	}
	list, err := s.deps.Groups.List(r.Context(), p2subject(p, subj))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": len(list)})
}

// ---------- POST /api/v1/groups ----------

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "group:manage", "/groups")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Groups == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name string `json:"name"`
		Sort int    `json:"sort"`
		URL  string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	g, err := s.deps.Groups.Create(r.Context(), shop.GroupCreateInput{
		Name: body.Name, Sort: body.Sort,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// ---------- PUT /api/v1/groups/{id} ----------

func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "group:manage", "/groups")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Groups == nil {
		s.notWired(w)
		return
	}
	var body struct {
		Name string `json:"name"`
		Sort *int   `json:"sort"`
		URL  string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	g, err := s.deps.Groups.Update(r.Context(), shop.GroupUpdateInput{
		ID: r.PathValue("id"), Name: body.Name, Sort: body.Sort,
		Actor: p2subject(p, subj), Terminal: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// ---------- DELETE /api/v1/groups/{id} ----------

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "group:manage", "/groups")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Groups == nil {
		s.notWired(w)
		return
	}
	if err := s.deps.Groups.Delete(r.Context(), p2subject(p, subj), r.PathValue("id"),
		terminalFrom(r), "/groups"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "deleted": true})
}

// ---------- PUT /api/v1/environments/{id}/group ----------

// handleEnvGroup 环境移动分组（group_id 为空串 = 移出分组）。
//
// 与旧接口 PUT /api/v1/shops/{id}/group 并存：旧接口改的是文本列（group_name），
// 新接口以 group_id 为准并双写 group_name，两者都不会留下「只有一半字段」的行。
func (s *Server) handleEnvGroup(w http.ResponseWriter, r *http.Request) {
	p, subj, _, err := s.authorize(r, "env:update", "/environments")
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.deps.Groups == nil {
		s.notWired(w)
		return
	}
	var body struct {
		GroupID string `json:"group_id"`
		URL     string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, apierr.Validation("请求体格式错误"))
		return
	}
	sp, err := s.deps.Groups.MoveEnv(r.Context(), shop.GroupMoveInput{
		EnvID: r.PathValue("id"), GroupID: body.GroupID,
		Actor: p2subject(p, subj), Action: "env:update", Path: "/environments",
		Term: terminalFrom(r), URL: body.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shop_id": sp.ID, "group_id": sp.GroupID, "group_name": sp.Group,
	})
}
