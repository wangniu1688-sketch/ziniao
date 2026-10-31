package notify_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/notify"
	"ziniao/platform/config"
)

// ---------- 假 SMTP 服务器 ----------
//
// 为什么自己起一个而不是 mock Send：真正会出错的地方恰恰在协议层——
// 主题没做 RFC2047 编码、正文没走 base64、DATA 没以单个 . 收尾，
// 这些在 mock 里全都是「反正参数传到了」，上了真 SMTP 才是 550。

type fakeSMTP struct {
	ln net.Listener
	mu sync.Mutex

	mail   string
	rcpt   []string
	data   string
	authed bool
	refuse bool // RCPT TO 一律拒绝
}

func startFakeSMTP(t *testing.T, f *fakeSMTP) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本地 SMTP 失败: %v", err)
	}
	f.ln = ln
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	r := bufio.NewReader(conn)
	w := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	w("220 fake.ziniao ESMTP ready")

	inData := false
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		if inData {
			if cmd == "." {
				f.mu.Lock()
				f.data = strings.Join(lines, "\r\n")
				f.mu.Unlock()
				inData = false
				w("250 OK queued")
				continue
			}
			lines = append(lines, cmd)
			continue
		}
		switch up := strings.ToUpper(cmd); {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250-fake.ziniao greets you")
			w("250 SIZE 10485760") // 不声明 STARTTLS：对照-group 用于降级测试
		case strings.HasPrefix(up, "STARTTLS"):
			w("502 STARTTLS not supported")
		case strings.HasPrefix(up, "AUTH"):
			f.mu.Lock()
			f.authed = true
			f.mu.Unlock()
			w("235 Authentication successful")
		case strings.HasPrefix(up, "MAIL FROM"):
			f.mu.Lock()
			f.mail = cmd
			f.mu.Unlock()
			w("250 OK")
		case strings.HasPrefix(up, "RCPT TO"):
			f.mu.Lock()
			refuse := f.refuse
			if !refuse {
				f.rcpt = append(f.rcpt, cmd)
			}
			f.mu.Unlock()
			if refuse {
				w("550 no such mailbox")
				continue
			}
			w("250 OK")
		case strings.HasPrefix(up, "DATA"):
			inData = true
			lines = nil
			w("354 End data with <CR><LF>.<CR><LF>")
		case strings.HasPrefix(up, "QUIT"):
			w("221 Bye")
			return
		case strings.HasPrefix(up, "RSET"), strings.HasPrefix(up, "NOOP"):
			w("250 OK")
		default:
			w("502 Not implemented")
		}
	}
}

func (f *fakeSMTP) snapshot() (mail string, rcpt []string, data string, authed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mail, append([]string{}, f.rcpt...), f.data, f.authed
}

// TestSMTPDeliversUTF8Alert 中文告警要能穿过 7bit 中继：主题 RFC2047、正文 base64。
func TestSMTPDeliversUTF8Alert(t *testing.T) {
	f := &fakeSMTP{}
	host, port := startFakeSMTP(t, f)
	s := notify.NewSMTPSender(notify.SMTPConfig{
		Host: host, Port: port, TLSMode: "none", Timeout: 3 * time.Second,
		From: "alerts@ziniao.local",
	}, nil)

	err := s.Send(context.Background(), notify.Message{
		Title: "出口 IP 轮换告警", Body: "环境 env-1 出口由 1.2.3.4:39101 变为 1.2.3.4:39102",
		To: "ops@example.com", EventType: "env.exit_ip_rotated",
	})
	if err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	mail, rcpt, data, _ := waitMail(t, f)
	if !strings.Contains(mail, "alerts@ziniao.local") {
		t.Fatalf("MAIL FROM 不符: %q", mail)
	}
	if len(rcpt) != 1 || !strings.Contains(rcpt[0], "ops@example.com") {
		t.Fatalf("RCPT TO 不符: %v", rcpt)
	}
	if !strings.Contains(data, "Subject: =?UTF-8?B?") {
		t.Fatalf("主题未做 RFC2047 编码:\n%s", data)
	}
	if strings.Contains(data, "出口 IP 轮换告警") {
		t.Fatalf("主题竟然以明文出现:\n%s", data)
	}
	if body := decodeB64Body(t, data); !strings.Contains(body, "1.2.3.4:39102") {
		t.Fatalf("正文未包含告警内容:\n%s", body)
	}
}

// TestSMTPNoRecipientIsSkippable 没填邮箱的用户不是故障：必须返回可判别的哨兵错误。
func TestSMTPNoRecipientIsSkippable(t *testing.T) {
	f := &fakeSMTP{}
	host, port := startFakeSMTP(t, f)
	s := notify.NewSMTPSender(notify.SMTPConfig{
		Host: host, Port: port, From: "a@b.c", TLSMode: "none",
	}, nil)

	err := s.Send(context.Background(), notify.Message{Title: "t", To: "  "})
	if !errors.Is(err, notify.ErrNoRecipient) {
		t.Fatalf("期望 ErrNoRecipient，实际: %v", err)
	}
	// Send 是同步的：返回时服务端已经收到并记录了这一步（data 在回 "250 OK queued" 之前写入）
	_, rcpt, data, _ := f.snapshot()
	if data != "" || len(rcpt) > 0 {
		t.Fatalf("无收件人时不应发出任何邮件: rcpt=%v data=%q", rcpt, data)
	}
}

// TestSMTPRefusesPlaintextAuth 明文连接上带口令：拒绝发送而不是照发（口令会裸奔）。
func TestSMTPRefusesPlaintextAuth(t *testing.T) {
	f := &fakeSMTP{}
	host, port := startFakeSMTP(t, f)
	s := notify.NewSMTPSender(notify.SMTPConfig{
		Host: host, Port: port, User: "alerts", Pass: "topsecret",
		TLSMode: "none", From: "a@b.c",
	}, nil)

	err := s.Send(context.Background(), notify.Message{Title: "t", To: "ops@example.com"})
	if err == nil {
		t.Fatal("未加密连接不应发送口令")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("错误信息泄露口令: %v", err)
	}
	if _, _, _, authed := f.snapshot(); authed {
		t.Fatal("明文连接上竟完成了 SMTP 认证")
	}
}

// TestSMTPStartTLSFailureIsFatal STARTTLS 协商失败必须报错，不得静默降级为明文。
func TestSMTPStartTLSFailureIsFatal(t *testing.T) {
	f := &fakeSMTP{}
	host, port := startFakeSMTP(t, f)
	s := notify.NewSMTPSender(notify.SMTPConfig{
		Host: host, Port: port, TLSMode: "starttls", From: "a@b.c", Timeout: 2 * time.Second,
	}, nil)

	err := s.Send(context.Background(), notify.Message{Title: "t", To: "ops@example.com"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("STARTTLS 失败应返回明确错误，实际: %v", err)
	}
	if _, _, data, _ := f.snapshot(); data != "" {
		t.Fatal("降级为明文投递会把告警内容送上半路")
	}
}

// TestSMTPHealthProbesForReal Health 必须真的连一次，不能因为有 Host 配置就报 OK。
func TestSMTPHealthProbesForReal(t *testing.T) {
	f := &fakeSMTP{}
	host, port := startFakeSMTP(t, f)
	ok := notify.NewSMTPSender(notify.SMTPConfig{Host: host, Port: port, TLSMode: "none"}, nil)
	if err := ok.Health(context.Background()); err != nil {
		t.Fatalf("健康实例的健康检查应通过: %v", err)
	}
	bad := notify.NewSMTPSender(notify.SMTPConfig{
		Host: "127.0.0.1", Port: 1, TLSMode: "none", Timeout: time.Second,
	}, nil)
	if err := bad.Health(context.Background()); err == nil {
		t.Fatal("端口不可达时健康检查必须失败")
	}
	if err := notify.NewSMTPSender(notify.SMTPConfig{}, nil).Health(context.Background()); err == nil {
		t.Fatal("未配置主机时健康检查必须失败")
	}
}

// TestSMTPRcptRejected 服务端拒收：错误要带出 RCPT 环节便于排查，但不能带凭据。
func TestSMTPRcptRejected(t *testing.T) {
	f := &fakeSMTP{refuse: true}
	host, port := startFakeSMTP(t, f)
	s := notify.NewSMTPSender(notify.SMTPConfig{
		Host: host, Port: port, TLSMode: "none", From: "a@b.c",
	}, nil)

	err := s.Send(context.Background(), notify.Message{Title: "t", To: "ops@example.com"})
	if err == nil || !strings.Contains(err.Error(), "RCPT") {
		t.Fatalf("应收件人被拒的错误，实际: %v", err)
	}
}

// TestBuildSendersSkipsDisabledChannels 没配就是没配，不做半套配置。
func TestBuildSendersSkipsDisabledChannels(t *testing.T) {
	senders, err := notify.BuildSenders(config.Default(), nil)
	if err != nil {
		t.Fatalf("默认配置（全关）不应报错: %v", err)
	}
	if len(senders) != 0 {
		t.Fatalf("默认应无任何外发通道，实际 %d", len(senders))
	}
	cfg := config.Default()
	cfg.Notify.SMTPHost = "smtp.example.com"
	cfg.Notify.WebhookURL = "https://hooks.example.com/ziniao"
	senders, err = notify.BuildSenders(cfg, nil)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(senders) != 2 || senders[0].Channel() != "email" || senders[1].Channel() != "webhook" {
		t.Fatalf("期望 email+webhook 两个通道，实际: %+v", senders)
	}
}

// TestBuildSendersRejectsBadWebhookURL Webhook 地址非法要在启动装配阶段就炸，
// 而不是等到某条 high 告警静默丢失。
func TestBuildSendersRejectsBadWebhookURL(t *testing.T) {
	cfg := config.Default()
	cfg.Notify.WebhookURL = "ftp://hooks.example.com/ziniao"
	if _, err := notify.BuildSenders(cfg, nil); err == nil {
		t.Fatal("非 http/https 的 Webhook 地址必须被拒绝")
	}
}

// TestNoRecipientPrecedesConfigCheck 收件人为空优先于其它检查：
// 配置问题不该掩盖「这条通知本来就没有收件人」的事实。
func TestNoRecipientPrecedesConfigCheck(t *testing.T) {
	s := notify.NewSMTPSender(notify.SMTPConfig{Host: "mail.example.com"}, nil)
	if err := s.Send(context.Background(), notify.Message{Title: "t", To: ""}); !errors.Is(err, notify.ErrNoRecipient) {
		t.Fatalf("期望 ErrNoRecipient，实际: %v", err)
	}
}

// ---------- 辅助 ----------

func waitMail(t *testing.T, f *fakeSMTP) (string, []string, string, bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mail, rcpt, data, authed := f.snapshot(); data != "" || len(rcpt) > 0 {
			return mail, rcpt, data, authed
		}
		time.Sleep(20 * time.Millisecond)
	}
	mail, rcpt, data, _ := f.snapshot()
	t.Fatalf("未收到邮件：mail=%q rcpt=%v data=%q", mail, rcpt, data)
	return "", nil, "", false
}

// decodeB64Body 取 DATA 段里的 base64 正文并还原。
func decodeB64Body(t *testing.T, data string) string {
	t.Helper()
	parts := strings.Split(data, "\r\n\r\n")
	enc := strings.Join(strings.Split(parts[len(parts)-1], "\r\n"), "")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		t.Fatalf("正文 base64 解码失败: %v\n%s", err, data)
	}
	return string(raw)
}
