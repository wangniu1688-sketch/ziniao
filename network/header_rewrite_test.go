package network

import (
	"bytes"
	"strings"
	"testing"
)

func cfg120() RewriteHeaders {
	return RewriteHeaders{
		UA:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.109 Safari/537.36",
		ChromeMajor: 120,
		Platform:    "Windows",
	}
}

func runRewrite(t *testing.T, in string) string {
	t.Helper()
	var out bytes.Buffer
	if err := copyRewritingRequests(&out, strings.NewReader(in), cfg120()); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	return out.String()
}

// TestRewriteSecCHUA 核心场景：浏览器自带的 sec-ch-ua 写的是真实内核版本（156/120 之外的
// 任何值），必须与 UA 声称的版本对齐，否则「UA 说 120、请求头说 156」等于自曝。
func TestRewriteSecCHUA(t *testing.T) {
	in := "GET /seller HTTP/1.1\r\n" +
		"Host: seller.example\r\n" +
		"User-Agent: Mozilla/5.0 Chrome/120.0.6099.109 Safari/537.36\r\n" +
		"sec-ch-ua: \"Chromium\";v=\"156\", \"Microsoft Edge WebView2\";v=\"156\"\r\n" +
		"sec-ch-ua-mobile: ?0\r\n" +
		"sec-ch-ua-platform: \"Windows\"\r\n" +
		"Accept: */*\r\n\r\n"
	out := runRewrite(t, in)
	const wantBrand = "sec-ch-ua: \"Not_A Brand\";v=\"8\", \"Chromium\";v=\"120\", \"Google Chrome\";v=\"120\""
	if !strings.Contains(out, wantBrand) {
		t.Fatalf("sec-ch-ua 未被改写:\n%s", out)
	}
	if strings.Contains(out, "156") {
		t.Fatalf("真实内核版本 156 残留:\n%s", out)
	}
	if !strings.Contains(out, "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64)") {
		t.Fatalf("UA 未被替换:\n%s", out)
	}
	// 无关头必须原样保留（含顺序）
	if !strings.Contains(out, "Accept: */*\r\n") {
		t.Fatalf("无关请求头被破坏:\n%s", out)
	}
}

// TestRewriteAddsMissingHints Chrome 在有 UA-CH 时 sec-ch-ua/-mobile/-platform 总是成套出现，
// 缺任意一个本身就是异常信号，所以必须补齐而不是只改已有的。
func TestRewriteAddsMissingHints(t *testing.T) {
	in := "GET / HTTP/1.1\r\nHost: x\r\n\r\n"
	out := runRewrite(t, in)
	for _, want := range []string{"User-Agent: Mozilla/5.0", "sec-ch-ua: ", "sec-ch-ua-mobile: ?0", "sec-ch-ua-platform: \"Windows\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
}

// TestRewriteKeepAlive 连接复用：同一条连接上的每个请求都必须改写，
// 只改第一个会让「第一个请求伪装、后续请求暴露」成为更明显的行为指纹。
func TestRewriteKeepAlive(t *testing.T) {
	req := "GET /p HTTP/1.1\r\nHost: x\r\nsec-ch-ua: \"Chromium\";v=\"156\"\r\n\r\n"
	out := runRewrite(t, req+req+req)
	if n := strings.Count(out, "GET /p HTTP/1.1"); n != 3 {
		t.Fatalf("请求数不对: got %d want 3\n%s", n, out)
	}
	if n := strings.Count(out, "\"Chromium\";v=\"120\""); n != 3 {
		t.Fatalf("改写的请求不对: got %d want 3\n%s", n, out)
	}
	if strings.Count(out, "156") != 0 {
		t.Fatalf("仍有未改写请求:\n%s", out)
	}
}

// TestRewriteBodyPreserved 改写只换头部取值，不能破坏 body：
// Content-Length 与 chunked 两种分帧都要原样转发。
func TestRewriteBodyPreserved(t *testing.T) {
	cl := "POST /api HTTP/1.1\r\nHost: x\r\nContent-Length: 11\r\n\r\nhello world"
	if out := runRewrite(t, cl); !strings.HasSuffix(out, "hello world") {
		t.Fatalf("Content-Length body 损坏:\n%q", out)
	}
	chunked := "POST /api HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
	out := runRewrite(t, chunked)
	if !strings.HasSuffix(out, "5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n") {
		t.Fatalf("chunked body 损坏:\n%q", out)
	}
	// chunked 之后还能继续读下一个请求（分帧边界不能被吃掉）
	if out2 := runRewrite(t, chunked+"GET /next HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(out2, "GET /next HTTP/1.1") {
		t.Fatalf("chunked 后无法识别下一个请求:\n%q", out2)
	}
}

// TestRewriteDoesNotBreakHTTP2 防回归：h2 是二进制帧，改写器必须整条透传。
// 曾经的 bug 是拿 h1 解析器去切帧，把 SETTINGS 里的 0x0A 当换行重写，
// 结果是所有 HTTP/2 站点都打不开——比「不改写」严重得多。
func TestRewriteDoesNotBreakHTTP2(t *testing.T) {
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	// 一个 SETTINGS 帧（type=0x04, len=0, flags=0, stream=0），后面带个含 0x0A 的帧体
	frames := append([]byte{0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00},
		[]byte{0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x0A}...)
	in := append(append([]byte{}, preface...), frames...)

	var out bytes.Buffer
	if err := copyRewritingRequests(&out, bytes.NewReader(in), cfg120()); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("h2 流量被改写破坏了:\n in=%x\nout=%x", in, out.Bytes())
	}
}

// TestRewriteDisabled 未下发 UA 时不改写（sidecar 不该凭空捏造身份）。
func TestRewriteDisabled(t *testing.T) {
	var out bytes.Buffer
	in := "GET / HTTP/1.1\r\nHost: x\r\nUser-Agent: real/1.0\r\n\r\n"
	if err := copyRewritingRequests(&out, strings.NewReader(in), RewriteHeaders{}); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if !strings.Contains(out.String(), "User-Agent: real/1.0") {
		t.Fatalf("未配置 UA 时不应改写:\n%s", out.String())
	}
}
