package fingerprint

import (
	"strconv"
	"strings"
)

// KernelAlignment 内核版本对齐结论：一个环境最终声称的 Chrome 版本从哪来、为什么。
type KernelAlignment struct {
	Major      int          // 生效内核大版本：UA / UA-CH / TLS 握手三者同源于此
	Version    string       // 生效完整版本串（UA-CH 的 uaFullVersion 用它）
	Baseline   *TLSBaseline // 生效 TLS 基线（nil：基线库无条目，未做对齐）
	LocalMajor int          // 本机浏览器内核大版本（0 = 未探测到）
	// Note 为什么不是本机内核版本。为空表示「已对齐到本机内核」。
	// 这一句要能直接给运营看：否则「为什么美国店的 Chrome 是 120」只能靠猜。
	Note string
}

// AlignKernel 把画像的 Chrome 版本对齐到「本机内核」与「TLS 基线库」共同允许的最高版本，
// 并把 UA / UA-CH 一并改到同一个版本上。
//
// 三条规则，冲突时**后者优先**（顺序即优先级）：
//
//  1. 不得声称高于本机内核大版本（localMajor>0 时生效）：本机跑的是 156，
//     却声称一个不存在的更新版本，等于把「UA 是改过的」写在脸上；
//  2. 不得与代理实际发出的 ClientHello 矛盾：基线库里 131 用的是 HelloChrome_120_PQ，
//     真正发出的是 120，UA 就必须写成 120——宁可用低版本但完全自洽，
//     也不要高版本混搭（UA 与 JA4 交叉比对是致命不自洽）；
//  3. 在前两条之内取**最高**可用版本：能对齐 131 就别无谓地把自己伪装成 115。
//
// 画像为 nil / UA 无法解析 / 基线库为空时原样不动（只回填 Note），
// 保证「对齐」永远是增强而不是把环境搞坏。
func AlignKernel(p *Profile, lib *BaselineLibrary, localMajor int) KernelAlignment {
	if p == nil {
		return KernelAlignment{Note: "画像为空，未做内核对齐"}
	}
	major, err := p.ChromeMajor()
	if err != nil || major <= 0 {
		return KernelAlignment{Major: major, LocalMajor: localMajor,
			Note: "画像 UA 无法解析 Chrome 版本，未做内核对齐"}
	}
	avail := lib.Majors()
	if len(avail) == 0 {
		return KernelAlignment{Major: major, LocalMajor: localMajor,
			Note: "TLS 基线库为空，未做内核对齐"}
	}

	notes := []string{}
	// 规则 1：上限 = min(画像版本, 本机内核)
	ceiling := major
	if localMajor > 0 && localMajor < ceiling {
		ceiling = localMajor
	}
	// 规则 3：取不超过上限的最高条目
	chosen := 0
	for _, m := range avail {
		if m <= ceiling {
			chosen = m
		}
	}
	if chosen == 0 {
		// 本机内核比基线库最低条目还老：仍不能违反规则 2，只能往上取最低条目
		chosen = avail[0]
		notes = append(notes, "本机内核 v"+strconv.Itoa(localMajor)+
			" 低于基线库最低条目 v"+strconv.Itoa(chosen)+"：宁可版本偏新也不与握手矛盾")
	}
	bl, _ := lib.For(chosen)
	if bl == nil {
		return KernelAlignment{Major: major, LocalMajor: localMajor,
			Note: strings.Join(notes, "；")}
	}

	// 规则 2：认出这条基线**实际发出**的握手版本
	eff := ParseChromeMajorFromHelloID(bl.HelloID)
	if eff > 0 && eff != chosen {
		if b2, ok := lib.For(eff); ok {
			notes = append(notes, "内核基线 v"+strconv.Itoa(chosen)+" 复用 "+bl.HelloID+
				"（实际发出的是 v"+strconv.Itoa(eff)+" 的握手）")
			bl, chosen = b2, eff
		}
	}
	if eff <= 0 {
		eff = chosen
	}
	if localMajor > 0 && eff < localMajor {
		notes = append(notes, "本机 Chrome v"+strconv.Itoa(localMajor)+" 高于可用基线 v"+
			strconv.Itoa(eff)+"：uTLS 基线滞后于 Chrome 正式版，UA 与握手必须同源，一并对齐到 v"+
			strconv.Itoa(eff))
	}

	AlignProfileChromeVersion(p, bl.ChromiumVersion, eff)
	return KernelAlignment{Major: eff, Version: bl.ChromiumVersion, Baseline: bl,
		LocalMajor: localMajor, Note: strings.Join(notes, "；")}
}

// AlignProfileChromeVersion 把画像的 UA 与 UA-CH 一并对齐到同一个 Chrome 版本。
//
// 只改 UA 不改 UA-CH 是自洽性事故：JS 里 navigator.userAgentData 说 131、UA 说 120，
// 平台把两个来源交叉一比就知道指纹是拼出来的（自洽性规则 uach-ua 查的正是这一条）。
//
// version 是完整版本串（UA-CH 用），UA 里写的是桌面 Chrome 的冻结版本（M110 起
// UA 只出大版本，如 "120.0.0.0"）——两者口径不同，不能互相替代。
func AlignProfileChromeVersion(p *Profile, version string, major int) {
	if p == nil || version == "" {
		return
	}
	if _, err := chromeMajorFromUA(p.UA); err != nil {
		return // UA 里没有 Chrome 段（Firefox 之类）：改不动就不要碰
	}
	if major <= 0 {
		if i := strings.Index(version, "."); i > 0 {
			major, _ = strconv.Atoi(version[:i])
		}
	}
	if major <= 0 {
		return
	}
	p.UA = AlignUAChromeVersion(p.UA, UAFrozenVersion(major))
	p.ChromeFullVersion = version
	// Not_A Brand 的小版本随 Chrome 大版本变（131 起是 24，老版本是 8）。
	notA := "8"
	if major >= 125 {
		notA = "24"
	}
	p.UACH.Brands = []string{
		`"Not_A Brand";v="` + notA + `"`,
		`"Chromium";v="` + strconv.Itoa(major) + `"`,
		`"Google Chrome";v="` + strconv.Itoa(major) + `"`,
	}
	p.UACH.FullVersionList = []string{
		`"Not_A Brand";v="8.0.0.0"`,
		`"Chromium";v="` + version + `"`,
		`"Google Chrome";v="` + version + `"`,
	}
}

// UAFrozenVersion 桌面 Chrome 在 UA 里使用的版本串。
//
// Chrome 自 M110 起把桌面 UA 的版本冻结成大版本（"120.0.0.0"）以削弱 UA 熵，
// 完整版本只保留在 UA-CH 的 uaFullVersion 里。写成 "120.0.6099.109" 是
// 真实桌面 Chrome 不会输出的组合（那是 M110 之前 / 移动端口径）。
func UAFrozenVersion(major int) string {
	if major <= 0 {
		return ""
	}
	return strconv.Itoa(major) + ".0.0.0"
}
