package main

import (
	"context"
	"log/slog"
	"time"

	"ziniao/app"
	"ziniao/device"
	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/store"
	"ziniao/platform/types"
	"ziniao/security"
	"ziniao/shop"
	"ziniao/tenant"
)

// demoPassword 需满足配置中心的口令最小长度（默认 10 位）。
const demoPassword = "Zn@12345678"

// seed 写入演示数据：一个团队 + 老板/财务/运营三名成员 + 两家店铺。
// 用途：Phase 0 验收演示（建租户 → 建用户 → 授角色 → 一次带审计的写操作）。
func seed(ctx context.Context, a *app.App, log *slog.Logger) error {
	st := a.Store
	if _, err := st.Tenants().GetBySlug(ctx, "demo"); err == nil {
		log.Info("演示数据已存在，跳过")
		return nil
	}
	term := eventbus.Terminal{IP: "127.0.0.1", UserAgent: "seed", Mode: "cli", ClientVer: "phase0"}
	res, err := a.Tenant.Create(ctx, tenant.CreateInput{
		Name: "紫鸟演示团队", Slug: "demo", PlanCode: "pro",
		Owner: tenant.OwnerInput{
			Username: "demo", Password: demoPassword,
			Email: "demo@ziniao.local", Nickname: "老板老王",
		},
		Terminal: term,
	})
	if err != nil {
		return err
	}

	// 财务成员：能看财务页，但利润/成本/余额在服务端就被遮蔽
	finance, err := a.Tenant.InviteMember(ctx, tenant.InviteInput{
		TenantID: res.Tenant.ID, Username: "finance", Password: demoPassword,
		Nickname: "财务小李", InvitedBy: res.User.ID, Terminal: term,
	})
	if err != nil {
		return err
	}
	if err := a.Tenant.GrantRole(ctx, tenant.GrantInput{
		TenantID: res.Tenant.ID, UserID: finance.ID, RoleCode: "finance",
		GrantedBy: res.User.ID, Terminal: term,
	}); err != nil {
		return err
	}

	// 运营成员：数据范围 own；再叠加运营主管以演示多角色并集
	operator, err := a.Tenant.InviteMember(ctx, tenant.InviteInput{
		TenantID: res.Tenant.ID, Username: "operator", Password: demoPassword,
		Nickname: "运营小张", InvitedBy: res.User.ID, Terminal: term,
	})
	if err != nil {
		return err
	}
	for _, code := range []string{"operator", "ops_manager"} {
		if err := a.Tenant.GrantRole(ctx, tenant.GrantInput{
			TenantID: res.Tenant.ID, UserID: operator.ID, RoleCode: code,
			GrantedBy: res.User.ID, Terminal: term,
		}); err != nil {
			return err
		}
	}

	ownerSubject := iam.Subject{
		UserID: res.User.ID, TenantID: res.Tenant.ID, Username: res.User.Username,
		Roles: []types.Role{*res.Role}, Now: time.Now(),
	}
	tctx := store.WithTenant(ctx, res.Tenant.ID)
	shops := []*types.Shop{}
	for _, s := range []struct{ name, platform, site string }{
		{"亚马逊美国站01", "Amazon", "US"},
		{"Shopee 马来站02", "Shopee", "MY"},
	} {
		sp, err := a.Shop.Create(tctx, shop.CreateInput{
			Name: s.name, Platform: s.platform, Site: s.site, Status: "正常运营",
			Actor: ownerSubject, Terminal: term, URL: "/shops",
		})
		if err != nil {
			return err
		}
		shops = append(shops, sp)
	}

	// Phase 1 演示数据：云网络设备 + 绑定 + 店铺环境 + 凭证托管 + 访问策略
	seedPhase1(a, tctx, shops, ownerSubject, term, log)

	log.Info("演示数据已写入",
		"owner", "demo/"+demoPassword,
		"finance", "finance/"+demoPassword,
		"operator", "operator/"+demoPassword,
		"tenant", res.Tenant.ID)
	return nil
}

// seedPhase1 写入 Phase 1 演示数据：设备、绑定、环境、托管凭证、访问策略。
// 任一环节失败只告警不中断——演示数据不是核心链路，不应让启动失败。
func seedPhase1(a *app.App, ctx context.Context, shops []*types.Shop,
	actor iam.Subject, term eventbus.Terminal, log *slog.Logger) {
	if a.Device == nil || a.Environments == nil || a.Vault == nil || a.Security == nil {
		log.Warn("Phase 1 依赖未装配，跳过演示数据")
		return
	}
	dev, err := a.Device.Purchase(ctx, device.PurchaseInput{
		Name: "美国住宅-洛杉矶", Country: "US", Region: "California", City: "Los Angeles",
		PeriodMonths: 1, Actor: actor, Terminal: term, URL: "/devices",
	})
	if err != nil {
		log.Warn("演示设备购买失败", "err", err)
		return
	}
	if len(shops) == 0 {
		return
	}
	if _, err := a.Device.Bind(ctx, device.BindInput{
		DeviceID: dev.ID, ShopID: shops[0].ID, Actor: actor, Terminal: term, URL: "/devices",
	}); err != nil {
		log.Warn("演示设备绑定失败", "err", err)
	}
	if _, _, err := a.Environments.CreateEnv(ctx, shop.CreateEnvInput{
		ShopID: shops[0].ID, Country: "US", DeviceID: dev.ID,
		Actor: actor, Terminal: term, URL: "/shops",
	}); err != nil {
		log.Warn("演示环境创建失败", "err", err)
	}
	if err := a.Vault.Put(ctx, shop.PutInput{
		ShopID: shops[0].ID, Kind: types.CredKindPassword, Username: "seller-demo",
		Secret: demoPassword, Actor: actor, Terminal: term, URL: "/shops",
	}); err != nil {
		log.Warn("演示凭证写入失败", "err", err)
	}
	tpl := security.TemplateOf("high_risk_ops")
	if tpl != nil {
		if _, err := a.Security.Upsert(ctx, security.UpsertInput{
			Name: tpl.Name, Scope: "shop", ScopeID: shops[0].ID, Template: tpl.Code,
			Enabled: true, Rules: tpl.Rules, Actor: actor, Terminal: term, URL: "/security",
		}); err != nil {
			log.Warn("演示策略写入失败", "err", err)
		}
	}
	log.Info("Phase 1 演示数据已写入", "device", dev.ID, "shop", shops[0].ID)
}
