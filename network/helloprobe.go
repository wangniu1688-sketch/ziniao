package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"

	"ziniao/fingerprint"
)

// RealClientHello 抓取「uTLS 真正会发出去的 ClientHello 原始字节」并解析成指纹描述。
//
// 为什么不能再只用 UTLSIdToSpec 静态计算：静态 spec 的转换会丢扩展、排错顺序、
// 漏掉 uTLS 在握手时动态插入的内容，算出的 JA3 与线上实际字节并不一致——
// 而电商平台看到的是字节，不是 spec。基线校验必须基于真实字节（否则等于自欺欺人）。
//
// 做法：用一对内存管道让 uTLS 真发一次 ClientHello，在另一端把字节读出来解析。
func RealClientHello(id utls.ClientHelloID) (fingerprint.ClientHelloSpec, error) {
	clientEnd, serverEnd := net.Pipe()
	defer func() { _ = clientEnd.Close() }()
	defer func() { _ = serverEnd.Close() }()

	cfg := &utls.Config{ServerName: "localhost", InsecureSkipVerify: true} //nolint:gosec // 仅本地自采样，不产生真实流量
	uconn := utls.UClient(clientEnd, cfg, id)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = uconn.HandshakeContext(ctx) // 对端不回 ServerHello，握手必然失败；我们只要它发出的字节
		_ = clientEnd.Close()
	}()

	if err := serverEnd.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fingerprint.ClientHelloSpec{}, err
	}
	rec := make([]byte, 5)
	if _, err := io.ReadFull(serverEnd, rec); err != nil {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: 读取握手记录失败: %w", err)
	}
	if rec[0] != 0x16 { // handshake
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: 非握手记录 type=0x%02x", rec[0])
	}
	n := int(binary.BigEndian.Uint16(rec[3:5]))
	body := make([]byte, n)
	if _, err := io.ReadFull(serverEnd, body); err != nil {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: 读取 ClientHello 失败: %w", err)
	}
	return ParseClientHello(body)
}

// isGREASE 判断是否为 GREASE 值（0x?a?a 形态）。JA3/JA4 规范必须排除，
// 因为这些值每次随机，否则指纹不可复现。
func isGREASE(v uint16) bool {
	return v&0x0f0f == 0x0a0a && byte(v>>8) == byte(v)
}

// ParseClientHello 解析 ClientHello 握手体（不含 record header）为指纹描述。
func ParseClientHello(body []byte) (fingerprint.ClientHelloSpec, error) {
	// handshake header: msg_type(1) length(3)
	if len(body) < 4 || body[0] != 0x01 {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: 不是 ClientHello (type=0x%02x)", body[0])
	}
	p := 4
	if len(body) < p+2+32+1 {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: ClientHello 过短")
	}
	out := fingerprint.ClientHelloSpec{Version: binary.BigEndian.Uint16(body[p : p+2])}
	p += 2 + 32 // legacy_version + random

	sidLen := int(body[p])
	p += 1 + sidLen
	if p+2 > len(body) {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: session_id 越界")
	}
	csLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2
	if p+csLen > len(body) {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: cipher_suites 越界")
	}
	for i := 0; i+1 < csLen; i += 2 {
		c := binary.BigEndian.Uint16(body[p+i : p+i+2])
		if isGREASE(c) {
			continue
		}
		out.CipherSuites = append(out.CipherSuites, c)
	}
	p += csLen

	if p >= len(body) {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: 缺少 compression_methods")
	}
	p += 1 + int(body[p]) // compression_methods
	if p+2 > len(body) {
		return fingerprint.ClientHelloSpec{}, fmt.Errorf("network: extensions 越界")
	}
	extLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2
	end := p + extLen
	if end > len(body) {
		end = len(body)
	}

	for p+4 <= end {
		typ := binary.BigEndian.Uint16(body[p : p+2])
		l := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		p += 4
		if p+l > end {
			break
		}
		data := body[p : p+l]
		p += l
		if isGREASE(typ) {
			continue
		}
		out.Extensions = append(out.Extensions, typ)
		switch typ {
		case 10: // supported_groups
			if len(data) < 2 {
				continue
			}
			n := int(binary.BigEndian.Uint16(data[0:2]))
			for i := 2; i+1 < len(data) && len(out.Curves) < n; i += 2 {
				g := binary.BigEndian.Uint16(data[i : i+2])
				if isGREASE(g) {
					continue
				}
				out.Curves = append(out.Curves, g)
			}
		case 11: // ec_point_formats
			out.PointFormats = append(out.PointFormats, data...)
		case 13: // signature_algorithms
			if len(data) < 2 {
				continue
			}
			n := int(binary.BigEndian.Uint16(data[0:2]))
			for i := 2; i+1 < len(data) && len(out.SigAlgs) < n; i += 2 {
				out.SigAlgs = append(out.SigAlgs, binary.BigEndian.Uint16(data[i:i+2]))
			}
		case 16: // ALPN
			for i := 2; i < len(data); {
				if i+1 > len(data) {
					break
				}
				l := int(data[i])
				i++
				if i+l > len(data) {
					break
				}
				out.ALPN = append(out.ALPN, string(data[i:i+l]))
				i += l
			}
		case 43: // supported_versions：JA3/JA4 的 version 口径是 ClientHello 里的
			// legacy_version（恒 0x0303），不能拿 supported_versions 的覆盖面覆盖——
			// 那里面混着 GREASE(0x?a?a) 值，取最大值会算出 27242 这种假版本。
		}
	}
	return out, nil
}

// RealJA3FromHelloID 又快又准：直接抓真实字节算 JA3（raw, hash）。
func RealJA3FromHelloID(id utls.ClientHelloID) (string, string, error) {
	spec, err := RealClientHello(id)
	if err != nil {
		return "", "", err
	}
	raw, hash := fingerprint.JA3From(spec)
	return raw, hash, nil
}
