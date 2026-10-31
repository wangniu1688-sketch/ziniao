package notify

import (
	"log/slog"

	"ziniao/platform/config"
)

// BuildSenders 按环境配置装配外发通道。
//
// 两条返回值口径：
//  1. 通道只依赖自己的配置是否齐全——SMTP 没有 Host 就是没配，Webhook 没有 URL
//     就是没配。**不存在半配置状态**：配了一半的通道要么能真的投递，要么不要
//     出现在 notifications.channels 上（否则运营看到"已通知"却没收到）。
//  2. 构造失败（如 Webhook URL 非法）不打日志后继续，而是返回错误给装配方：
//     配置错了应该在启动阶段就暴露，而不是等到一条 high 告警静默消失。
func BuildSenders(cfg *config.Config, log *slog.Logger) ([]Sender, error) {
	if cfg == nil {
		return nil, nil
	}
	out := []Sender{}
	if cfg.Notify.SMTPHost != "" {
		out = append(out, NewSMTPSender(SMTPConfig{
			Host:    cfg.Notify.SMTPHost,
			Port:    cfg.Notify.SMTPPort,
			User:    cfg.Notify.SMTPUser,
			Pass:    cfg.Notify.SMTPPass,
			From:    cfg.Notify.SMTPFrom,
			TLSMode: cfg.Notify.SMTPTLSMode,
			Timeout: cfg.Notify.SMTPTimeout,
		}, log))
	}
	if cfg.Notify.WebhookURL != "" {
		wh, err := NewWebhookSender(WebhookConfig{
			URL:           cfg.Notify.WebhookURL,
			Secret:        cfg.Notify.WebhookSecret,
			Timeout:       cfg.Notify.WebhookTimeout,
			MaxRetry:      cfg.Notify.WebhookMaxRetry,
			AllowLoopback: cfg.Notify.WebhookAllowLoopback,
		}, log)
		if err != nil {
			return nil, err
		}
		out = append(out, wh)
	}
	return out, nil
}
