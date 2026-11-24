//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"strings"
	"strconv"
	"syscall"
)

// procAlive 判断进程是否还活着（Unix：signal 0 探测）。
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Linux 的 kill(pid, 0) 对僵尸进程仍返回成功。对账若把 Z 状态
	// 当成活进程，就会认为孤儿未关闭，反复保留失效 PID。
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if end := strings.LastIndexByte(string(stat), ')'); end >= 0 {
			fields := strings.Fields(string(stat[end+1:]))
			if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
				return false
			}
		}
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
