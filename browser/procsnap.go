package browser

import (
	"strconv"
	"strings"
)

// ProcInfo 进程快照里的一条（PID + 命令行）。
//
// 只存这两项：对账判定孤儿只关心「它是不是用我们的 profile 目录启动的」，
// 多读一个字段就多一份要解释的隐私（本机进程列表里可能有别的业务）。
type ProcInfo struct {
	PID     int
	Cmdline string
}

// procSnapshotFn 进程快照动作（可替换：单测注入固定列表，不真扫本机）。
type procSnapshotFn func() ([]ProcInfo, error)

var procSnapshot procSnapshotFn = snapshotProcs

// envIDFromCmdline 从命令行里解析出环境 ID。
//
// 判据是 --user-data-dir=<profiles 目录>/<env-id>：这是我们唯一能确认
// 「这个 Chrome 是启动器拉起来的」的证据。只看进程名（chrome.exe）会误伤
// 运营自己开的浏览器——杀掉别人的窗口是这一层最严重的后果。
func envIDFromCmdline(cmdline, profilesDir string) string {
	if cmdline == "" || profilesDir == "" {
		return ""
	}
	const flag = "--user-data-dir="
	i := strings.Index(cmdline, flag)
	if i < 0 {
		return ""
	}
	rest := cmdline[i+len(flag):]
	// 命令行里该参数通常以空格结束；也可能后面紧跟其它参数（罕见）。
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.Trim(rest, `"`)
	prefix := strings.TrimRight(profilesDir, "\\/") + string(separator())
	if !strings.HasPrefix(rest, prefix) {
		return ""
	}
	id := strings.Trim(rest[len(prefix):], "\\/ \"")
	if id == "" || strings.ContainsAny(id, `/\`) {
		return ""
	}
	return id
}

// parseProcLines 解析 "pid\tcmdline" 行。
func parseProcLines(out string) []ProcInfo {
	var list []ProcInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 {
			continue
		}
		list = append(list, ProcInfo{PID: pid, Cmdline: strings.TrimSpace(parts[1])})
	}
	return list
}
