package network

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// RewriteHeaders 客户端->目标方向的身份头改写配置。
type RewriteHeaders struct {
	// UA 该环境声称的完整 User-Agent（必须与 TLS 基线的 Chrome 版本同源）。
	UA string
	// ChromeMajor 由 UA 解析出的大版本，用于生成 sec-ch-ua 的版本串。
	ChromeMajor int
	// Platform UA-CH 的 platform（UA 出什么就跟着出，通常是 "Windows"）。
	Platform string
}

// SecCHUAHeader 生成 sec-ch-ua 请求头值（Chrome 的真实格式：
// 含 Not_A Brand 占位、Chromium、Google Chrome 三条，顺序固定）。
func SecCHUAHeader(major int) string {
	if major <= 0 {
		return ""
	}
	return `"Not_A Brand";v="8", "Chromium";v="` + strconv.Itoa(major) +
		`", "Google Chrome";v="` + strconv.Itoa(major) + `"`
}

// copyRewritingRequests 在明文 HTTP/1.1 请求流上改写身份头后转发。
//
// 为什么必须做这一层：TLS 指纹可以由 uTLS 重建，JS 层的 navigator.userAgent /
// userAgentData 也能注入覆盖，但 **请求头里的 sec-ch-ua / User-Agent 是浏览器内核
// 在发出请求时自动加的**，JS 碰不到。不改写的话会出现「UA 说 120、请求头说 156」的
// 自相矛盾——等于主动告诉平台这个 UA 是被改过的。
//
// 限制：只覆盖 HTTP/1.1。HTTP/2 的头部是 HPACK 编码的帧，无法按字节改写，
// 需要在应用层解/编码或直接 fork Chromium；两者都已在 TODO 登记。
// h2Preface HTTP/2 连接前言。h2 的头部是 HPACK 编码的二进制帧，
// 无法按行改写——一旦拿 h1 的解析器去切帧，会把 SETTINGS 里的 0x0A 当换行
// 重写回去，直接把整条 h2 连接搞坏（实测现象是网页完全打不开）。
const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func copyRewritingRequests(dst io.Writer, src io.Reader, cfg RewriteHeaders) error {
	br := bufio.NewReader(src)
	// 先看是不是 HTTP/2：是则原样透传（不改写，但必须保证不破坏）
	if head, err := br.Peek(len(h2Preface)); err == nil {
		if string(head) == h2Preface {
			_, err := io.Copy(dst, br)
			return err
		}
	} else if err != io.EOF {
		// 连前言长度都读不到，数据已不可用：保守交给对端处理
		_, err := io.Copy(dst, br)
		return err
	}
	for {
		// 空行是 keep-alive 连接的常见尾随，吃掉再继续读下一个请求
		reqLine, err := readLine(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if reqLine == "" {
			continue
		}
		headers, err := readHeaders(br)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(dst, reqLine+"\r\n"); err != nil {
			return err
		}
		for _, h := range rewriteRequestHeaders(headers, cfg) {
			if _, err := io.WriteString(dst, h+"\r\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(dst, "\r\n"); err != nil {
			return err
		}
		// body 必须按原 framing 转发：改写只换头部取值，不改分帧方式
		if err := copyBody(dst, br, headers); err != nil {
			return err
		}
	}
}

// rewriteRequestHeaders 替换/补齐身份相关头。保持其它头不变（含顺序）。
func rewriteRequestHeaders(headers []string, cfg RewriteHeaders) []string {
	if cfg.UA == "" {
		return headers
	}
	out := make([]string, 0, len(headers)+3)
	hasUA, hasMobile, hasPlatform, hasBrand := false, false, false, false
	for _, h := range headers {
		name := strings.ToLower(headerName(h))
		switch name {
		case "user-agent":
			if !hasUA {
				out = append(out, "User-Agent: "+cfg.UA)
				hasUA = true
			}
		case "sec-ch-ua":
			out = append(out, "sec-ch-ua: "+SecCHUAHeader(cfg.ChromeMajor))
			hasBrand = true
		case "sec-ch-ua-mobile":
			out = append(out, "sec-ch-ua-mobile: ?0")
			hasMobile = true
		case "sec-ch-ua-platform":
			out = append(out, "sec-ch-ua-platform: \""+cfg.Platform+"\"")
			hasPlatform = true
		default:
			out = append(out, h)
		}
	}
	// 补齐缺失项：Chrome 在有 UA-CH 时这三个头总是成套出现，缺一个就是异常信号
	if !hasUA {
		out = append(out, "User-Agent: "+cfg.UA)
	}
	if !hasBrand {
		out = append(out, "sec-ch-ua: "+SecCHUAHeader(cfg.ChromeMajor))
	}
	if !hasMobile {
		out = append(out, "sec-ch-ua-mobile: ?0")
	}
	if !hasPlatform {
		platform := cfg.Platform
		if platform == "" {
			platform = "Windows"
		}
		out = append(out, "sec-ch-ua-platform: \""+platform+"\"")
	}
	return out
}

// copyBody 按 Content-Length / chunked 转发请求体，并停在下一个请求边界前。
func copyBody(dst io.Writer, br *bufio.Reader, headers []string) error {
	var cl int64 = -1
	chunked := false
	for _, h := range headers {
		switch strings.ToLower(headerName(h)) {
		case "content-length":
			v := strings.TrimSpace(headerValue(h))
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				cl = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(headerValue(h)), "chunked") {
				chunked = true
			}
		}
	}
	switch {
	case chunked:
		// 逐块读：块大小行 + 数据 + CRLF，直到终止块（size=0）
		for {
			sizeLine, err := readLine(br)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(dst, sizeLine+"\r\n"); err != nil {
				return err
			}
			size := parseChunkSize(sizeLine)
			if size == 0 {
				// 终止块后面是可选 trailer，直到空行
				for {
					l, err := readLine(br)
					if err != nil || l == "" {
						_, err2 := io.WriteString(dst, "\r\n")
						return err2
					}
					if _, err := io.WriteString(dst, l+"\r\n"); err != nil {
						return err
					}
				}
			}
			buf := make([]byte, size+2) // data + 尾随 CRLF
			if _, err := io.ReadFull(br, buf); err != nil {
				return err
			}
			if _, err := dst.Write(buf); err != nil {
				return err
			}
		}
	case cl > 0:
		_, err := io.CopyN(dst, br, cl)
		return err
	default:
		return nil
	}
}

func parseChunkSize(line string) int64 {
	hexPart := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
	if hexPart == "" {
		return 0
	}
	v, err := strconv.ParseInt(hexPart, 16, 64)
	if err != nil {
		return 0
	}
	return v
}

func readLine(br *bufio.Reader) (string, error) {
	s, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	s = strings.TrimRight(s, "\r\n")
	return s, err
}

func readHeaders(br *bufio.Reader) ([]string, error) {
	out := []string{}
	for {
		line, err := readLine(br)
		if err != nil {
			return out, err
		}
		if line == "" {
			return out, nil
		}
		out = append(out, line)
	}
}

func headerName(h string) string {
	if i := strings.Index(h, ":"); i > 0 {
		return h[:i]
	}
	return h
}

func headerValue(h string) string {
	if i := strings.Index(h, ":"); i > 0 {
		return strings.TrimSpace(h[i+1:])
	}
	return ""
}
