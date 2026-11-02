package browser

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ChromeProc 一个已被拉起的浏览器进程句柄。
//
// 抽象成接口而不是直接用 *exec.Cmd，只有一个理由：启动链路的 9 个阶段里
// 有 6 个失败分支要验证回滚，单测不可能真开 Chrome——注入一个假进程，
// 才能把「端口释放 / 进程收掉 / 状态复位」逐条断言下来。
type ChromeProc interface {
	// PID 操作系统进程号（登记进运行时注册表，对账靠它判断死活）。
	PID() int
	// Done 进程退出后关闭（用于「崩溃自动复位」）。
	Done() <-chan struct{}
	// Terminate 强制结束（优雅关不掉时的兜底：Windows 是 TerminateProcess）。
	Terminate() error
}

// procStarter 拉起浏览器的动作（默认走 exec，单测替换成假实现）。
type procStarter func(exe string, argv []string) (ChromeProc, error)

type execProc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	exitErr error
}

// StartChrome 拉起浏览器进程并立即返回（不等待退出）。
//
// 不接管 cmd.Stdout/Stderr：Chrome 的调试输出会把 CDP URL 与内部报错全量打出来，
// 接进我们的日志等于把一份不可控的文本流引进审计域；进程本身也不需要与我们通信
// （一切控制走 CDP）。
func StartChrome(exe string, argv []string) (ChromeProc, error) {
	if exe == "" {
		return nil, fmt.Errorf("browser: 浏览器路径为空")
	}
	cmd := exec.Command(exe, argv...) //nolint:gosec // 路径来自本机探测或管理员显式配置
	setProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("browser: 拉起浏览器失败: %w", err)
	}
	p := &execProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *execProc) PID() int { return p.cmd.Process.Pid }

func (p *execProc) Done() <-chan struct{} { return p.done }

func (p *execProc) Terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil // 已经退出了，不必再杀（杀错 PID 的风险永远是第一位的）
	default:
	}
	return p.cmd.Process.Kill()
}

// waitProcExit 等待进程**真正**退出，最多等 timeout；返回是否已退出。
//
// 为什么不能「发完信号就返回」：Chrome 的退出是一串动作（关窗口 → 写回 profile →
// 删自己的单实例锁），强杀之后它还要几百毫秒才真正消失。在它消失之前去删残留文件，
// 要么删不掉（Windows 独占锁），要么删完又被正在退出的进程写回——下一轮启动照旧
// 读到上一轮的 DevToolsActivePort。
func waitProcExit(p ChromeProc, timeout time.Duration) bool {
	if p == nil {
		return true
	}
	if timeout <= 0 {
		timeout = stopForceWait
	}
	select {
	case <-p.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

// procExited 进程是否已经退出（不阻塞）。
func procExited(p ChromeProc) bool {
	if p == nil {
		return true
	}
	select {
	case <-p.Done():
		return true
	default:
		return false
	}
}

// killPID 按 PID 结束进程（对账关闭孤儿进程用）。
//
// 只杀「确认是我们自己拉起的」进程：PID 复用在长时间运行的机器上真实存在，
// 杀错进程是这一层最严重的后果，所以调用方必须先按命令行特征判定过。
func killPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("browser: PID 非法: %d", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
