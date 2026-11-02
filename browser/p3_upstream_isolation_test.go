package browser

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/network"
	"ziniao/shop"
)

// P3 出口隔离：A 绑代理走上游，B 不绑走直连，两者互不串。
//
// 为什么必须起两个真实本地代理跑一次流量：只断言「配置里有没有上游」无法证明
// 流量真的走了那条路——上游配置写对了但拨号器没接上，配置断言照样是绿的。
// 这里的判据是「上游见过几次 CONNECT」：A 的每一次出网必须在上游留下记录，
// B 的一次都不许有。
//
// 全程 127.0.0.1：不发任何真实外网请求，也不依赖本机装 Chrome。

const (
	// 测试专用串，不是任何真实凭据；它存在的唯一目的是让「A 的凭据有没有
	// 串到 B」变成一个可被断言的事实。
	isoUpstreamUser = "iso-user-A"
	isoUpstreamPass = "iso-pass-A-NOT-REAL-0000"
)

// isoUpstream 一个本地 HTTP CONNECT 上游：把隧道接到给定目标，并记录见过什么。
type isoUpstream struct {
	addr  string
	mu    sync.Mutex
	conns int      // 收到的 CONNECT 次数（= 走了这个上游的出网次数）
	seen  []string // 每次 CONNECT 的目标
	auth  string   // 最后一次收到的 Proxy-Authorization 头
}

func newIsoUpstream(t *testing.T, targetAddr string) *isoUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假上游失败: %v", err)
	}
	u := &isoUpstream{addr: ln.Addr().String()}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go u.serve(conn, targetAddr)
		}
	}()
	return u
}

func (u *isoUpstream) serve(conn net.Conn, targetAddr string) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var hdr strings.Builder
	hdr.WriteString(line)
	// 读到空行（请求头结束）为止，否则对端可能还在发。
	for {
		l, lerr := br.ReadString('\n')
		if lerr != nil || l == "\r\n" || l == "\n" {
			break
		}
		hdr.WriteString(l)
	}
	fields := strings.Fields(line)
	target := ""
	if len(fields) >= 2 {
		target = fields[1]
	}
	u.mu.Lock()
	u.conns++
	u.seen = append(u.seen, target)
	for _, l := range strings.Split(hdr.String(), "\r\n") {
		if strings.HasPrefix(strings.ToLower(l), "proxy-authorization:") {
			u.auth = strings.TrimSpace(l)
		}
	}
	u.mu.Unlock()

	// 隧道：接到目标后双向搬运。这里不做任何鉴权——鉴权分支由
	// network/proxy_upstream_test.go 覆盖，本文件只关心流量走向。
	up, derr := net.Dial("tcp", targetAddr)
	if derr != nil {
		return
	}
	defer func() { _ = up.Close() }()
	if _, werr := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); werr != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}

func (u *isoUpstream) connCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conns
}

func (u *isoUpstream) lastTarget() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.seen) == 0 {
		return ""
	}
	return u.seen[len(u.seen)-1]
}

func (u *isoUpstream) authHeader() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auth
}

// splitAddr 127.0.0.1:1234 → ("127.0.0.1", 1234)。
func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("地址拆分失败: %v", err)
	}
	n := 0
	for i := 0; i < len(ps); i++ {
		n = n*10 + int(ps[i]-'0')
	}
	return h, n
}

// isoGetViaProxy 经本地代理访问目标（客户端不校验证书：MITM 用的是临时 CA）。
func isoGetViaProxy(t *testing.T, proxyPort int, targetURL string) string {
	t.Helper()
	pu, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 本地临时 CA，仅测试
	}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(targetURL)
	if err != nil {
		t.Fatalf("经本地代理访问目标失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("目标返回 %d: %s", resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body))
}

// TestP3UpstreamIsolationBetweenEnvs A 绑代理走上游、B 不绑直连，两者互不串。
//
// 覆盖三件事：
//  1. 出口走向：A 的出网在上游留下 CONNECT，B 的一次都没有；
//  2. 配置隔离：B 的上游配置必须是直连（不残留 A 的地址与凭据）；
//  3. argv 隔离：B 的命令行里不得出现 A 的用户名/口令。
func TestP3UpstreamIsolationBetweenEnvs(t *testing.T) {
	// 目标站：一个只回 PONG 的本地 HTTPS 服务（凭它判断「请求到没到」）。
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("PONG"))
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "https://")

	// 上游：只服务 A。B 若走过来，计数就会变 —— 这是「互不串」的硬判据。
	up := newIsoUpstream(t, targetAddr)
	upHost, upPort := splitAddr(t, up.addr)

	// 本地 CA：MITM 必需（CADir 为空时 CONNECT 一律失败）。只生成、不装信任库
	// ——改信任库是有副作用的动作，测试绝不该往开发机里塞证书。
	caDir := t.TempDir()
	if _, err := network.LoadOrCreateCA(caDir); err != nil {
		t.Fatalf("生成临时 CA 失败: %v", err)
	}

	// A：绑了手工代理（manual）；B：什么都不绑（direct）。
	specA := &shop.LaunchSpec{
		EnvID: "env-A", ShopID: "shop-A", Script: "/*fp*/", ExitMode: ExitManual,
		Upstream: &shop.UpstreamCredential{
			Protocol: "http", Host: upHost, Port: upPort,
			Username: isoUpstreamUser, Password: isoUpstreamPass, Country: "US",
		},
	}
	specB := &shop.LaunchSpec{
		EnvID: "env-B", ShopID: "shop-B", Script: "/*fp*/", ExitMode: ExitDirect,
	}

	cfgA := proxyConfigOf(specA, caDir)
	cfgB := proxyConfigOf(specB, caDir)
	// 目标是 httptest 的自签证书；生产链路的证书校验不在本用例的覆盖范围内。
	cfgA.Insecure, cfgB.Insecure = true, true

	// ---------- 配置隔离 ----------
	if cfgA.Upstream.IsDirect() {
		t.Fatalf("A 绑了代理，上游配置不应是直连: %+v", cfgA.Upstream)
	}
	if !cfgB.Upstream.IsDirect() {
		t.Fatalf("B 未绑代理，上游配置必须是直连: %+v", cfgB.Upstream)
	}
	if cfgB.Upstream.Host != "" || cfgB.Upstream.Username != "" || cfgB.Upstream.Password != "" {
		t.Fatalf("B 的上游配置不得残留 A 的地址或凭据: %+v", cfgB.Upstream)
	}

	pA, err := startLocalProxy(cfgA)
	if err != nil {
		t.Fatalf("A 的本地代理启动失败: %v", err)
	}
	defer func() { _ = pA.Close() }()
	pB, err := startLocalProxy(cfgB)
	if err != nil {
		t.Fatalf("B 的本地代理启动失败: %v", err)
	}
	defer func() { _ = pB.Close() }()
	// 两个环境各有一个本地代理：共用端口会让「各自走各自的出口」无从谈起。
	if pA.Port() == pB.Port() {
		t.Fatalf("A/B 的本地代理端口相同（%d）", pA.Port())
	}

	// ---------- 1) B 先跑：直连，上游不该被碰到 ----------
	if got := isoGetViaProxy(t, pB.Port(), target.URL+"/ping"); got != "PONG" {
		t.Fatalf("B 直连取到的响应异常: %q", got)
	}
	if n := up.connCount(); n != 0 {
		t.Fatalf("B 是直连环境，上游不该收到任何 CONNECT，实际 %d 次", n)
	}

	// ---------- 2) A 再跑：必须走上游 ----------
	if got := isoGetViaProxy(t, pA.Port(), target.URL+"/ping"); got != "PONG" {
		t.Fatalf("A 走上游取到的响应异常: %q", got)
	}
	if n := up.connCount(); n != 1 {
		t.Fatalf("A 的出网应恰好在上游留下 1 次 CONNECT，实际 %d 次", n)
	}
	if got := up.lastTarget(); got != targetAddr {
		t.Fatalf("上游收到的 CONNECT 目标应为 %q，实际 %q", targetAddr, got)
	}
	// 上游凭据确实被送到了上游（否则「走上游」只是连上了个没鉴权的口子）。
	if !strings.Contains(up.authHeader(), "Proxy-Authorization:") {
		t.Fatalf("A 的 CONNECT 未带 Proxy-Authorization: %q", up.authHeader())
	}

	// ---------- 3) 回头再跑一次 B：仍然不许碰到上游 ----------
	if got := isoGetViaProxy(t, pB.Port(), target.URL+"/ping"); got != "PONG" {
		t.Fatalf("B 第二次直连响应异常: %q", got)
	}
	if n := up.connCount(); n != 1 {
		t.Fatalf("B 的请求串到了 A 的上游（上游计数从 1 变成 %d）", n)
	}

	// ---------- 4) argv 隔离：A 的凭据不得出现在 B 的命令行里 ----------
	// 命令行会被 wmic/tasklist 与对账日志采集，串一次等于把 A 的出口凭据
	// 抄送给了所有能读进程列表的人。
	argvB := BuildArgv(ArgvConfig{
		ProfileDir:     t.TempDir(),
		LocalProxyPort: pB.Port(),
		OpenURL:        "about:blank",
	})
	if ArgvHasSecret(argvB, isoUpstreamPass, isoUpstreamUser) {
		t.Fatalf("B 的 argv 出现 A 的上游凭据: %v", argvB)
	}
	if !strings.Contains(strings.Join(argvB, " "), "--proxy-server=http://127.0.0.1:") {
		t.Fatalf("B 的 argv 应只写本机代理地址: %v", argvB)
	}
}
