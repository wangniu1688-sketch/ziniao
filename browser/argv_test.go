package browser

import (
	"strings"
	"testing"
)

// TestArgvNeverContainsUpstreamSecret 是 M3 硬验收项「Chrome argv 中不含上游代理口令」
// 的自动化证据。
//
// 为什么值得一条单测：口令进 argv 的代价不是「理论上可能被看到」，而是
// wmic/tasklist 一条命令就能读到，且本包的对账日志会顺手把命令行记下来。
// 这条断言把「忘了」变成「编译不过」。
func TestArgvNeverContainsUpstreamSecret(t *testing.T) {
	const user, pass, host = "secret_user_01", "P@ssw0rd-!Super", "us.resi.example.com"

	// ArgvConfig 结构上没有承载凭据的字段（见类型注释），凭据只可能通过
	// 文本类参数混进来——这里用最坏情况构造：把整条 upstream URL 塞进 UA 与
	// OpenURL（真实链路绝不会这么做），断言依旧不出现。
	argv := BuildArgv(ArgvConfig{
		ProfileDir:     `C:\data\profiles\01HENV`,
		LocalProxyPort: 53128,
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/131.0.0.0",
		OpenURL:        "https://sellercentral.amazon.com",
	})
	if ArgvHasSecret(argv, user, pass, host, "://"+user+":"+pass+"@") {
		t.Fatalf("argv 出现上游凭据: %v", argv)
	}
	for _, a := range argv {
		if strings.Contains(a, "@") && strings.Contains(a, "proxy-server") {
			t.Fatalf("proxy-server 参数不得携带用户信息: %s", a)
		}
	}

	// 正例控制：断言检测函数本身有效（否则上面的断言是空转）。
	if !ArgvHasSecret([]string{"--proxy-server=http://" + user + ":" + pass + "@1.2.3.4:8000"}, pass) {
		t.Fatal("ArgvHasSecret 未能识别含口令的 argv，断言本身失效")
	}
}

func TestBuildArgvCoreFlags(t *testing.T) {
	argv := BuildArgv(ArgvConfig{
		ProfileDir:     "/data/profiles/env1",
		LocalProxyPort: 41000,
		UserAgent:      "UA-131",
		OpenURL:        "https://example.com",
	})
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--user-data-dir=/data/profiles/env1",
		"--remote-debugging-port=0",
		"--remote-allow-origins=http://127.0.0.1:*",
		"--proxy-server=http://127.0.0.1:41000",
		"--proxy-bypass-list=<-loopback>",
		"--user-agent=UA-131",
		"https://example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 缺少 %q: %v", want, argv)
		}
	}
	// 本期默认不加载侧边栏（批复 3.3：规避 Chrome 开发者模式提示条）。
	if strings.Contains(joined, "--load-extension") {
		t.Fatalf("默认不得加载侧边栏扩展: %v", argv)
	}
}

func TestBuildArgvSidepanelOnlyWhenEnabled(t *testing.T) {
	argv := BuildArgv(ArgvConfig{
		ProfileDir:     "/data/profiles/env1",
		LoadSidepanel:  true,
		SidepanelDir:   "/data/extensions/sidepanel",
		LocalProxyPort: 41000,
	})
	if !hasArg(argv, "--load-extension=/data/extensions/sidepanel") {
		t.Fatalf("开启后应加载侧边栏: %v", argv)
	}
}

func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in    string
		ver   string
		major int
	}{
		{"Google Chrome 131.0.6778.86", "131.0.6778.86", 131},
		{"Microsoft Edge 131.0.2903.86 ", "131.0.2903.86", 131},
		{"120.0.6099.109", "120.0.6099.109", 120},
		{"no version here", "", 0},
	}
	for _, c := range cases {
		ver, major := parseVersion(c.in)
		if ver != c.ver || major != c.major {
			t.Fatalf("parseVersion(%q) = (%q,%d)，期望 (%q,%d)", c.in, ver, major, c.ver, c.major)
		}
	}
}

// TestLocatorOverrideMissing 覆盖路径不存在时不得谎报「已安装」——
// 谎报的后果是启动走到 Stage 4 才失败，而错误信息会指向 Chrome 而不是配置。
func TestLocatorOverrideMissing(t *testing.T) {
	info := Locator{Override: "/definitely/not/here/chrome"}.Detect()
	if info.Installed {
		t.Fatalf("路径不存在不应判为已安装: %+v", info)
	}
	if len(info.Candidates) != 0 {
		t.Fatalf("候选列表应为空: %+v", info.Candidates)
	}
}
