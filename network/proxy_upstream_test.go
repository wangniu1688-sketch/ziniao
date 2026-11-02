package network_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"ziniao/network"
)

// 上游 CONNECT 拨号器的真实场景（P3 交付要求）。
//
// 为什么必须 mock 上游而不是连公网：这几个分支（407、接受后不响应、连接被拒）
// 全都依赖上游的具体表现，用公网代理既不可控，也会把测试凭据发出去。
// 全部监听 127.0.0.1，测试不发任何真实外网请求。

// upstreamMock 一个假上游：handler 决定它怎么对待 CONNECT，同时把收到的
// 原始握手文本记下来（用于断言认证头）。
type upstreamMock struct {
	addr    string
	mu      sync.Mutex
	request string // CONNECT 行 + 全部请求头
	target  string
}

func newUpstreamMock(t *testing.T, handler func(conn net.Conn, br *bufio.Reader, target string)) *upstreamMock {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假上游失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	m := &upstreamMock{addr: ln.Addr().String()}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go m.serve(conn, handler)
		}
	}()
	return m
}

func (m *upstreamMock) serve(conn net.Conn, handler func(net.Conn, *bufio.Reader, string)) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	var sb strings.Builder
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	sb.WriteString(line)
	// 读掉剩余请求头（到空行为止），否则写完响应后对端可能还在发。
	for {
		l, lerr := br.ReadString('\n')
		if lerr != nil || l == "\r\n" || l == "\n" {
			break
		}
		sb.WriteString(l)
	}
	fields := strings.Fields(line)
	target := ""
	if len(fields) >= 2 {
		target = fields[1]
	}
	m.mu.Lock()
	m.request, m.target = sb.String(), target
	m.mu.Unlock()
	handler(conn, br, target)
}

func (m *upstreamMock) Request() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.request
}

func (m *upstreamMock) Target() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.target
}

func (m *upstreamMock) Config() network.ProxyConfig {
	h, p, err := net.SplitHostPort(m.addr)
	if err != nil {
		panic(err)
	}
	n := 0
	for i := 0; i < len(p); i++ {
		n = n*10 + int(p[i]-'0')
	}
	return network.ProxyConfig{Protocol: network.ProtoHTTP, Host: h, Port: n}
}

// TestUpstreamConnectSuccess 成功建隧道：200 之后隧道要能跑通普通字节流。
func TestUpstreamConnectSuccess(t *testing.T) {
	m := newUpstreamMock(t, func(c net.Conn, br *bufio.Reader, _ string) {
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		// 隧道内回显：验证它是一条可用的全双工管道，而不只是「连上了」。
		b, _ := br.ReadString('\n')
		_, _ = c.Write([]byte(strings.ToUpper(b)))
	})
	d := network.NewDialer(m.Config())
	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("建隧道失败: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("隧道读取失败: %v", err)
	}
	if got := strings.ToUpper(string(buf[:n])); got != "PING\n" {
		t.Fatalf("隧道回显异常: %q", got)
	}
	if m.Target() != "example.com:443" {
		t.Fatalf("CONNECT 目标应为 example.com:443，实际 %q", m.Target())
	}
}

// TestUpstreamConnectAuthHeaderOnlyInHandshake 认证头只出现在 CONNECT 握手里。
//
// 这条断言的另一半更重要：**明文口令不得出现在错误信息里**。拨号器拿到的
// 口令一旦被拼进 error 字符串，就会随着启动失败的日志与审计事件一起落地，
// 那正是 P3 安全红线要挡住的事。
func TestUpstreamConnectAuthHeaderOnlyInHandshake(t *testing.T) {
	const user, pass = "isp_user_01", "P@ssw0rd!SuperSecret"
	want := "Proxy-Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte(user+":"+pass))

	m := newUpstreamMock(t, func(c net.Conn, _ *bufio.Reader, _ string) {
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	})
	cfg := m.Config()
	cfg.Username, cfg.Password = user, pass
	_, err := network.NewDialer(cfg).DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("407 必须判定为失败")
	}
	// 正面：握手里确实带了 Basic 认证。
	if req := m.Request(); !strings.Contains(req, want) {
		t.Fatalf("CONNECT 握手缺少 Basic 认证头: %q", req)
	}
	// 反面：错误串里既没有口令也没有认证头。
	msg := err.Error()
	if strings.Contains(msg, pass) || strings.Contains(msg, user) || strings.Contains(msg, want) {
		t.Fatalf("拨号错误串泄露凭据: %q", msg)
	}
	if !strings.Contains(msg, "CONNECT") {
		t.Fatalf("错误串应说明是 CONNECT 失败: %q", msg)
	}
}

// TestUpstreamConnect407 407 认证失败必须判失败（不能被当成 2xx 放行）。
func TestUpstreamConnect407(t *testing.T) {
	m := newUpstreamMock(t, func(c net.Conn, _ *bufio.Reader, _ string) {
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n"))
	})
	if conn, err := network.NewDialer(m.Config()).DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		_ = conn.Close()
		t.Fatal("407 必须失败，实际建成了隧道")
	}
}

// TestUpstreamConnectClosedWithoutResponse 上游接受了连接却直接关闭（无响应）：
// 常见原因是上游并发打满或被中间设备重置——必须快速失败，不能一直等。
func TestUpstreamConnectClosedWithoutResponse(t *testing.T) {
	m := newUpstreamMock(t, func(c net.Conn, _ *bufio.Reader, _ string) { _ = c.Close() })
	start := time.Now()
	if conn, err := network.NewDialer(m.Config()).DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		_ = conn.Close()
		t.Fatal("上游无响应必须失败")
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("上游无响应应快速失败，实际耗时 %v", el)
	}
}

// TestUpstreamConnectRefused 上游端口不可达：连接被拒同样是拨号失败。
func TestUpstreamConnectRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 立刻关掉 → 这个端口现在没人听
	h, p, _ := net.SplitHostPort(addr)
	n := 0
	for i := 0; i < len(p); i++ {
		n = n*10 + int(p[i]-'0')
	}
	cfg := network.ProxyConfig{Protocol: network.ProtoHTTP, Host: h, Port: n}
	if conn, err := network.NewDialer(cfg).DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		_ = conn.Close()
		t.Fatal("上游不可达必须失败")
	}
}

// TestUpstreamConnectTimeout 上游收下 CONNECT 却永不回复 → 必须超时失败。
//
// 拨号器的握手 deadline 固定 15s，所以这条用例必然耗 15s：
// `go test -short` 下跳过，避免日常跑测试被它拖慢。
func TestUpstreamConnectTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 15s 的超时用例（-short）")
	}
	m := newUpstreamMock(t, func(_ net.Conn, _ *bufio.Reader, _ string) {
		time.Sleep(30 * time.Second) // 永不回复
	})
	start := time.Now()
	if conn, err := network.NewDialer(m.Config()).DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		_ = conn.Close()
		t.Fatal("上游不回复时必须超时失败")
	}
	if el := time.Since(start); el > 25*time.Second {
		t.Fatalf("超时应在 15s 左右触发，实际 %v", el)
	}
}

// TestDirectDialerStaysDirect 未绑定代理必须是裸直连（P3 回归项）。
//
// 直连是所有未绑代理环境的现状行为，接入上游绝不能改变它——
// 这条断言把「未绑定环境被顺手改成走代理」变成编译不过。
func TestDirectDialerStaysDirect(t *testing.T) {
	for _, cfg := range []network.ProxyConfig{
		{Protocol: network.ProtoDirect},
		{Protocol: ""},
		{Protocol: network.ProtoHTTP, Host: ""},
	} {
		if !cfg.IsDirect() {
			t.Fatalf("应判定为直连: %+v", cfg)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	d := network.NewDialer(network.ProxyConfig{Protocol: network.ProtoDirect})
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("直连拨号失败: %v", err)
	}
	_ = conn.Close()
}
