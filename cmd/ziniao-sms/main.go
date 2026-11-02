// Command ziniao-sms 短信通道真机自检。
//
// 为什么必须有这个命令：签名算法对不对，本地怎么自测都证明不了——假服务器只会
// 复算我们自己写的那一套，两边一起错也能对上。唯一可信的判据是网关说"签名正确"。
// 短信通道因此必须有一个"一发就知道"的入口，而不是等一条 high 告警静默消失。
//
// 用法（凭据只经环境变量注入，绝不进 argv——wmic 能读出本机命令行）：
//
//	$env:ZINIAO_SMS_APP_KEY='ck_…'
//	$env:ZINIAO_SMS_APP_SECRET='cs_…'
//	$env:ZINIAO_SMS_SIGN_NAME='佳圆'
//	$env:ZINIAO_SMS_TEMPLATE_CODE='SMS_492460356'
//	go run ./cmd/ziniao-sms -to 13800138000
//
// 注意：每执行一次真的发一条短信（计费 + 打扰收件人），请勿写进巡检任务。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"ziniao/notify"
	"ziniao/platform/config"
)

func main() {
	to := flag.String("to", "", "接收短信的手机号（必填）")
	text := flag.String("text", "紫鸟浏览器通知通道自检：收到即表示签名与模板配置正确", "短信内容")
	flag.Parse()
	if *to == "" {
		fmt.Fprintln(os.Stderr, "缺少 -to（接收手机号）")
		os.Exit(2)
	}
	cfg := config.Load()
	if !cfg.Notify.SMSConfigured() {
		fmt.Fprintln(os.Stderr, "短信通道未配置：需要 ZINIAO_SMS_APP_KEY / ZINIAO_SMS_APP_SECRET / ZINIAO_SMS_SIGN_NAME / ZINIAO_SMS_TEMPLATE_CODE")
		os.Exit(2)
	}
	s, err := notify.NewSMSSender(notify.SMSConfig{
		AppKey:        cfg.Notify.SMSAppKey,
		AppSecret:     cfg.Notify.SMSAppSecret,
		SignName:      cfg.Notify.SMSSignName,
		TemplateCode:  cfg.Notify.SMSTemplateCode,
		TemplateKey:   cfg.Notify.SMSTemplateKey,
		Timeout:       cfg.Notify.SMSTimeout,
		MaxRetry:      cfg.Notify.SMSMaxRetry,
		AllowLoopback: cfg.Notify.SMSAllowLoopback,
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "装配失败：", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Health(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "配置自检失败：", err)
		os.Exit(1)
	}
	if err := s.Send(ctx, notify.Message{Title: *text, Phone: *to, CreatedAt: time.Now().UTC()}); err != nil {
		fmt.Fprintln(os.Stderr, "发送失败：", err)
		os.Exit(1)
	}
	fmt.Printf("已提交：签名=%s 模板=%s 收件人=%s\n", cfg.Notify.SMSSignName, cfg.Notify.SMSTemplateCode, *to)
	fmt.Println("请以手机是否收到为准——网关返回 200 只代表受理，不代表送达。")
}
