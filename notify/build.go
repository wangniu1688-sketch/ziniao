package notify

import (
	"fmt"
	"log/slog"
	"strings"

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
	if cfg.Notify.SMSConfigured() {
		// 服务商写错必须拦在装配阶段：云市场（AppKey 签名）与 dysmsapi
		// （AccessKey 的 RPC 签名）是两套完全不同的算法，写成后者会得到一批
		// 稳定的 403，而不是"看起来发成功了"。
		if p := strings.ToLower(strings.TrimSpace(cfg.Notify.SMSProvider)); p != "" && p != "aliyun_market" {
			return nil, fmt.Errorf("notify: 不支持的短信服务商 %q（当前仅 aliyun_market：云市场 AppKey+AppSecret 签名）", cfg.Notify.SMSProvider)
		}
		sms, err := NewSMSSender(SMSConfig{
			AppKey:        cfg.Notify.SMSAppKey,
			AppSecret:     cfg.Notify.SMSAppSecret,
			SignName:      cfg.Notify.SMSSignName,
			TemplateCode:  cfg.Notify.SMSTemplateCode,
			TemplateKey:   cfg.Notify.SMSTemplateKey,
			Timeout:       cfg.Notify.SMSTimeout,
			MaxRetry:      cfg.Notify.SMSMaxRetry,
			AllowLoopback: cfg.Notify.SMSAllowLoopback,
		}, log)
		if err != nil {
			return nil, err
		}
		out = append(out, sms)
	}
	return out, nil
}
