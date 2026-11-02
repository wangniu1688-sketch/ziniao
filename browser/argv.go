package browser

import "strconv"

// ArgvConfig 构造 Chrome 启动参数所需的全部输入。
//
// 这个结构**没有、也永远不允许有**上游代理账号/口令字段：
// 上游凭据只存在于 Go 进程内存里，通过本地 TLSProxy 的上游配置生效
// （network.TLSProxyConfig），不进 argv、不进日志、不进审计、不进响应。
// 把口令放进 argv 的代价是：任何进程都能用 wmic/tasklist 读到命令行，
// 而命令行会被记进对账日志——等于把凭据抄送给了所有能看日志的人。
type ArgvConfig struct {
	ProfileDir string // data/profiles/<env-id>
	// LocalProxyPort 本地 MITM 代理端口（由 ListenLocal 分配后传入）；
	// <=0 表示不挂代理（仅用于排障，正式链路必带）。
	LocalProxyPort int
	UserAgent      string // UA 不是机密，允许出现在 argv
	OpenURL        string // 末尾位置参数；为空则不传
	WindowSize     string // 默认 1280,860
	// LoadSidepanel 是否加载侧边栏扩展。默认 false：Chrome 加载未打包扩展会弹
	// 「请停用开发者模式扩展」提示条，干扰指纹验收观感（批复 3.3）。
	LoadSidepanel bool
	SidepanelDir  string
}

// BuildArgv 构造 Chrome 启动参数。
//
// 指纹注入**不通过命令行**：脚本正文经 CDP 的
// Page.addScriptToEvaluateOnNewDocument 在 document_start 注入（M3），
// 命令行里只有 UA、窗口尺寸这类非机密项。
//
// ---------- 参数清单（M2 补查项：逐条确认不含凭据）----------
//
//	--user-data-dir=<data/profiles/<env-id>> 独立 profile，环境之间不共享 Cookie/存储
//	--remote-debugging-port=0                CDP 端口由 OS 分配，从 DevToolsActivePort 读回
//	--remote-allow-origins=http://127.0.0.1:*  只放行本机来源（CDP 是完整浏览器控制权）
//	--proxy-bypass-list=<-loopback>          回环不过代理，避免浏览器自连接本地代理
//	--no-first-run / --no-default-browser-check / --disable-sync   去掉首次运行引导与账号同步
//	--disable-background-networking / --disable-component-update /
//	--disable-domain-reliability / --metrics-recording-only / --disable-breakpad
//	    减少与 Google 的无关连接：那些请求会污染出口画像
//	--window-size=1280,860                   窗口尺寸（指纹一致性）
//	--proxy-server=http://127.0.0.1:<port>   本地 TLSProxy（**只写地址，绝不写 user:pass@**）
//	--user-agent=<UA>                        UA 不是机密，允许出现在 argv
//	--load-extension=<sidepanel>（默认关闭） 侧边栏扩展，本期默认不加载
//	<open-url>                               末尾位置参数；为空则不传
//
// 上游代理账号与口令：只存在于 Go 进程内存（network.TLSProxyConfig 的上游配置），
// 通过本机 TLSProxy 生效——**不进 argv、不进日志、不进审计 detail、不进 HTTP 响应**。
// 理由：任何进程都能用 wmic/tasklist 读到命令行，命令行还会被记进对账日志。
// 自检入口见 ArgvHasSecret（M3 硬验收项）。
func BuildArgv(cfg ArgvConfig) []string {
	size := cfg.WindowSize
	if size == "" {
		size = "1280,860"
	}
	argv := []string{
		"--user-data-dir=" + cfg.ProfileDir,
		// CDP 端口由 OS 分配后从 <profile>/DevToolsActivePort 读回（方案 A），
		// 避免静态端口段的冲突与「释放→绑定」竞态。
		"--remote-debugging-port=0",
		// 只放行本机来源，绝不放行任意 origin（CDP 是完整的浏览器控制权）。
		"--remote-allow-origins=http://127.0.0.1:*",
		// 回环不过代理：否则浏览器连本地代理本身也会被送去代理，形成自连接。
		"--proxy-bypass-list=<-loopback>",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		// 减少与 Google 的无关连接：那些请求会污染出口画像，
		// 让「这个环境看起来像一台刚装好的浏览器」变成一句空话。
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-domain-reliability",
		"--metrics-recording-only",
		"--disable-breakpad",
		"--window-size=" + size,
	}
	if cfg.LocalProxyPort > 0 {
		// 只写 127.0.0.1:port，**绝不**写成 http://user:pass@host:port。
		argv = append(argv, "--proxy-server=http://127.0.0.1:"+strconv.Itoa(cfg.LocalProxyPort))
	}
	if cfg.UserAgent != "" {
		argv = append(argv, "--user-agent="+cfg.UserAgent)
	}
	if cfg.LoadSidepanel && cfg.SidepanelDir != "" {
		argv = append(argv, "--load-extension="+cfg.SidepanelDir)
	}
	if cfg.OpenURL != "" {
		argv = append(argv, cfg.OpenURL)
	}
	return argv
}

// ArgvHasSecret 断言参数里是否出现给定机密串（M3 硬验收项的自检入口）。
//
// 单独导出是为了让「argv 不含上游口令」这条红线有可调用的断言，而不是靠
// 人眼扫一遍字符串拼接——字符串拼接会随版本增长，扫描不会。
func ArgvHasSecret(argv []string, secrets ...string) bool {
	for _, a := range argv {
		for _, s := range secrets {
			if s != "" && contains(a, s) {
				return true
			}
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
