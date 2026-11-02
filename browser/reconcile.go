package browser

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"
)

// ReconcileResult 一次对账的结果摘要（启动日志与排障接口都用它）。
type ReconcileResult struct {
	Scanned  int `json:"scanned"`  // 注册表条目数
	Alive    int `json:"alive"`    // 进程仍在，原样保留
	Dead     int `json:"dead"`     // 进程已不在，条目已删除
	Relinked int `json:"relinked"` // 重新读到 DevToolsActivePort 并更新端口
	Orphans  int `json:"orphans"`  // 孤儿进程（进程在、注册表没有）
	Closed   int `json:"closed"`   // 其中被关闭的孤儿（orphan_policy=close）
	Reset    int `json:"reset"`    // 库里 running 但本机没进程，已复位 idle
	// SnapshotOK 进程快照是否可用；false 时孤儿一档直接跳过（不能把「没查」当「没有」）。
	SnapshotOK bool `json:"snapshot_ok"`
}

// reconcile 启动器重启后的残留进程对账（M3 完整版：三档全处理）。
//
// 时序要求：必须在 New() 内、对外提供服务之前完成——此时还没有任何 launch 请求，
// 天然排除「对账关掉正在启动的进程」这个竞态。
//
// 三档：
//  1. 注册表有 + 进程没了 → 删条目；库里若还写着 running → 复位 idle（Reset）；
//  2. 注册表有 + 进程在 → 重挂：重新读 DevToolsActivePort 校正端口（Relinked）；
//  3. 进程在 + 注册表没有 → 孤儿：按 orphan_policy 关闭或仅提示（Orphans/Closed）。
func (m *Manager) reconcile() ReconcileResult {
	res := ReconcileResult{SnapshotOK: true}
	if m.registry == nil {
		return res
	}
	keep := map[string]bool{}
	tenants := map[string]bool{}
	for _, e := range m.registry.Snapshot() {
		res.Scanned++
		if e.TenantID != "" {
			tenants[e.TenantID] = true
		}
		if e.PID <= 0 || !procAlive(e.PID) {
			// 进程没了：条目已经失真，留着会让「本机运行时」永远显示一个不存在的窗口。
			m.registry.Remove(e.EnvID)
			res.Dead++
			// 进程已不在 ⇒ 它留下的端口文件与单实例锁都是垃圾；不清掉的话，
			// 下次启动会读到旧端口（M3 人工验收的重启 bug）。
			m.clearProfileResidue(m.layout.ProfileDir(e.EnvID), "对账（进程已退出）")
			m.log.Warn("对账：注册表中的环境进程已退出，条目已移除", "env_id", e.EnvID, "pid", e.PID)
			continue
		}
		keep[e.EnvID] = true
		res.Alive++
		// 重挂：崩溃重启后端口文件里的端口可能与登记值不同（Chrome 重开就是新端口）。
		if port, _, err := readDevToolsActivePort(m.layout.DevToolsActivePort(e.EnvID)); err == nil && port > 0 && port != e.CDPPort {
			m.log.Info("对账：CDP 端口已重挂", "env_id", e.EnvID, "old", e.CDPPort, "new", port)
			e.CDPPort = port
			m.registry.Upsert(e)
			res.Relinked++
		}
	}

	// 孤儿：进程还在，但注册表里没有 —— 上次启动器崩溃留下的。
	// 它占着 profile 锁，会让该环境下次启动直接失败（且错误信息是「profile 被占用」
	// 这种看不懂的提示），所以生产策略是关掉。
	procs, perr := procSnapshot()
	if perr != nil {
		res.SnapshotOK = false
		m.reconcileOK = false
		m.log.Warn("对账：进程快照不可用，跳过孤儿处理（reconcile 降级）", "err", perr.Error())
	} else {
		for _, p := range procs {
			envID := envIDFromCmdline(p.Cmdline, m.layout.ProfilesDir)
			if envID == "" || keep[envID] {
				continue
			}
			res.Orphans++
			if strings.EqualFold(m.orphanPolicy, "keep") {
				m.log.Warn("对账：发现孤儿进程（按策略保留）", "env_id", envID, "pid", p.PID)
				continue
			}
			if kerr := killPID(p.PID); kerr != nil {
				m.log.Error("对账：孤儿进程关闭失败", "env_id", envID, "pid", p.PID, "err", kerr.Error())
				continue
			}
			res.Closed++
			m.log.Warn("对账：已关闭孤儿进程", "env_id", envID, "pid", p.PID)
		}
	}

	// 库里 running 但本机没进程：不复位的话前端永远显示「运行中」，
	// 而窗口早没了——运营只能去改数据库。
	if m.envs != nil && len(tenants) > 0 {
		ids := make([]string, 0, len(tenants))
		for t := range tenants {
			ids = append(ids, t)
		}
		sort.Strings(ids)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n, err := m.envs.ResetStaleRunning(ctx, ids, keep)
		if err != nil {
			m.log.Error("对账：残留 running 复位失败", "err", err.Error())
			m.reconcileOK = false
		}
		res.Reset = n
	}

	if res.Dead+res.Relinked > 0 {
		if err := m.registry.Flush(); err != nil {
			m.log.Error("对账后注册表落盘失败", "err", err.Error())
			m.reconcileOK = false
		}
	}
	m.log.Info("启动器对账完成", "scanned", res.Scanned, "alive", res.Alive, "dead", res.Dead,
		"relinked", res.Relinked, "orphans", res.Orphans, "closed", res.Closed, "reset", res.Reset)
	return res
}

// Reconcile 手动触发一次对账（排障用；正常流程只在启动时跑一次）。
func (m *Manager) Reconcile() ReconcileResult {
	if m.secondInstance {
		return ReconcileResult{}
	}
	return m.reconcile()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
