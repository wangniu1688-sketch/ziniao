package browser

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// profileResidueFiles Chrome 写在 profile 根目录下、**只属于某一次进程实例**的文件。
//
//   - DevToolsActivePort：`--remote-debugging-port=0` 时 Chrome 回写的实际端口；
//   - SingletonLock / SingletonSocket / SingletonCookie：Chrome 的单实例三件套。
//
// 它们不属于 profile 本身：进程正常退出时 Chrome 会自己删掉，但被强杀、崩溃、
// 或运营在任务管理器里结束进程时，一定留在盘上。
//
// 留着的后果（M3 人工验收实测）：下一轮启动先读到上一轮的旧端口，而新 Chrome
// 又可能因为看到残留的单实例锁，把启动请求转发给一个已经不存在的旧实例后自己退出
// ——表现为「环境启动失败（阶段 6）：CDP 连接失败: dial tcp 127.0.0.1:<旧端口>」，
// 并且手动杀掉全部 chrome.exe 也修不好（因为要读的文件还在盘上）。
var profileResidueFiles = []string{
	"DevToolsActivePort",
	"SingletonLock",
	"SingletonSocket",
	"SingletonCookie",
}

// ProfileResidueFiles 残留文件清单（排障与测试共用，返回副本避免外部改写）。
func ProfileResidueFiles() []string {
	out := make([]string, len(profileResidueFiles))
	copy(out, profileResidueFiles)
	return out
}

// purgeProfileResidue 删除 profile 根目录下的启动残留。
//
// 返回「已删掉的」与「删完仍在的」。删不掉通常意味着有个活着的 Chrome 正独占持有
// （Windows 上就是这个表现）——那本身就是一条信息，调用方据此决定是连它的端口
// 还是提示运营，而不是假装已经清干净了。
//
// 「文件已经不在」不算失败：停止流程与崩溃监视协程都会看到进程退出并各自清一次，
// 后到的那次必然撞上已被删掉的文件，把它报成错误只会污染日志。
func purgeProfileResidue(profileDir string) (removed []string, remaining []string) {
	if profileDir == "" {
		return nil, nil
	}
	for _, name := range profileResidueFiles {
		p := filepath.Join(profileDir, name)
		if _, err := os.Lstat(p); err != nil {
			continue // 本来就没有，不用管
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			remaining = append(remaining, name)
			continue
		}
		removed = append(removed, name)
	}
	return removed, remaining
}

// residueFilesPresent 列出当前仍然存在的残留文件（清理后复核）。
func residueFilesPresent(profileDir string) []string {
	if profileDir == "" {
		return nil
	}
	var out []string
	for _, name := range profileResidueFiles {
		if _, err := os.Lstat(filepath.Join(profileDir, name)); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// clearProfileResidue 清残留 + 复核，返回**仍然残留**的文件名（空 = 已清干净）。
//
// why 只是日志标签（"启动前"/"停止后"/"启动失败回滚"），用于事后从日志里看出
// 残留是被哪一步留下的——这类缺陷最耗时的部分永远是「到底谁没删」。
func (m *Manager) clearProfileResidue(profileDir, why string) []string {
	if profileDir == "" {
		return nil
	}
	log := m.log
	if log == nil {
		log = slog.Default()
	}
	removed, remaining := purgeProfileResidue(profileDir)
	if len(removed) > 0 {
		log.Info("已清理浏览器启动残留", "why", why, "dir", profileDir, "files", strings.Join(removed, ","))
	}
	if len(remaining) > 0 {
		log.Warn("启动残留未能清除（多半是浏览器进程仍在运行）",
			"why", why, "dir", profileDir, "files", strings.Join(remaining, ","))
	}
	return residueFilesPresent(profileDir)
}
