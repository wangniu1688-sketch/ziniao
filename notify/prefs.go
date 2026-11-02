package notify

import (
	"context"
	"sort"
	"strings"

	"ziniao/platform/apierr"
	"ziniao/platform/types"
)

// ---------- 每人通知偏好（migration 0008） ----------
//
// 背景：外发清单原先只按「当前装了什么通道」决定，用户填了邮箱手机号就一定会
// 收到。日常审批待办也这样推的结果不是"更及时"，而是**人把告警静音**——
// 噪音是最有效的「关掉所有通知」按钮。所以选择权回到每个人手上。
//
// 三条边界（刻意收窄，避免过度设计）：
//  1. 站内信不可关——它是可回看的事实记录，也是"我到底有没有收到过"的证据；
//     关掉全部开关 = 只要站内信，而不是什么都收不到。
//  2. Webhook 不进个人偏好：它是服务端集成通道（全局单 URL），
//     把它交给个人开关会让"这条告警有没有进 SIEM"变成不可预期的事。
//  3. 未设置 = 走默认（全部已装配外发通道），既有租户的行为不因这次改造而改变。

// PersonalChannels 允许个人选择的外发通道。
var PersonalChannels = []string{"email", SMSChannel}

// AvailableChannels 通知设置页可勾选的通道（装了不等于能选：webhook 不可选）。
func (s *Sink) AvailableChannels() []string {
	out := []string{}
	for _, ch := range s.externalChannels() {
		for _, p := range PersonalChannels {
			if ch == p {
				out = append(out, ch)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// NormalizeChannels 归一化并校验用户提交的外发通道清单。
//
// · "inapp" 静默丢弃（站内信恒开，不是一个可选项）；
// · 去重、排序，让 PUT 再 GET 回来的结果稳定；
// · 未知值直接报错而不是忽略——通道名拼错却静默生效，用户会以为自己关掉了。
func NormalizeChannels(chs []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, ch := range chs {
		ch = strings.TrimSpace(ch)
		if ch == "" || ch == ChannelInApp {
			continue
		}
		known := false
		for _, p := range PersonalChannels {
			if ch == p {
				known = true
				break
			}
		}
		if !known {
			return nil, apierr.Validation("未知的通知通道：" + ch + "（可选：" + strings.Join(PersonalChannels, "/") + "）")
		}
		if !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SetChannels 写入一人在一个团队里的通知偏好。
func (s *Sink) SetChannels(ctx context.Context, tenantID, userID string, chs []string) error {
	norm, err := NormalizeChannels(chs)
	if err != nil {
		return err
	}
	return s.st.Notifications().SetPrefs(ctx, &types.NotificationPrefs{
		TenantID: tenantID, UserID: userID, Channels: norm,
	})
}

// channelsFor 该用户实际要外发的通道 = 本次事件需要的通道 ∩ 该用户偏好。
//
// 读失败**按默认放行**（= want 原样）而不是把邮件捂住：通知是「打扰」不是
// 「授权」，偏好读不出来就静音，比多发一封更难解释。
// 返回值不含站内信，站内信由调用方拼在最前面。
func (s *Sink) channelsFor(ctx context.Context, tenantID, userID string, want []string) []string {
	p, err := s.st.Notifications().GetPrefs(ctx, tenantID, userID)
	if err != nil {
		s.log.Warn("读取通知偏好失败，按默认通道外发", "tenant", tenantID, "user", userID, "err", err)
		return want
	}
	if p == nil { // 未设置：行为与改造前一致
		return want
	}
	return intersect(want, p.Channels)
}

// EffectiveChannels 通知设置页展示用：用户当前启用的外发通道（未设置 = 已装配的全部）。
func (s *Sink) EffectiveChannels(ctx context.Context, tenantID, userID string) []string {
	installed := s.AvailableChannels()
	p, err := s.st.Notifications().GetPrefs(ctx, tenantID, userID)
	if err != nil || p == nil {
		return installed
	}
	// 设过但通道已卸载（比如撤掉了短信配置）：不能把已经不存在的通道摆回开关上
	return intersect(installed, p.Channels)
}

func intersect(a, b []string) []string {
	allow := map[string]bool{}
	for _, v := range b {
		allow[v] = true
	}
	out := []string{}
	for _, v := range a {
		if allow[v] {
			out = append(out, v)
		}
	}
	return out
}

// ChannelsSummary 偏好摘要（日志/状态展示用，不含任何凭据）。
func ChannelsSummary(chs []string) string {
	if len(chs) == 0 {
		return "仅站内信"
	}
	return "站内信+" + strings.Join(chs, "/")
}
