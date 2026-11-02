package browser

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Injector 负责把指纹脚本挂到每个页面的 document_start。
//
// 为什么必须走 document_start 而不是加载后 eval：页面脚本在 document_start 之前
// 就跑完了指纹采集（window.screen / canvas / navigator），等我们再去改已经晚了——
// 平台读到的会是真实值，与「声称的内核画像」对不上，那正是要防的漂移。
type Injector struct {
	conn   *CDPConn
	script string
	log    *slog.Logger

	mu       sync.Mutex
	sessions map[string]string // sessionID → targetID（已注入过的页面）
}

// NewInjector 构造注入器。
func NewInjector(conn *CDPConn, script string, log *slog.Logger) *Injector {
	if log == nil {
		log = slog.Default()
	}
	return &Injector{conn: conn, script: script, log: log, sessions: map[string]string{}}
}

// InjectExistingTargets 对启动瞬间已存在的页面补注入一次，返回成功注入的页面数。
//
// 不 reload（批复 §11 第 6 条）：打断已打开的页面会让运营正在填的表单没了；
// M3 验收以新开 fp-test.html 为准，新开 target 走的是 document_start 正常挂载。
func (i *Injector) InjectExistingTargets(ctx context.Context) int {
	if i.script == "" {
		return 0
	}
	targets, err := i.conn.ListTargets(ctx)
	if err != nil {
		i.log.Warn("枚举已存在页面失败，跳过补注入", "err", err.Error())
		return 0
	}
	n := 0
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if err := i.InjectTarget(ctx, t.TargetID); err != nil {
			i.log.Warn("已有页面补注入失败", "target", t.TargetID, "err", err.Error())
			continue
		}
		n++
	}
	return n
}

// InjectTarget 挂到单个 target：attach → Page.enable → 注册 document_start。
func (i *Injector) InjectTarget(ctx context.Context, targetID string) error {
	sessionID, err := i.conn.AttachToTarget(ctx, targetID)
	if err != nil {
		return err
	}
	// Page.enable 重试一次：它是「attach 成功但浏览器还没回过神」时最容易
	// 撞上超时的调用，直接放弃会让这个页面少一份指纹脚本。
	//
	// 重试只在 ctx 还有余量时进行（retryCDP 内部会看 ctx.Done），
	// 所以不会把上层已经放弃的等待再拖长。
	if err := retryCDP(ctx, 2, 200*time.Millisecond, func(c context.Context) error {
		return i.conn.EnablePage(c, sessionID)
	}); err != nil {
		return fmt.Errorf("Page.enable 失败: %w", err)
	}
	if i.script != "" {
		if err := i.conn.AddScriptToEvaluateOnNewDocument(ctx, sessionID, i.script); err != nil {
			return fmt.Errorf("注入脚本注册失败: %w", err)
		}
	}
	i.mu.Lock()
	i.sessions[sessionID] = targetID
	i.mu.Unlock()
	return nil
}

// Watch 监听新页面的附着事件并补注入，直到 ctx 结束或连接断开。
//
// 这是「新开标签页也带指纹」的关键：启动后运营随手 Ctrl+T 开的页面，
// 走的正是 Target.attachedToTarget 这条路。
func (i *Injector) Watch(ctx context.Context) {
	if i.conn == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-i.conn.Closed():
			return
		case ev := <-i.conn.Events():
			at := ParseAttachedTarget(ev)
			if at == nil || at.Type != "page" || at.SessionID == "" {
				continue
			}
			if err := i.conn.EnablePage(ctx, at.SessionID); err != nil {
				i.log.Warn("新页面 Page.enable 失败", "target", at.TargetID, "err", err.Error())
				continue
			}
			if i.script != "" {
				if err := i.conn.AddScriptToEvaluateOnNewDocument(ctx, at.SessionID, i.script); err != nil {
					i.log.Warn("新页面注入失败", "target", at.TargetID, "err", err.Error())
					continue
				}
			}
			i.mu.Lock()
			i.sessions[at.SessionID] = at.TargetID
			i.mu.Unlock()
		}
	}
}

// Sessions 已注入的页面会话（停止时据此优雅关页）。
func (i *Injector) Sessions() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.sessions))
	for s := range i.sessions {
		out = append(out, s)
	}
	return out
}
