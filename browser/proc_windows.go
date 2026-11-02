//go:build windows

package browser

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procAlive 判断进程是否还活着（Windows：tasklist 按 PID 查询）。
//
// 只在启动期/对账期调用（非热路径），所以用命令行工具换实现简单性：
// Go 标准库在 Windows 上没有「只查询进程是否存在」的零依赖入口，
// os.FindProcess 的语义在不同版本间又不一致，赌它不如问系统一次。
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	s := string(out)
	// tasklist 查不到匹配进程时会打 INFO: 提示（退出码随版本为 0/1，不可依赖）。
	if strings.Contains(s, "INFO:") {
		return false
	}
	if err != nil {
		// 查询本身失败（命令缺失、被策略拦截）时按「活着」保守处理：
		// 把正常运行的实例误判成陈旧锁并接管，后果远大于「多提示一次已在运行」。
		return true
	}
	return strings.Contains(s, strconv.Itoa(pid))
}

// setProcAttr Windows 下不让浏览器继承控制台：启动器从终端拉起时，
// Chrome 的调试输出会污染启动器控制台（里面可能有 CDP 路径等我们不希望外流的信息）。
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = 0x08000000 // CREATE_NO_WINDOW
}
