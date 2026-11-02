// Package browser 启动器内核：本机浏览器进程的编排层（M1 骨架 / M3 闭环）。
//
// 定位：**只管本机进程与端口**，业务状态（created/running/idle/stopped）的权威
// 永远在 PostgreSQL environments.status。这里维护的注册表只是「本机这一刻」的
// 缓存：进程崩溃、换机器、启动器重启都可能让它与库不一致，靠 reconcile 纠正。
//
// 明确不做：不 fork Chromium、不引入 Electron/Tauri、不做标签栏/地址栏/书签栏
// （用 Chrome 原生 UI）、不把上游代理凭据写进 argv / 日志 / 响应。
//
// M1 交付：目录布局、浏览器探测、CA 就绪判定、运行时注册表（含单实例锁）、
// argv 构造。Launch / Stop / Reconcile 的时序见 docs/design-m1-browser-manager.md，
// 属于 M3，不在本期实现。
package browser

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout 数据目录布局（ZINIAO_DATA_DIR，默认 ./data）。
//
// 全部字段在 NewLayout 里转绝对路径：进程的工作目录会变（服务注册后、计划任务
// 拉起后），相对路径意味着「换个目录启动就换了一套 profile」——那是最难排查的
// 一类缺陷，因为症状是「浏览器起来了但指纹全变了」。
type Layout struct {
	DataDir       string // 根
	ProfilesDir   string // profiles/<env-id>/：Chrome --user-data-dir
	ExtensionsDir string
	SidepanelDir  string // 侧边栏扩展源码（本期默认不加载）
	RuntimeDir    string // runtime/registry.json
	ThumbnailsDir string // thumbnails/<env-id>.png：停止前留的窗口缩略图（M3/T10）
	LogsDir       string
	CADir         string // MITM 根证书与私钥
}

// NewLayout 解析数据目录并确保子目录存在。
func NewLayout(dataDir string, caDir string) (Layout, error) {
	if dataDir == "" {
		dataDir = "./data"
	}
	abs, err := filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return Layout{}, fmt.Errorf("browser: 数据目录解析失败: %w", err)
	}
	l := Layout{
		DataDir:       abs,
		ProfilesDir:   filepath.Join(abs, "profiles"),
		ExtensionsDir: filepath.Join(abs, "extensions"),
		SidepanelDir:  filepath.Join(abs, "extensions", "sidepanel"),
		RuntimeDir:    filepath.Join(abs, "runtime"),
		ThumbnailsDir: filepath.Join(abs, "thumbnails"),
		LogsDir:       filepath.Join(abs, "logs"),
		CADir:         filepath.Join(abs, "ca"),
	}
	if caDir != "" {
		caAbs, err := filepath.Abs(filepath.Clean(caDir))
		if err != nil {
			return Layout{}, fmt.Errorf("browser: CA 目录解析失败: %w", err)
		}
		l.CADir = caAbs
	}
	for _, d := range []string{l.ProfilesDir, l.ExtensionsDir, l.SidepanelDir, l.RuntimeDir, l.ThumbnailsDir, l.LogsDir, l.CADir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Layout{}, fmt.Errorf("browser: 数据目录创建失败 %s: %w", d, err)
		}
	}
	return l, nil
}

// ProfileDir 某环境的 profile 目录（data/profiles/<env-id>）。
func (l Layout) ProfileDir(envID string) string { return filepath.Join(l.ProfilesDir, envID) }

// EnsureProfileDir 创建并校验环境 profile 目录。
//
// envID 必须非空：空 ID 会把 profile 直接落在 profiles/ 根目录，届时一次删除
// 就是删掉全部环境——目录名就是隔离边界。
func (l Layout) EnsureProfileDir(envID string) (string, error) {
	if envID == "" {
		return "", fmt.Errorf("browser: 环境 ID 为空，拒绝创建 profile 目录")
	}
	dir := l.ProfileDir(envID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("browser: profile 目录创建失败: %w", err)
	}
	return dir, nil
}

// RegistryPath 本机运行时注册表路径。
func (l Layout) RegistryPath() string { return filepath.Join(l.RuntimeDir, "registry.json") }

// LockPath 单实例锁文件路径。
func (l Layout) LockPath() string { return filepath.Join(l.RuntimeDir, "launcher.lock") }

// DevToolsActivePort Chrome 写入的调试端口文件路径。
func (l Layout) DevToolsActivePort(envID string) string {
	return filepath.Join(l.ProfileDir(envID), "DevToolsActivePort")
}

// ThumbnailPath 某环境的窗口缩略图（停止前截图，M3/T10）。
func (l Layout) ThumbnailPath(envID string) string {
	return filepath.Join(l.ThumbnailsDir, envID+".png")
}
