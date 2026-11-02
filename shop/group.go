package shop

import (
	"context"
	"strings"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// GroupService 店铺分组（M2：把 shops.group_name 的自由文本分组升格为实体）。
//
// 三条硬规则（0012 的设计前提，服务层必须各自落实一次）：
//  1. 分组改名必须**同事务**同步成员店铺的 group_name（历史列还在用，
//     不同步就会出现「列表按分组筛出来、卡片上却写着旧组名」）；
//  2. 只有空分组能删（判据是实时 COUNT，不是缓存列）；
//  3. 分组与成员都带 tenant_id，任何读写都必须带租户上下文（RLS fail-closed）。
type GroupService struct {
	st     store.Store
	bus    *eventbus.Bus
	engine *iam.Engine
}

// NewGroupService 构造分组服务。
func NewGroupService(st store.Store, bus *eventbus.Bus, engine *iam.Engine) *GroupService {
	return &GroupService{st: st, bus: bus, engine: engine}
}

// GroupCreateInput 新建分组入参。
type GroupCreateInput struct {
	Name     string
	Sort     int
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Create 新建分组。
func (s *GroupService) Create(ctx context.Context, in GroupCreateInput) (*types.Group, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "group:manage", Path: "/groups"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权维护分组（" + string(d.Layer) + "）")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, apierr.Validation("分组名称需为 1-64 个字符")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	now := time.Now().UTC()
	g := &types.Group{
		ID: ids.New(), TenantID: in.Actor.TenantID, Name: name, Sort: in.Sort,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Groups().Create(tctx, g); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "group.created", OccurredAt: now,
			Actor: actorOf(in.Actor), Terminal: in.Terminal,
			Context: eventbus.ContextInfo{URL: in.URL, Action: "group.create", Result: "success"},
			Data:    map[string]any{"group_id": g.ID, "name": g.Name},
		})
	}); err != nil {
		return nil, err
	}
	g.EnvironmentCount = 0
	return g, nil
}

// GroupUpdateInput 修改分组入参（改名 / 排序）。
type GroupUpdateInput struct {
	ID       string
	Name     string
	Sort     *int
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Update 改名 / 改排序。改名**同事务**同步成员店铺的 group_name（双写兼容旧列）。
func (s *GroupService) Update(ctx context.Context, in GroupUpdateInput) (*types.Group, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "group:manage", Path: "/groups"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权维护分组（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	g, err := s.st.Groups().Get(tctx, in.ID)
	if err != nil {
		return nil, err
	}
	renamed := false
	if name := strings.TrimSpace(in.Name); name != "" && name != g.Name {
		if len([]rune(name)) > 64 {
			return nil, apierr.Validation("分组名称需为 1-64 个字符")
		}
		g.Name = name
		renamed = true
	}
	if in.Sort != nil {
		g.Sort = *in.Sort
	}
	now := time.Now().UTC()
	g.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Groups().Update(tctx, g); err != nil {
			return err
		}
		// 改名同步：不写这一遍，店铺卡片的 group_name 会停在旧名字上，
		// 而列表筛选用的是 group_id——「筛得出来、看着不对」最难排查。
		synced := 0
		if renamed {
			members, _, lerr := tx.Shops().List(tctx, store.ShopFilter{GroupID: g.ID, Limit: 500})
			if lerr != nil {
				return lerr
			}
			for i := range members {
				sp := members[i]
				sp.Group = g.Name
				sp.GroupID = g.ID
				sp.UpdatedAt = now
				if uerr := tx.Shops().Update(tctx, &sp); uerr != nil {
					return uerr
				}
				synced++
			}
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "group.updated", OccurredAt: now,
			Actor: actorOf(in.Actor), Terminal: in.Terminal,
			Context: eventbus.ContextInfo{URL: in.URL, Action: "group.update", Result: "success"},
			Data: map[string]any{
				"group_id": g.ID, "name": g.Name, "renamed": renamed, "synced_shops": synced,
			},
		})
	}); err != nil {
		return nil, err
	}
	if n, cerr := s.st.Groups().CountMembers(tctx, g.ID); cerr == nil {
		g.EnvironmentCount = n
	}
	return g, nil
}

// Delete 删除分组：仅空分组可删（非空 409，判据是实时计数）。
func (s *GroupService) Delete(ctx context.Context, actor iam.Subject, id string, term eventbus.Terminal, url string) error {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "group:manage", Path: "/groups"})
	if !d.Allow {
		return apierr.Forbidden("无权维护分组（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	g, err := s.st.Groups().Get(tctx, id)
	if err != nil {
		return err
	}
	n, err := s.st.Groups().CountMembers(tctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return apierr.Conflict("分组「" + g.Name + "」下仍有 " + itoa(n) + " 个店铺，请先移出或改组")
	}
	now := time.Now().UTC()
	return s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Groups().Delete(tctx, id); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: actor.TenantID, Type: "group.deleted", OccurredAt: now,
			Actor: actorOf(actor), Terminal: term,
			Context: eventbus.ContextInfo{URL: url, Action: "group.delete", Result: "success"},
			Data:    map[string]any{"group_id": id, "name": g.Name},
		})
	})
}

// List 分组列表（带 environment_count，按 sort, created_at）。
func (s *GroupService) List(ctx context.Context, actor iam.Subject) ([]*types.Group, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "group:read", Path: "/groups"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看分组（" + string(d.Layer) + "）")
	}
	return s.st.Groups().List(store.WithTenant(ctx, actor.TenantID))
}

// Get 单条分组。
func (s *GroupService) Get(ctx context.Context, actor iam.Subject, id string) (*types.Group, error) {
	d := s.engine.Check(ctx, iam.Request{Subject: actor, Action: "group:read", Path: "/groups"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看分组（" + string(d.Layer) + "）")
	}
	return s.st.Groups().Get(store.WithTenant(ctx, actor.TenantID), id)
}

// ---------- 环境 / 店铺归属 ----------

// GroupMoveInput 把环境（其所属店铺）移动到某个分组。
type GroupMoveInput struct {
	EnvID string
	// GroupID 目标分组；空串 = 移出分组（未分组）。
	GroupID string
	Actor   iam.Subject
	// 权限口径：从环境入口进来用 env:update，从店铺入口进来用 shop:update。
	Action string
	Path   string
	Term   eventbus.Terminal
	URL    string
}

// MoveEnv 环境移动分组：解析环境 → 校验目标分组属于本租户 → 双写 group_id/group_name。
//
// 目标分组必须在本租户内（Groups().Get 走 RLS + tenant_id 谓词）：
// 传一个别家租户的 group_id 进来若只是「写不进去」还好，怕的是静默写成
// 一个指向不存在的分组——那会在店铺网格页表现为「这个店铺归到一个看不见的组」。
func (s *GroupService) MoveEnv(ctx context.Context, in GroupMoveInput) (*types.Shop, error) {
	action := in.Action
	if action == "" {
		action = "env:update"
	}
	path := in.Path
	if path == "" {
		path = "/environments"
	}
	d := s.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: action, Path: path})
	if !d.Allow {
		return nil, apierr.Forbidden("无权变更分组归属（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	env, err := s.st.Environments().Get(tctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	sp, err := s.st.Shops().Get(tctx, env.ShopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(s.st, tctx, d, in.Actor, sp, "变更他人店铺的分组"); err != nil {
		return nil, err
	}
	groupID := ""
	groupName := ""
	if in.GroupID != "" {
		g, gerr := s.st.Groups().Get(tctx, in.GroupID)
		if gerr != nil {
			return nil, gerr // 404：分组不存在或不属于本租户
		}
		groupID, groupName = g.ID, g.Name
	}
	now := time.Now().UTC()
	sp.GroupID = groupID
	sp.Group = groupName
	sp.UpdatedAt = now
	if err := s.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Shops().Update(tctx, sp); err != nil {
			return err
		}
		return s.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "env.group_moved", OccurredAt: now,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: sp.ID},
			Terminal: in.Term,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "env.group.move", Result: "success"},
			Data: map[string]any{
				"env_id": env.ID, "shop_id": sp.ID,
				"group_id": groupID, "group_name": groupName,
			},
		})
	}); err != nil {
		return nil, err
	}
	return sp, nil
}
