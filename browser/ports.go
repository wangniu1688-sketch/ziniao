package browser

import (
	"fmt"
	"net"
)

// ListenLocal 在 127.0.0.1 上取一个 OS 分配的空闲端口并返回**已绑定**的 listener。
//
// 端口零竞态的关键：listener 一旦拿到就归调用方持有，直到交回给
// network.TLSProxy.ServeListener（M1 新增的变体）。绝不走「Listen → Close →
// 再 Listen(port)」那种先释放再绑定的写法——那个窗口里端口会被别的进程抢走，
// 症状是「偶发启动失败，重试就好」，最难复现的一类缺陷。
//
// 只绑回环：本地代理与 CDP 都是本机进程间通道，绑 0.0.0.0 等于把 MITM 代理
// 暴露给局域网内任何人。
func ListenLocal() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("browser: 本地端口分配失败: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		_ = ln.Close()
		return nil, 0, fmt.Errorf("browser: 本地端口解析失败: %v", ln.Addr())
	}
	return ln, addr.Port, nil
}
