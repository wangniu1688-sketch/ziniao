//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// procAlive 判断进程是否还活着（Unix：signal 0 探测）。
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// 信号 0 不发送任何信号，只做存在性与权限检查。
	return syscall.Kill(pid, 0) == nil //nolint:errcheck // EPERM（属于别的用户的进程）也算活着
}

// setProcAttr 让浏览器进程脱离启动器的进程组：启动器被强杀时不至于把 Chrome
// 一起带走（运营的窗口不该因为后台重启而消失）。
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
