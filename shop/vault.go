package shop

import (
	"context"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/crypto"
	"ziniao/platform/ids"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// Vault 店铺账密托管（M2.2 / Q10）：
// 信封加密三层（KEK → DEK → 数据），密文含 key_version 以支持密钥轮换；
// 明文仅驻内存，永不进日志、永不进 API 响应（L4）。
type Vault struct {
	st     store.Store
	bus    *eventbus.Bus
	engine *iam.Engine
	cipher *crypto.EnvelopeCipher
}

// NewVault 构造凭证托管服务。
func NewVault(st store.Store, bus *eventbus.Bus, engine *iam.Engine, cipher *crypto.EnvelopeCipher) *Vault {
	return &Vault{st: st, bus: bus, engine: engine, cipher: cipher}
}

// PutInput 写入凭证入参（明文只在调用栈内）。
type PutInput struct {
	ShopID   string
	Kind     types.CredentialKind
	Username string
	Secret   string
	Actor    iam.Subject
	Terminal eventbus.Terminal
	URL      string
}

// Put 写入/更新托管凭证（写操作走事件总线落审计，L5）。
func (v *Vault) Put(ctx context.Context, in PutInput) error {
	d := v.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:credential.manage", Path: "/shops"})
	if !d.Allow {
		return apierr.Forbidden("无权维护店铺凭证（" + string(d.Layer) + "）")
	}
	// 数据级收口：own 范围下只能维护本人（或被委托）店铺的凭证
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	sp, err := v.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return err
	}
	if err := checkShopAccess(v.st, tctx, d, in.Actor, sp, "维护他人店铺的凭证"); err != nil {
		return err
	}
	if in.Secret == "" {
		return apierr.Validation("凭证内容不能为空")
	}
	enc, err := v.cipher.Seal(in.Secret)
	if err != nil {
		return apierr.Internal("凭证加密失败")
	}
	now := time.Now().UTC()
	cred := &types.ShopCredential{
		ID: ids.New(), TenantID: in.Actor.TenantID, ShopID: in.ShopID, Kind: in.Kind,
		Username: in.Username, SecretEnc: enc, Algo: "aes-256-gcm",
		KeyVersion: v.cipher.CurrentVersion(), UpdatedAt: now, UpdatedBy: in.Actor.UserID,
	}
	return v.st.WithTx(tctx, func(tx store.Tx) error {
		if err := tx.Vault().Upsert(tctx, cred); err != nil {
			return err
		}
		// L4：审计只记元信息，绝不记密文或明文
		return v.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "shop.credential.updated", OccurredAt: now,
			Risk:  eventbus.RiskMedium,
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.credential.put", Result: "success"},
			Data: map[string]any{
				"kind": string(in.Kind), "key_version": cred.KeyVersion, "username": in.Username,
			},
		})
	})
}

// RevealInput 查看凭证明文入参（高危）。
type RevealInput struct {
	ShopID     string
	Kind       types.CredentialKind
	Reason     string
	ApprovalID string // 二次审批单号（有值时写入审计，便于追溯）
	Actor      iam.Subject
	Terminal   eventbus.Terminal
	URL        string
}

// Reveal 解密查看凭证明文：
// 必须带 shop:credential.reveal 权限（ Approval=true → 触发 require_approval 义务），
// 且无论是否放行都要落高危审计 + 告警（L4 / M14 事后追溯）。
func (v *Vault) Reveal(ctx context.Context, in RevealInput) (string, error) {
	d := v.engine.Check(ctx, iam.Request{Subject: in.Actor, Action: "shop:credential.reveal", Path: "/shops"})
	if !d.Allow {
		_ = v.record(ctx, in, "denied")
		return "", apierr.Forbidden("无权查看凭证明文（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	// 数据级收口：own 范围下只能查看本人（或被委托）店铺的凭证。
	// 拒绝同样落高危审计（与功能层拒绝同一口径），明文从未离开密文层。
	sp, err := v.st.Shops().Get(tctx, in.ShopID)
	if err != nil {
		return "", err
	}
	if err := checkShopAccess(v.st, tctx, d, in.Actor, sp, "查看他人店铺的凭证"); err != nil {
		_ = v.record(ctx, in, "denied")
		return "", err
	}
	cred, err := v.st.Vault().Get(tctx, in.ShopID, in.Kind)
	if err != nil {
		return "", err
	}
	plain, err := v.cipher.Open(cred.SecretEnc)
	if err != nil {
		_ = v.record(ctx, in, "decrypt_failed")
		return "", apierr.Internal("凭证解密失败（可能是密钥版本缺失）")
	}
	_ = v.record(ctx, in, "success")
	return plain, nil
}

// ListKinds 列出店铺已托管的凭证类型（不含任何密文）。
// 服务层完整裁决（功能权限 + 数据级收口）：网关做一次、服务再核一次，与 Put/Reveal 同口径。
func (v *Vault) ListKinds(ctx context.Context, actor iam.Subject, shopID string) ([]string, error) {
	d := v.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:credential.manage", Path: "/shops"})
	if !d.Allow {
		return nil, apierr.Forbidden("无权查看店铺凭证（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sp, err := v.st.Shops().Get(tctx, shopID)
	if err != nil {
		return nil, err
	}
	if err := checkShopAccess(v.st, tctx, d, actor, sp, "查看他人店铺的凭证"); err != nil {
		return nil, err
	}
	list, err := v.st.Vault().List(tctx, shopID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, string(c.Kind))
	}
	return out, nil
}

// RotateKey 密钥轮换：把该店铺全部凭证用当前主密钥版本重封（不解业务明文以外的东西）。
func (v *Vault) RotateKey(ctx context.Context, shopID string, actor iam.Subject, term eventbus.Terminal) (int, error) {
	d := v.engine.Check(ctx, iam.Request{Subject: actor, Action: "shop:credential.manage", Path: "/shops"})
	if !d.Allow {
		return 0, apierr.Forbidden("无权维护店铺凭证（" + string(d.Layer) + "）")
	}
	tctx := store.WithTenant(ctx, actor.TenantID)
	sp, err := v.st.Shops().Get(tctx, shopID)
	if err != nil {
		return 0, err
	}
	if err := checkShopAccess(v.st, tctx, d, actor, sp, "轮换他人店铺的凭证密钥"); err != nil {
		return 0, err
	}
	list, err := v.st.Vault().List(tctx, shopID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range list {
		enc, err := v.cipher.Rekey(c.SecretEnc)
		if err != nil {
			continue
		}
		c.SecretEnc = enc
		c.KeyVersion = v.cipher.CurrentVersion()
		c.UpdatedBy = actor.UserID
		if err := v.st.Vault().Upsert(tctx, &c); err != nil {
			continue
		}
		n++
	}
	if n > 0 {
		_ = v.st.WithTx(tctx, func(tx store.Tx) error {
			return v.bus.Emit(tctx, tx, eventbus.Event{
				TenantID: actor.TenantID, Type: "shop.credential.rotated", OccurredAt: time.Now().UTC(),
				Actor: actorOf(actor), Subject: eventbus.Subject{ShopID: shopID}, Terminal: term,
				Context: eventbus.ContextInfo{Action: "shop.credential.rotate", Result: "success"},
				Data:    map[string]any{"count": n, "key_version": v.cipher.CurrentVersion()},
			})
		})
	}
	return n, nil
}

func (v *Vault) record(ctx context.Context, in RevealInput, result string) error {
	tctx := store.WithTenant(ctx, in.Actor.TenantID)
	now := time.Now().UTC()
	return v.st.WithTx(tctx, func(tx store.Tx) error {
		return v.bus.Emit(tctx, tx, eventbus.Event{
			TenantID: in.Actor.TenantID, Type: "shop.credential.revealed", OccurredAt: now,
			Risk:  eventbus.RiskHigh, // 查看凭证明文 = 高危，必须告警
			Actor: actorOf(in.Actor), Subject: eventbus.Subject{ShopID: in.ShopID},
			Terminal: in.Terminal,
			Context:  eventbus.ContextInfo{URL: in.URL, Action: "shop.credential.reveal", Result: result},
			Data:     map[string]any{"kind": string(in.Kind), "reason": in.Reason, "approval_id": in.ApprovalID},
		})
	})
}
