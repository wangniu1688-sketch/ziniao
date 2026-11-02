//go:build windows

package browser

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// separator 路径分隔符（跨平台解析命令行用）。
func separator() byte { return filepath.Separator }

// snapshotProcs 列出本机 chrome.exe / msedge.exe 的 PID 与命令行。
//
// 只在启动期对账调用一次，所以用 PowerShell 的 CIM 查询换实现简单性：
// Go 标准库没有跨平台读他人命令行的入口，而「对账要能发现孤儿进程」这件事
// 必须做——启动器崩溃留下的 Chrome 会一直占着 profile 锁，下次启动该环境直接失败。
//
// 拿不到快照（PowerShell 被策略禁用、超时）时**必须返回错误**让调用方降级，
// 绝不能返回空列表——空列表会被解释成「没有孤儿」，那是把「没查」当成「没有」。
func snapshotProcs() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := `Get-CimInstance Win32_Process -Filter "Name='chrome.exe' OR Name='msedge.exe' OR Name='edge.exe'"` +
		` | ForEach-Object { "$($_.ProcessId)` + "\t" + `$($_.CommandLine)" }`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("browser: 进程快照不可用: %w", err)
	}
	return parseProcLines(string(out)), nil
}
