//go:build !windows

package browser

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// separator 路径分隔符。
func separator() byte { return filepath.Separator }

// snapshotProcs 列出本机 chrome / chromium / msedge 的 PID 与命令行。
//
// 只在启动期对账调用一次。拿不到快照时返回错误让调用方降级为「不做孤儿处理」，
// 绝不返回空列表——空列表会被误读成「没有孤儿」。
func snapshotProcs() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=", "args=").Output()
	if err != nil {
		return nil, fmt.Errorf("browser: 进程快照不可用: %w", err)
	}
	var list []ProcInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		args := fields[1]
		base := filepath.Base(strings.Fields(args)[0])
		if base != "chrome" && base != "chromium" && base != "msedge" && base != "google-chrome" {
			continue
		}
		pid := 0
		if _, serr := fmt.Sscanf(strings.TrimSpace(fields[0]), "%d", &pid); serr != nil || pid <= 0 {
			continue
		}
		list = append(list, ProcInfo{PID: pid, Cmdline: args})
	}
	return list, nil
}
