package network

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// TestRealClientHelloNoGREASE 真实 ClientHello：不得含 GREASE、版本取 0x0303。
func TestRealClientHelloNoGREASE(t *testing.T) {
	spec, err := RealClientHello(utls.HelloChrome_120)
	if err != nil {
		t.Fatalf("抓真实 ClientHello 失败: %v", err)
	}
	if spec.Version != 0x0303 {
		t.Fatalf("JA3/JA4 口径的 version 应为 0x0303，实得 %d", spec.Version)
	}
	for _, c := range spec.CipherSuites {
		if isGREASE(c) {
			t.Fatalf("密码套件里残留 GREASE: %#x", c)
		}
	}
	for _, e := range spec.Extensions {
		if isGREASE(e) {
			t.Fatalf("扩展里残留 GREASE: %#x", e)
		}
	}
	if len(spec.CipherSuites) == 0 || len(spec.Extensions) == 0 {
		t.Fatal("套件或扩展不应为空")
	}
}

// TestRealClientHelloDiffersPerKernel 不同内核 → 不同指纹（否则 TLS 与声称版本脱钩，L2）。
func TestRealClientHelloDiffersPerKernel(t *testing.T) {
	cases := []string{"HelloChrome_115_PQ", "HelloChrome_120"}
	seen := map[string]string{}
	for _, name := range cases {
		p := NewTLSProxy(TLSProxyConfig{HelloID: name})
		_, hash, err := p.JA3()
		if err != nil {
			t.Fatalf("%s: JA3 失败: %v", name, err)
		}
		ja4, err := p.JA4()
		if err != nil {
			t.Fatalf("%s: JA4 失败: %v", name, err)
		}
		if ja4 == "" {
			t.Fatalf("%s: JA4 不应为空", name)
		}
		if src := p.SpecSource(); src != "real" {
			t.Fatalf("%s: 指纹应取自真实握手字节，实得来源=%s", name, src)
		}
		if prev, dup := seen[ja4]; dup {
			t.Fatalf("指纹不应重复: %s 与 %s 同为 %s", prev, name, ja4)
		}
		seen[ja4] = name
		_ = hash
	}
}

// TestHelloIDNameIsDeterministic 审计口径必须确定：同样参数永远报同一个基线名。
// uTLS 里 HelloChrome_Auto 与 HelloChrome_120 是同一个 ClientHelloID 值，
// 靠反查 map 取名字会因 Go map 随机迭代顺序摇摆——实测同样 `--kernel 120`
// 的进程会随机报 Auto 或 120，"哪个环境用了什么指纹"就不可核查了。
func TestHelloIDNameIsDeterministic(t *testing.T) {
	p := NewTLSProxy(TLSProxyConfig{HelloID: "HelloChrome_120"})
	for i := 0; i < 50; i++ {
		if got := p.HelloIDName(); got != "HelloChrome_120" {
			t.Fatalf("第 %d 次得到 %q（期望稳定为 HelloChrome_120）", i, got)
		}
	}
	if got := (NewTLSProxy(TLSProxyConfig{})).HelloIDName(); got == "" {
		t.Fatal("未指定 HelloID 时也必须给出一个名字")
	}
}
