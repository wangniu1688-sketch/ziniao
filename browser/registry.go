package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ProcState 本机进程态（易失、可重建，绝不写回数据库）。
const (
	ProcStarting = "starting"
	ProcRunning  = "running"
	ProcStopping = "stopping"
	ProcUnknown  = "unknown"
)

// ExitMode 出口来源：manual（手工代理）/ pool（代理商池）/ direct（直连，仍走本地代理）。
const (
	ExitManual = "manual"
	ExitPool   = "pool"
	ExitDirect = "direct"
)

// RuntimeEntry 一个环境的本机运行时。
//
// 它是**缓存**而不是权威：业务状态永远以 PostgreSQL environments.status 为准。
// 进程崩溃、换机器、启动器重启都可能让这里与库不一致——所以任何「注册表里没有」
// 都只能解释为「本机看不见」，不能解释为「环境没在跑」。
type RuntimeEntry struct {
	EnvID          string    `json:"env_id"`
	ShopID         string    `json:"shop_id"`
	TenantID       string    `json:"tenant_id"`
	PID            int       `json:"pid"`
	CDPPort        int       `json:"cdp_port"`
	LocalProxyPort int       `json:"local_proxy_port"`
	ProfileDir     string    `json:"profile_dir"`
	ChromePath     string    `json:"chrome_path"`
	ExitMode       string    `json:"exit_mode"`
	StartedAt      time.Time `json:"started_at"`
	ProcState      string    `json:"proc_state"`
}

// RegistrySchema 注册表文件版本号：字段增删时递增，旧文件按不兼容处理。
const RegistrySchema = 1

type registryFile struct {
	Schema    int            `json:"schema"`
	UpdatedAt time.Time      `json:"updated_at"`
	Entries   []RuntimeEntry `json:"entries"`
}

// Registry 本机运行时注册表：内存态 + data/runtime/registry.json 落盘。
//
// 并发模型（与 Manager 的锁边界一致）：
//   - entries 由 mu 保护（读多写少，读走 RLock）；
//   - 落盘由 diskMu 串行 + 临时文件 rename，避免出现半截 JSON；
//   - 单环境串行化不在这里做（那是 Manager.perEnv 的职责）：注册表只保证「自己的
//     结构不会坏」，不保证业务时序。
type Registry struct {
	mu     sync.RWMutex
	diskMu sync.Mutex
	path   string
	// schema 为 0 表示「磁盘上没有文件」；读到的文件版本不匹配时按空处理
	// （宁可丢缓存重建，也不要拿旧结构去解释新字段）。
	entries map[string]*RuntimeEntry
}

// LoadRegistry 载入注册表；文件缺失 / 损坏 / 版本不符都按空注册表处理。
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, entries: map[string]*RuntimeEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("browser: 注册表读取失败: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil || f.Schema != RegistrySchema {
		// 损坏文件不删除：留给人工取证（谁在什么时候写了半截文件）。
		return r, fmt.Errorf("browser: 注册表内容不可解析或版本不符（%s），按空注册表启动", path)
	}
	for i := range f.Entries {
		e := f.Entries[i]
		r.entries[e.EnvID] = &e
	}
	return r, nil
}

// Upsert 登记 / 更新一个环境的运行时。
func (r *Registry) Upsert(e RuntimeEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := e
	r.entries[e.EnvID] = &cp
}

// Remove 移除一个环境的运行时，返回是否确有此条。
func (r *Registry) Remove(envID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[envID]
	delete(r.entries, envID)
	return ok
}

// Get 取单个环境的运行时（返回副本，调用方改不到内部状态）。
func (r *Registry) Get(envID string) (RuntimeEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[envID]
	if !ok {
		return RuntimeEntry{}, false
	}
	return *e, true
}

// Snapshot 全部运行时快照（按 started_at 升序，保证输出稳定可断言）。
func (r *Registry) Snapshot() []RuntimeEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RuntimeEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].EnvID < out[j].EnvID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Len 当前登记条数。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Flush 原子落盘：写 .tmp 后 rename。
//
// 为什么必须原子：注册表被进程崩溃打断时如果留下半截 JSON，下次启动连「哪些环境
// 在跑」都读不出来——而它是 reconcile 唯一的输入。rename 在同一分区上是原子的，
// 要么旧文件完整、要么新文件完整。
func (r *Registry) Flush() error {
	r.mu.RLock()
	entries := make([]RuntimeEntry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, *e)
	}
	r.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].EnvID < entries[j].EnvID })
	payload, err := json.MarshalIndent(registryFile{
		Schema: RegistrySchema, UpdatedAt: time.Now().UTC(), Entries: entries,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("browser: 注册表序列化失败: %w", err)
	}

	r.diskMu.Lock()
	defer r.diskMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("browser: 注册表目录创建失败: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("browser: 注册表写入失败: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("browser: 注册表替换失败: %w", err)
	}
	return nil
}

// ---------- 单实例锁 ----------

// ErrAlreadyRunning 已有启动器实例在运行（本期不支持多开）。
var ErrAlreadyRunning = errors.New("browser: 启动器已在运行")

// InstanceLock 启动器单实例锁。
//
// 本期按「不支持多开」处理：两个实例会互相把对方的 Chrome 当成孤儿进程对账掉，
// 也会各自持一份注册表副本互相覆盖。所以第二个实例只能以只读方式提示，
// 不允许重复拉起环境。
type InstanceLock struct {
	path string
	pid  int
}

type lockFile struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// AcquireInstanceLock 获取实例锁。锁存在但持有进程已死（上次崩溃）时接管。
func AcquireInstanceLock(path string) (*InstanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("browser: 锁目录创建失败: %w", err)
	}
	// O_EXCL：创建成功即持锁；失败再判断是不是陈旧锁，绝不「先删再建」
	// （那会连活着的实例一起顶掉）。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		payload, _ := json.Marshal(lockFile{PID: os.Getpid(), StartedAt: time.Now().UTC()})
		_, werr := f.Write(payload)
		_ = f.Close()
		if werr != nil {
			return nil, fmt.Errorf("browser: 锁文件写入失败: %w", werr)
		}
		return &InstanceLock{path: path, pid: os.Getpid()}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("browser: 锁文件创建失败: %w", err)
	}
	// 锁存在：只有「持有进程已经不在了」才允许接管。
	b, rerr := os.ReadFile(path)
	if rerr == nil {
		var lf lockFile
		if jerr := json.Unmarshal(b, &lf); jerr == nil && lf.PID > 0 && procAlive(lf.PID) {
			return nil, ErrAlreadyRunning
		}
	}
	_ = os.Remove(path)
	return AcquireInstanceLock(path)
}

// Release 释放实例锁（只删自己写的那把）。
func (l *InstanceLock) Release() error {
	if l == nil {
		return nil
	}
	_ = l.pid
	return os.Remove(l.path)
}
