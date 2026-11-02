package shop_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ziniao/eventbus"
	"ziniao/iam"
	"ziniao/platform/config"
	"ziniao/platform/crypto"
	"ziniao/platform/store"
	"ziniao/platform/store/mem"
	"ziniao/platform/types"
	"ziniao/shop"
)

// P3 上游代理接入的服务层测试。
//
// 全部用本地 mock：假上游（CONNECT 隧道 / 407 拒绝）+ 假探测目标（回显出口 IP）。
// 不连任何真实外网，也不使用任何真实凭据。

// ---------- mock ----------

// tunnelProxy 起一个「真转发」的假上游：CONNECT 之后连目标并双向搬运。
func tunnelProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假上游失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				line, rerr := br.ReadString('\n')
				if rerr != nil {
					return
				}
				for {
					l, lerr := br.ReadString('\n')
					if lerr != nil || l == "\r\n" || l == "\n" {
						break
					}
				}
				fields := strings.Fields(line)
				if len(fields) < 2 {
					return
				}
				up, derr := net.Dial("tcp", fields[1])
				if derr != nil {
					return
				}
				defer func() { _ = up.Close() }()
				if _, werr := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); werr != nil {
					return
				}
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// denyProxy 起一个「拒绝」的假上游：一律回给定状态行（如 407）后关闭。
func denyProxy(t *testing.T, statusLine string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假上游失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				if _, rerr := br.ReadString('\n'); rerr != nil {
					return
				}
				for {
					l, lerr := br.ReadString('\n')
					if lerr != nil || l == "\r\n" || l == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte(statusLine + "\r\n\r\n"))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// ipEchoTarget 起一个只回显裸 IP 的探测目标（对应 api.ipify.org 的行为）。
func ipEchoTarget(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1.2.3.4"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("地址解析失败: %v", err)
	}
	n := 0
	for i := 0; i < len(p); i++ {
		n = n*10 + int(p[i]-'0')
	}
	return h, n
}

// ---------- 装配 ----------

type probeSuite struct {
	proxies *shop.ProxyService
	env     *shop.EnvService
	st      *mem.Store
	actor   iam.Subject
	cipher  *crypto.EnvelopeCipher
}

func newProbeSuite(t *testing.T) *probeSuite {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	st := mem.New()
	if err := iam.EnsureBuiltinRoles(ctx, st); err != nil {
		t.Fatal(err)
	}
	role, err := st.Roles().GetByCode(ctx, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New(st, cfg, nil)
	kek, err := crypto.NewLocalKEK(nil, 0) // 测试用随机主密钥
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	envSvc := shop.NewEnvService(st, bus, iam.NewEngine(cfg), nil, nil)
	envSvc.SetCipher(cipher)
	// 默认与 config.Default() 同口径（ProxyTestEnabled=true）；
	// 目标留空会让拨测回落到公网默认值，测试里需要出网的用例都会显式覆盖它。
	envSvc.SetEgressProbe(shop.EgressProbeConfig{Enabled: true, TimeoutMS: 3000})
	return &probeSuite{
		proxies: shop.NewProxyService(st, bus, iam.NewEngine(cfg), cipher),
		env:     envSvc,
		st:      st,
		cipher:  cipher,
		actor:   iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner", Roles: []types.Role{*role}},
	}
}

// seedProxy 建一条台账代理；host/port 来自传入地址。
func (s *probeSuite) seedProxy(t *testing.T, name, addr, password, country string) string {
	t.Helper()
	h, p := splitHostPort(t, addr)
	v, err := s.proxies.Create(context.Background(), shop.ProxyCreateInput{
		Name: name, Protocol: "http", Host: h, Port: p,
		Username: "isp_user", Password: password, Country: country,
		Type: "isp", City: "Los Angeles", Actor: s.actor, URL: "/proxies",
	})
	if err != nil {
		t.Fatalf("建代理失败: %v", err)
	}
	return v.ID
}

// ---------- 拨测落库 ----------

// TestProxyTestPersistsOK 拨测成功：exit_ip / latency_ms 落库，check_status=ok。
//
// 落库而不是只返回给前端，是因为「能不能绑」这个判据必须来自库里的事实——
// 否则换个浏览器点一下就能绕过绑定校验。
func TestProxyTestPersistsOK(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	id := s.seedProxy(t, "美西-01", tunnelProxy(t), "s3cr3t-pass", "US")

	res, err := s.proxies.Test(ctx, shop.ProxyTestInput{
		ProxyID: id, Target: ipEchoTarget(t), TimeoutMS: 3000, Actor: s.actor,
	})
	if err != nil {
		t.Fatalf("拨测失败: %v", err)
	}
	if !res.OK {
		t.Fatalf("拨测应成功，实际 %+v", res)
	}
	if res.ExitIP != "1.2.3.4" {
		t.Fatalf("exit_ip 应为 1.2.3.4，实际 %q", res.ExitIP)
	}
	if res.Skipped {
		t.Fatal("TEST=on 时不得标记 skipped")
	}
	// 台账必须同步：这是「下次能不能绑」的判据。
	tctx := store.WithTenant(ctx, "t1")
	p, err := s.st.Proxies().Get(tctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.CheckStatus != types.ProxyCheckOK {
		t.Fatalf("check_status 应为 ok，实际 %q", p.CheckStatus)
	}
	if p.ExitIP != "1.2.3.4" {
		t.Fatalf("exit_ip 未落库: %q", p.ExitIP)
	}
	if p.LatencyMS == nil {
		t.Fatal("latency_ms 未落库")
	}
	if p.LastCheckedAt == nil {
		t.Fatal("last_checked_at 未落库")
	}
	if p.LastError != "" {
		t.Fatalf("成功时 last_error 应清空，实际 %q", p.LastError)
	}
}

// TestProxyTestPersistsFail 拨测失败：check_status=fail + last_error 落库。
func TestProxyTestPersistsFail(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	id := s.seedProxy(t, "坏代理", denyProxy(t, "HTTP/1.1 407 Proxy Authentication Required"), "wrong-pass", "US")

	res, err := s.proxies.Test(ctx, shop.ProxyTestInput{
		ProxyID: id, Target: ipEchoTarget(t), TimeoutMS: 3000, Actor: s.actor,
	})
	if err != nil {
		t.Fatalf("拨测接口不应返回错误（失败是结论不是异常）: %v", err)
	}
	if res.OK {
		t.Fatalf("407 不应判成功: %+v", res)
	}
	if res.Error == "" {
		t.Fatal("失败必须有可读原因")
	}
	tctx := store.WithTenant(ctx, "t1")
	p, err := s.st.Proxies().Get(tctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.CheckStatus != types.ProxyCheckFail {
		t.Fatalf("check_status 应为 fail，实际 %q", p.CheckStatus)
	}
	if p.LastError == "" {
		t.Fatal("last_error 应落库（运营靠它排障）")
	}
}

// TestProxyResponseNeverCarriesPassword 口令不进任何响应（双重断言）：
// 建代理的响应、详情响应、拨测响应，全都不许出现明文口令。
func TestProxyResponseNeverCarriesPassword(t *testing.T) {
	s := newProbeSuite(t)
	const pass = "P@ssw0rd-SuperSecret-01"
	id := s.seedProxy(t, "掩码校验", tunnelProxy(t), pass, "US")

	for name, v := range map[string]any{
		"create": mustView(t, s, id),
		"test":   mustTestResult(t, s, id),
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), pass) {
			t.Fatalf("%s 响应泄露明文口令: %s", name, b)
		}
		if strings.Contains(string(b), "password_enc") {
			t.Fatalf("%s 响应泄露密文列: %s", name, b)
		}
	}
	view := mustView(t, s, id)
	if !view.HasPassword {
		t.Fatal("设了口令却显示 has_password=false")
	}
	if view.PasswordMask == "" || view.PasswordMask == pass {
		t.Fatalf("详情应带掩码且不等于明文，实际 %q", view.PasswordMask)
	}
	if strings.Contains(view.PasswordMask, pass[1:4]) {
		t.Fatalf("掩码泄露了中间字符: %q", view.PasswordMask)
	}
}

// TestProxyUpdateKeepsPasswordWhenUntouched 改备注不该把口令抹掉。
//
// 需求 §4「编辑时未改口令则不覆盖」：常规读路径显式排除 password_enc，
// 整行写回会把空密文写进去——表现是「改一次备注，下次启动上游返 407」。
func TestProxyUpdateKeepsPasswordWhenUntouched(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	id := s.seedProxy(t, "改备注", tunnelProxy(t), "keep-me-pass", "US")

	if _, err := s.proxies.Update(ctx, shop.ProxyUpdateInput{
		ID: id, Note: "只改备注", Actor: s.actor, URL: "/proxies",
	}); err != nil {
		t.Fatalf("改备注失败: %v", err)
	}
	tctx := store.WithTenant(ctx, "t1")
	enc, err := s.st.Proxies().PasswordEnc(tctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if enc == "" {
		t.Fatal("改备注后口令被清空了")
	}
	plain, err := s.cipher.Open(enc)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "keep-me-pass" {
		t.Fatalf("口令内容被改写: %q", plain)
	}
	// 显式清空仍然要生效。
	if _, err := s.proxies.Update(ctx, shop.ProxyUpdateInput{
		ID: id, Password: strPtr(""), Actor: s.actor, URL: "/proxies",
	}); err != nil {
		t.Fatalf("清空口令失败: %v", err)
	}
	if enc, _ := s.st.Proxies().PasswordEnc(tctx, id); enc != "" {
		t.Fatal("显式清空口令后仍有密文")
	}
}

// ---------- 启动前出口拨测（阶段 1） ----------

// TestLaunchProbeBlocksStartWhenProxyBroken 拨测失败必须阻断启动，且不悬空 running。
func TestLaunchProbeBlocksStartWhenProxyBroken(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	seedEnv(t, s.st, "e-bad", shop.EnvStatusIdle, time.Now().UTC())
	id := s.seedProxy(t, "坏出口", denyProxy(t, "HTTP/1.1 407 Proxy Authentication Required"), "bad-pass", "US")
	bindProxy(t, s, "e-bad", id, true)

	_, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e-bad", Actor: s.actor})
	if err == nil {
		t.Fatal("拨测失败必须阻断启动")
	}
	if !strings.Contains(err.Error(), "拨测") {
		t.Fatalf("错误应说明是出口拨测未通过，实际: %v", err)
	}
	env, err := s.st.Environments().Get(store.WithTenant(ctx, "t1"), "e-bad")
	if err != nil {
		t.Fatal(err)
	}
	if env.Status == shop.EnvStatusRunning {
		t.Fatal("拨测失败不得留下 running（悬空态）")
	}
}

// TestLaunchProbeAllowedWhenTestOff TEST=off 跳过健康校验：允许启动、不拨测。
//
// off 是「部署侧摘掉了这条出网路径」，不是「代理不通」，因此**不得**拦。
func TestLaunchProbeAllowedWhenTestOff(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	seedEnv(t, s.st, "e-off", shop.EnvStatusIdle, time.Now().UTC())
	id := s.seedProxy(t, "未验证出口", denyProxy(t, "HTTP/1.1 407 Proxy Authentication Required"), "bad-pass", "US")
	bindProxy(t, s, "e-off", id, false)

	spec, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e-off", Actor: s.actor})
	if err != nil {
		t.Fatalf("TEST=off 时不得阻断启动: %v", err)
	}
	if spec.ExitMode != "manual" || spec.Upstream == nil {
		t.Fatalf("仍应走手工代理出口: %+v", spec)
	}
}

// TestLaunchProbePassesAndDirectEnvStaysDirect 两条回归：
//   - 绑定了好代理 → 启动放行且出口是 manual；
//   - 未绑定 → 直连（现状行为不得被上游接入改变）。
func TestLaunchProbePassesAndDirectEnvStaysDirect(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	target := ipEchoTarget(t)

	seedEnv(t, s.st, "e-ok", shop.EnvStatusIdle, time.Now().UTC())
	id := s.seedProxy(t, "好出口", tunnelProxy(t), "good-pass", "US")
	bindProxy(t, s, "e-ok", id, true)
	spec, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e-ok", Actor: s.actor})
	if err != nil {
		t.Fatalf("好代理应放行启动: %v", err)
	}
	if spec.ExitMode != "manual" || spec.Upstream == nil {
		t.Fatalf("绑定后应为 manual 出口: %+v", spec)
	}
	if spec.Upstream.Password != "good-pass" {
		t.Fatal("启动单应带上解密后的出口口令（明文只在内存）")
	}
	// 拨测成功的结果要落到台账：运营在代理管理页就能看到出口 IP。
	p, _ := s.st.Proxies().Get(store.WithTenant(ctx, "t1"), id)
	if p.CheckStatus != types.ProxyCheckOK || p.ExitIP != "1.2.3.4" {
		t.Fatalf("启动前拨测结果未落库: %+v", p)
	}
	_ = target

	// 未绑定 → 直连。
	seedEnv(t, s.st, "e-direct", shop.EnvStatusIdle, time.Now().UTC())
	spec2, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e-direct", Actor: s.actor})
	if err != nil {
		t.Fatalf("未绑定环境应能启动: %v", err)
	}
	if spec2.ExitMode != "direct" || spec2.Upstream != nil {
		t.Fatalf("未绑定环境必须直连，实际 mode=%s upstream=%+v", spec2.ExitMode, spec2.Upstream)
	}
}

// TestEgressProbePasswordNeverInLogs 口令不进日志（日志捕获断言）。
func TestEgressProbePasswordNeverInLogs(t *testing.T) {
	s := newProbeSuite(t)
	ctx := context.Background()
	var buf bytes.Buffer
	s.env.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	seedEnv(t, s.st, "e-log", shop.EnvStatusIdle, time.Now().UTC())
	const pass = "LOG-MUST-NOT-CONTAIN-ME-99"
	id := s.seedProxy(t, "日志校验", denyProxy(t, "HTTP/1.1 407 Proxy Authentication Required"), pass, "US")
	bindProxy(t, s, "e-log", id, true)

	if _, err := s.env.Launch(ctx, shop.LaunchInput{ShopID: "shop-e-log", Actor: s.actor}); err == nil {
		t.Fatal("坏代理应阻断启动")
	}
	if got := buf.String(); strings.Contains(got, pass) {
		t.Fatalf("日志泄露出口口令:\n%s", got)
	}
	if got := buf.String(); !strings.Contains(got, "拨测") {
		t.Fatalf("失败原因应进日志（否则排障没有抓手）:\n%s", got)
	}
}

// ---------- 绑定校验与软告警 ----------

// TestBindProxyRequiresProbeOK 绑定前必须拨测 ok；TEST=off 时不强制。
func TestBindProxyRequiresProbeOK(t *testing.T) {
	s := newProbeSuite(t)
	seedEnv(t, s.st, "e-bind", shop.EnvStatusIdle, time.Now().UTC())

	// unknown（没拨测过）→ 拒绝
	id := s.seedProxy(t, "未拨测", tunnelProxy(t), "p1", "US")
	if err := bindProxyErr(s, "e-bind", id); err == nil {
		t.Fatal("check_status=unknown 时不得允许绑定")
	}
	// ok → 允许
	if err := markCheck(t, s, id, types.ProxyCheckOK); err != nil {
		t.Fatal(err)
	}
	if err := bindProxyErr(s, "e-bind", id); err != nil {
		t.Fatalf("check_status=ok 应允许绑定: %v", err)
	}
	// fail → 拒绝
	if err := markCheck(t, s, id, types.ProxyCheckFail); err != nil {
		t.Fatal(err)
	}
	if err := bindProxyErr(s, "e-bind", id); err == nil {
		t.Fatal("check_status=fail 时不得允许绑定")
	}
	// TEST=off → 不强制（内网部署不能被自己的开关卡死）
	s.env.SetEgressProbe(shop.EgressProbeConfig{Enabled: false})
	if err := bindProxyErr(s, "e-bind", id); err != nil {
		t.Fatalf("TEST=off 时不得因健康状态拒绝绑定: %v", err)
	}
}

// TestBindProxyWarnsSharedProxyAndCountryMismatch 软校验只告警、不阻断。
func TestBindProxyWarnsSharedProxyAndCountryMismatch(t *testing.T) {
	s := newProbeSuite(t)
	seedEnv(t, s.st, "e-a", shop.EnvStatusIdle, time.Now().UTC())
	seedEnv(t, s.st, "e-b", shop.EnvStatusIdle, time.Now().UTC())
	// 环境指纹国家 DE，代理出口 US → 应给国家不一致告警。
	id := s.seedProxy(t, "美西共享", tunnelProxy(t), "p1", "US")
	if err := markCheck(t, s, id, types.ProxyCheckOK); err != nil {
		t.Fatal(err)
	}
	if err := setEnvCountry(t, s, "e-a", "DE"); err != nil {
		t.Fatal(err)
	}
	res, err := s.env.BindProxy(context.Background(), shop.BindProxyInput{
		EnvID: "e-a", ProxyID: id, Actor: s.actor, URL: "/environments",
	})
	if err != nil {
		t.Fatalf("国家不一致不得阻断绑定: %v", err)
	}
	if !hasWarning(res.Warnings, "国家") {
		t.Fatalf("应给出国家不一致告警，实际 %v", res.Warnings)
	}
	// 一号多环境：再绑一个环境，应出现「被其它环境绑定」告警。
	if err := setEnvCountry(t, s, "e-b", "US"); err != nil {
		t.Fatal(err)
	}
	res2, err := s.env.BindProxy(context.Background(), shop.BindProxyInput{
		EnvID: "e-b", ProxyID: id, Actor: s.actor, URL: "/environments",
	})
	if err != nil {
		t.Fatalf("一号多环境不得阻断绑定（本期只告警）: %v", err)
	}
	if !hasWarning(res2.Warnings, "其它环境") {
		t.Fatalf("应给出一号多环境告警，实际 %v", res2.Warnings)
	}
}

// TestBindProxyRejectsRunningEnv 运行中的环境禁止换出口（P3 回归项）。
func TestBindProxyRejectsRunningEnv(t *testing.T) {
	s := newProbeSuite(t)
	seedEnv(t, s.st, "e-run", shop.EnvStatusRunning, time.Now().UTC())
	id := s.seedProxy(t, "运行中换绑", tunnelProxy(t), "p1", "US")
	if err := markCheck(t, s, id, types.ProxyCheckOK); err != nil {
		t.Fatal(err)
	}
	err := bindProxyErr(s, "e-run", id)
	if err == nil {
		t.Fatal("运行中环境必须拒绝换绑")
	}
	if !strings.Contains(err.Error(), "运行") {
		t.Fatalf("拒绝原因应提示先停止环境，实际: %v", err)
	}
}

// ---------- 小工具 ----------

func mustView(t *testing.T, s *probeSuite, id string) *shop.ProxyView {
	t.Helper()
	v, err := s.proxies.Get(context.Background(), s.actor, id)
	if err != nil {
		t.Fatalf("取代理详情失败: %v", err)
	}
	return v
}

func mustTestResult(t *testing.T, s *probeSuite, id string) *shop.ProxyTestResult {
	t.Helper()
	// 目标故意不可达：这里只关心响应体里有没有凭据，成不成功无所谓。
	res, err := s.proxies.Test(context.Background(), shop.ProxyTestInput{
		ProxyID: id, Target: ipEchoTarget(t), TimeoutMS: 500, Actor: s.actor,
	})
	if err != nil {
		t.Fatalf("拨测失败: %v", err)
	}
	return res
}

func markCheck(t *testing.T, s *probeSuite, id, status string) error {
	t.Helper()
	return s.st.Proxies().SaveCheckResult(store.WithTenant(context.Background(), "t1"),
		id, store.ProxyCheckResult{
			CheckStatus: status, ExitIP: "1.2.3.4", ExitCountry: "US",
			LatencyMS: 42, CheckedAt: time.Now().UTC(),
		})
}

func bindProxy(t *testing.T, s *probeSuite, envID, proxyID string, probeOn bool) {
	t.Helper()
	if err := markCheck(t, s, proxyID, types.ProxyCheckOK); err != nil {
		t.Fatal(err)
	}
	s.env.SetEgressProbe(shop.EgressProbeConfig{
		Enabled: probeOn, Target: ipEchoTarget(t), TimeoutMS: 3000,
	})
	if _, err := s.env.BindProxy(context.Background(), shop.BindProxyInput{
		EnvID: envID, ProxyID: proxyID, Actor: s.actor, URL: "/environments",
	}); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
}

func bindProxyErr(s *probeSuite, envID, proxyID string) error {
	_, err := s.env.BindProxy(context.Background(), shop.BindProxyInput{
		EnvID: envID, ProxyID: proxyID, Actor: s.actor, URL: "/environments",
	})
	return err
}

func setEnvCountry(t *testing.T, s *probeSuite, envID, country string) error {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "t1")
	env, err := s.st.Environments().Get(ctx, envID)
	if err != nil {
		t.Fatal(err)
	}
	env.Country = country
	env.Snapshot = map[string]string{"country": country}
	return s.st.Environments().Update(ctx, env)
}

func hasWarning(w []string, sub string) bool {
	for _, s := range w {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func strPtr(s string) *string { return &s }
