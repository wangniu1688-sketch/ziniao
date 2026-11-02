package shop

import (
	"context"
	"time"

	"ziniao/iam"
	"ziniao/platform/apierr"
	"ziniao/platform/store"
	"ziniao/platform/types"
)

// checkShopAccess 数据级店铺校验的**统一收口**。
//
// 背景：own 数据范围的判定原先散在各入口，且只有店铺读/改路径接入——
// 环境与凭证入口（启动/关闭环境、托管凭证的维护与查看）完全没有数据级校验，
// 持有 env:launch / shop:credential.manage 的 own 范围成员可以操作租户内
// 任何店铺的环境与凭证，"数据范围"在这两条链路上形同虚设。
// 现在所有「带店铺 ID 的入口」一律走这里，判定口径只有一份：
//
//   - all/team 数据范围：放行（租户级管理职责）；
//   - own 范围 + 本人店铺：放行；
//   - 其余：必须持有该店铺的有效店铺级 DataGrant（代理机构委托批准后写入，
//     到期行随读即失效，过期判定在这里做、不等巡检清行——与临时授权同一分工）；
//   - 授权查询失败：拒绝（fail-closed：查不到授权就当没有，半开比不开危险）。
//
// verb 只用于错误文案（如 "启动他人店铺的环境"），不参与判定。
func checkShopAccess(st store.Store, tctx context.Context, d iam.Decision,
	actor iam.Subject, sp *types.Shop, verb string) error {
	if d.DataScope != "own" || sp.OwnerID == actor.UserID {
		return nil
	}
	grants, err := st.DataGrants().ListByUser(tctx, actor.TenantID, actor.UserID, "shop")
	if err != nil {
		return apierr.Forbidden("无权" + verb + "（数据级校验失败，已拒绝）")
	}
	now := time.Now().UTC()
	for _, g := range grants {
		if g.ResourceID == sp.ID && (g.ExpiresAt == nil || g.ExpiresAt.After(now)) {
			return nil
		}
	}
	return apierr.Forbidden("无权" + verb + "（数据级权限）")
}
