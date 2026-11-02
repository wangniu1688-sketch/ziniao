package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// CDP 客户端：只实现启动器真正用到的那几个方法。
//
// 选型（M1 前置设计 §11 批复第 1 条）：优先 `github.com/gorilla/websocket`，
// HTTP 层的 /json/version、/json/list 仍走标准库 net/http。
// 用它而不是自研 RFC6455 客户端，是因为分片重组、掩码、ping/pong 这些细节
// 恰恰是最容易在「自研 200 行」里漏掉、又在现场最难复现的部分。
//
// 红线：**注入脚本正文只经这条通道传递**，不落 argv、不落日志、不落 HTTP 响应。

// ErrCDPClosed 连接已关闭（进程退出 / 主动断开）。
var ErrCDPClosed = errors.New("browser: CDP 连接已关闭")

// cdpEnvelope CDP 消息信封：请求与响应同构，靠 id / method 区分。
type cdpEnvelope struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// CDPEvent 一条服务端事件（Target.attachedToTarget 等）。
type CDPEvent struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// CDPConn 一条到浏览器（或某个 target 会话）的 WebSocket 连接。
//
// 并发模型：发送侧一把锁（gorilla 不允许并发写）；响应按 id 分发到各自的
// channel；事件进一个带缓冲的 channel，**满了就丢**——事件用于「发现新页面并注入」，
// 丢一条最多是那个页面没被注入，而阻塞读循环会让所有 Call 一起超时。
type CDPConn struct {
	ws *websocket.Conn

	sendMu sync.Mutex
	nextID int64

	mu      sync.Mutex
	pending map[int64]chan *cdpEnvelope

	events chan CDPEvent

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

// DialCDP 连接 CDP WebSocket。
func DialCDP(ctx context.Context, url string) (*CDPConn, error) {
	d := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.DialContext(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("browser: CDP 连接失败: %w", err)
	}
	c := &CDPConn{
		ws:      ws,
		pending: map[int64]chan *cdpEnvelope{},
		events:  make(chan CDPEvent, 64),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Call 发起一次 CDP 调用并等待结果（sessionID 为空 = 浏览器级）。
func (c *CDPConn) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("browser: CDP 参数序列化失败 %s: %w", method, err)
		}
		raw = b
	}
	id := c.newID()
	ch := make(chan *cdpEnvelope, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return nil, ErrCDPClosed
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer c.forget(id)

	if err := c.send(cdpEnvelope{ID: id, Method: method, SessionID: sessionID, Params: raw}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("browser: CDP 调用超时 %s: %w", method, ctx.Err())
	case <-c.closed:
		return nil, ErrCDPClosed
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("browser: CDP %s 返回错误(%d): %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// Events 事件流（只读）。
func (c *CDPConn) Events() <-chan CDPEvent { return c.events }

// Closed 连接是否已关闭。
func (c *CDPConn) Closed() <-chan struct{} { return c.closed }

// Close 关闭连接（幂等）。
//
// 先发 close 帧再关 socket：粗暴关 TCP 会让 Chrome 侧的 DevTools 会话残留到超时
// （表现为下次启动同一 profile 时端口迟迟不放）。
func (c *CDPConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.sendMu.Lock()
		_ = c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
		c.closeErr = c.ws.Close()
		c.sendMu.Unlock()
		c.mu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			close(ch)
		}
		c.mu.Unlock()
	})
	return c.closeErr
}

func (c *CDPConn) newID() int64 {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *CDPConn) send(env cdpEnvelope) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	select {
	case <-c.closed:
		return ErrCDPClosed
	default:
	}
	if err := c.ws.WriteJSON(env); err != nil {
		return fmt.Errorf("browser: CDP 发送失败: %w", err)
	}
	return nil
}

func (c *CDPConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *CDPConn) readLoop() {
	for {
		var env cdpEnvelope
		if err := c.ws.ReadJSON(&env); err != nil {
			_ = c.Close()
			return
		}
		if env.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[env.ID]
			if ok {
				delete(c.pending, env.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- &env
			}
			continue
		}
		if env.Method == "" {
			continue
		}
		ev := CDPEvent{Method: env.Method, SessionID: env.SessionID, Params: env.Params}
		select {
		case c.events <- ev:
		default:
			// 事件队列满：丢弃并继续。见 CDPConn 注释。
		}
	}
}

// ---------- 具体方法封装 ----------

// TargetInfo 一个 CDP target。
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// ListTargets Target.getTargets。
func (c *CDPConn) ListTargets(ctx context.Context) ([]TargetInfo, error) {
	res, err := c.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("browser: 解析 target 列表失败: %w", err)
	}
	return out.TargetInfos, nil
}

// AttachToTarget Target.attachToTarget（flatten=true：消息扁平化）。
// 返回 sessionId，后续针对该页面的调用都要带上它。
func (c *CDPConn) AttachToTarget(ctx context.Context, targetID string) (string, error) {
	res, err := c.Call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true})
	if err != nil {
		return "", err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("browser: 解析 attach 结果失败: %w", err)
	}
	return out.SessionID, nil
}

// EnableAutoAttach Target.setAutoAttach：让**之后**新建的页面自动挂上来，
// 这样新开标签页也能在 document_start 拿到注入脚本。
func (c *CDPConn) EnableAutoAttach(ctx context.Context) error {
	_, err := c.Call(ctx, "", "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	})
	return err
}

// EnablePage Page.enable（注入前必须开）。
func (c *CDPConn) EnablePage(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, sessionID, "Page.enable", nil)
	return err
}

// AddScriptToEvaluateOnNewDocument 注册 document_start 注入。
//
// 这是指纹注入的**唯一**通道：脚本正文既不进 argv，也不进任何 HTTP 响应
// （环境详情只回 script_bytes），只在 Go 进程内存与这条 WS 通道里存在。
func (c *CDPConn) AddScriptToEvaluateOnNewDocument(ctx context.Context, sessionID, source string) error {
	_, err := c.Call(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": source})
	return err
}

// Navigate Page.navigate：失败不算启动失败（页面打不开不等于环境不可用）。
func (c *CDPConn) Navigate(ctx context.Context, sessionID, url string) error {
	_, err := c.Call(ctx, sessionID, "Page.navigate", map[string]any{"url": url})
	return err
}

// ClosePage Page.close（优雅关页）。
func (c *CDPConn) ClosePage(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, sessionID, "Page.close", nil)
	return err
}

// CaptureScreenshot Page.captureScreenshot：停止前留一张缩略图（M3/T10）。
func (c *CDPConn) CaptureScreenshot(ctx context.Context, sessionID string) ([]byte, error) {
	res, err := c.Call(ctx, sessionID, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("browser: 解析截图结果失败: %w", err)
	}
	if out.Data == "" {
		return nil, errors.New("browser: 截图为空")
	}
	return base64.StdEncoding.DecodeString(out.Data)
}

// CloseBrowser Browser.close（优雅关不掉时的兜底）。
func (c *CDPConn) CloseBrowser(ctx context.Context) error {
	_, err := c.Call(ctx, "", "Browser.close", nil)
	return err
}

// AttachedTarget 从 Target.attachedToTarget 事件里解析出的页面会话。
type AttachedTarget struct {
	SessionID string `json:"sessionId"`
	TargetID  string `json:"targetId"`
	Type      string `json:"type"`
	URL       string `json:"url"`
}

// ParseAttachedTarget 解析附着事件（解析不出来返回 nil，调用方忽略即可）。
func ParseAttachedTarget(ev CDPEvent) *AttachedTarget {
	if ev.Method != "Target.attachedToTarget" {
		return nil
	}
	var p struct {
		SessionID  string     `json:"sessionId"`
		TargetInfo TargetInfo `json:"targetInfo"`
	}
	if err := json.Unmarshal(ev.Params, &p); err != nil {
		return nil
	}
	return &AttachedTarget{
		SessionID: p.SessionID, TargetID: p.TargetInfo.TargetID,
		Type: p.TargetInfo.Type, URL: p.TargetInfo.URL,
	}
}
