package iam

import (
	"context"
	"testing"
	"time"

	"ziniao/platform/config"
	"ziniao/platform/types"
)

// TestRequiredPageCoversM2Sections M2 终审 T15：代理台账 / 分组 / 日志 / 环境四处页面路径必须登记，
// 且登记用的 page:* 码必须真实存在于权限目录（否则登记等于写了个永远拒绝的死码）。
func TestRequiredPageCoversM2Sections(t *testing.T) {
	want := map[string]string{
		"/proxies":      "page:proxies",
		"/groups":       "page:groups",
		"/logs":         "page:logs",
		"/environments": "page:environments",
	}
	for path, code := range want {
		got, ok := RequiredPage[path]
		if !ok {
			t.Fatalf("RequiredPage 未登记 %s", path)
		}
		if got != code {
			t.Fatalf("%s 页面码不符: 期望 %s，实际 %s", path, code, got)
		}
		if _, ok := Lookup(code); !ok {
			t.Fatalf("页面码 %s 不在权限目录 Catalog 中", code)
		}
	}
	// 导航清单的可见性码同样必须在目录里，否则前端会整栏消失。
	for _, s := range NavSections {
		for _, key := range []string{s.Perm, s.Page} {
			if key == "" {
				continue
			}
			if _, ok := Lookup(key); !ok {
				t.Fatalf("导航项 %s 的可见性码 %s 不在目录中", s.ID, key)
			}
		}
	}
}

// TestPageCodeAbsentDoesNotBlockResourceDecision 双层口径第 1 条：
// 资源接口由资源/功能码裁决，缺 page:* 不得把 200 变 403。
//
// 反例（必须锁死）：cs / agency_ops 持有 env:read 但没有 page:environments，
// 若页面层也拦，就会出现「给了只读码又被页面层收回」的自相矛盾。
func TestPageCodeAbsentDoesNotBlockResourceDecision(t *testing.T) {
	engine := NewEngine(config.Default())
	ctx := context.Background()

	cases := []struct {
		name   string
		roles  []string
		action string
		path   string
	}{
		{name: "cs 只读环境（无 page:environments）", roles: []string{"cs"}, action: "env:read", path: "/environments"},
		{name: "agency_ops 只读环境（无 page:environments）", roles: []string{"agency_ops"}, action: "env:read", path: "/environments"},
		{name: "cs 只读分组", roles: []string{"cs"}, action: "group:read", path: "/groups"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Check(ctx, Request{
				Subject: Subject{Roles: roleSet(t, tc.roles...), Now: time.Now()},
				Action:  tc.action, Path: tc.path,
			})
			if !d.Allow {
				t.Fatalf("%s 应放行（资源码为准），实际被 %s 层拒绝：%s", tc.name, d.Layer, d.Reason)
			}
		})
	}

	// 反向：只有 page:* 而没有资源码，仍然必须被功能层拒绝——页面码不单独放行。
	pageOnly := []types.Role{{ID: "custom", Code: "page_only", Permissions: []string{"page:proxies"}}}
	d := engine.Check(ctx, Request{
		Subject: Subject{Roles: pageOnly, Now: time.Now()},
		Action:  "proxy:read", Path: "/proxies",
	})
	if d.Allow {
		t.Fatal("仅持有 page:proxies 而无 proxy:read 时应拒绝")
	}
	if d.Layer != LayerFunction {
		t.Fatalf("拒绝层级应为 function（资源码才是闸门），实际 %s", d.Layer)
	}
}

// TestPageLayerGatesPureNavigation 双层口径第 2 条：不带 Action 的纯页面导航裁决由 page:* 把关。
func TestPageLayerGatesPureNavigation(t *testing.T) {
	engine := NewEngine(config.Default())
	ctx := context.Background()

	d := engine.Check(ctx, Request{
		Subject: Subject{Roles: roleSet(t, "finance"), Now: time.Now()},
		Path:    "/proxies",
	})
	if d.Allow {
		t.Fatal("财务角色不应能进入代理台账页")
	}
	if d.Layer != LayerPage {
		t.Fatalf("纯页面导航拒绝应由页面层给出，实际 %s（%s）", d.Layer, d.Reason)
	}

	ok := engine.Check(ctx, Request{
		Subject: Subject{Roles: roleSet(t, "operator"), Now: time.Now()},
		Path:    "/proxies",
	})
	if !ok.Allow {
		t.Fatalf("运营应能进入代理台账页，实际 %s", ok.Reason)
	}
}

// TestNavVisibilityFollowsResourceCode 双层口径第 3 条：资源码被拒 ⇒ 对应导航项不可见。
func TestNavVisibilityFollowsResourceCode(t *testing.T) {
	vis := func(roles []string) map[string]bool {
		out := map[string]bool{}
		for _, it := range NavFor(roleSet(t, roles...)) {
			out[it.ID] = it.Visible
		}
		return out
	}

	finance := vis([]string{"finance"})
	for _, id := range []string{"proxies", "groups", "logs", "devices"} {
		if finance[id] {
			t.Fatalf("财务角色（无对应资源码）的 %s 导航项不应可见", id)
		}
	}
	if !finance["shops"] {
		t.Fatal("财务持有 shop:read，店铺管理应可见")
	}

	operator := vis([]string{"operator"})
	for _, id := range []string{"shops", "proxies", "groups", "devices"} {
		if !operator[id] {
			t.Fatalf("运营的 %s 导航项应可见", id)
		}
	}
	// 运营持有 page:logs 但没有 audit:read —— 日志中心接口一律 403，
	// 按「资源码被拒 ⇒ 导航不可见」的口径，这里必须隐藏，不能留下点了就报错的死入口。
	if operator["logs"] {
		t.Fatal("运营缺 audit:read，日志中心导航项不应可见")
	}
	for _, id := range []string{"dashboard", "settings"} {
		if !operator[id] {
			t.Fatalf("运营的 %s 应恒可见", id)
		}
	}

	cs := vis([]string{"cs"})
	if !cs["groups"] {
		t.Fatal("客服持有 group:read，分组管理应可见")
	}
	if cs["proxies"] || cs["logs"] {
		t.Fatalf("客服不应看到代理/日志导航: %+v", cs)
	}

	none := vis(nil)
	if !none["dashboard"] || !none["settings"] {
		t.Fatal("工作台与系统设置应恒可见")
	}
	for _, id := range []string{"shops", "proxies", "groups", "logs", "devices"} {
		if none[id] {
			t.Fatalf("无角色成员不应看到 %s 导航", id)
		}
	}
}
