package browser

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func entry(envID string, pid int) RuntimeEntry {
	return RuntimeEntry{
		EnvID: envID, ShopID: "shop-" + envID, TenantID: "t1", PID: pid,
		CDPPort: 53000, LocalProxyPort: 53001, ProfileDir: "/data/profiles/" + envID,
		ChromePath: "C:/chrome.exe", ExitMode: ExitPool,
		StartedAt: time.Now().UTC().Truncate(time.Second), ProcState: ProcRunning,
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime", "registry.json")
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("空注册表载入失败: %v", err)
	}
	reg.Upsert(entry("env-1", 1111))
	reg.Upsert(entry("env-2", 2222))
	if err := reg.Flush(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	again, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	if again.Len() != 2 {
		t.Fatalf("重载后条目数 = %d，期望 2", again.Len())
	}
	e, ok := again.Get("env-1")
	if !ok || e.PID != 1111 || e.CDPPort != 53000 {
		t.Fatalf("重载内容不符: %+v", e)
	}
	// 读出来的是副本：改返回值不得污染注册表（mem 驱动踩过的同一类坑）。
	e.PID = 9999
	if cur, _ := again.Get("env-1"); cur.PID != 1111 {
		t.Fatalf("返回值应为副本，内部被改成了 %d", cur.PID)
	}
	if !again.Remove("env-2") {
		t.Fatal("移除 env-2 应返回 true")
	}
	if again.Len() != 1 {
		t.Fatalf("移除后条目数 = %d", again.Len())
	}
}

// TestRegistrySnapshotStable 快照顺序必须稳定：它直接进 API 响应与排障截图，
// 顺序随 map 迭代抖动会让「前后两次刷新结果不一样」被当成 bug 报上来。
func TestRegistrySnapshotStable(t *testing.T) {
	reg := &Registry{path: filepath.Join(t.TempDir(), "registry.json"), entries: map[string]*RuntimeEntry{}}
	for _, id := range []string{"c", "a", "b"} {
		reg.Upsert(entry(id, 1))
	}
	first := reg.Snapshot()
	for i := 0; i < 20; i++ {
		got := reg.Snapshot()
		for j := range got {
			if got[j].EnvID != first[j].EnvID {
				t.Fatalf("快照顺序不稳定: %v vs %v", got, first)
			}
		}
	}
}

// TestRegistryConcurrent 并发登记：go test -race 下必须无数据竞争。
// 多环境并发启动是日常操作，注册表是它们唯一的共享写入点。
func TestRegistryConcurrent(t *testing.T) {
	reg := &Registry{path: filepath.Join(t.TempDir(), "registry.json"), entries: map[string]*RuntimeEntry{}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "env-" + string(rune('A'+i%26))
			reg.Upsert(entry(id, i))
			_ = reg.Snapshot()
			_ = reg.Len()
			_, _ = reg.Get(id)
		}(i)
	}
	wg.Wait()
	if reg.Len() == 0 {
		t.Fatal("并发登记后不应为空")
	}
}

// TestInstanceLock 单实例锁：活着 → 抢占失败；已死 → 允许接管（上次崩溃的残留锁
// 不能把启动器永久锁死在门外）。
func TestInstanceLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launcher.lock")

	lk, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("首次抢占失败: %v", err)
	}
	if _, err := AcquireInstanceLock(path); err != ErrAlreadyRunning {
		t.Fatalf("第二实例应返回 ErrAlreadyRunning，实际 %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("释放后锁文件应不存在")
	}

	// 活着的持有者：锁文件里写当前进程 PID，必须判为「已在运行」。
	// 用真实存活 PID 而不是编造的 PID——后者在不同平台上的判定结果不可移植。
	if err := os.WriteFile(path, []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"started_at":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireInstanceLock(path); err != ErrAlreadyRunning {
		t.Fatalf("持有者存活时应返回 ErrAlreadyRunning，实际 %v", err)
	}
	// 陈旧锁：内容不可解析（上次崩溃写了一半）应被接管，
	// 否则一次崩溃就把启动器永久锁在门外。
	if err := os.WriteFile(path, []byte(`{"pid":12`), 0o600); err != nil {
		t.Fatal(err)
	}
	lk2, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("陈旧锁应被接管，实际: %v", err)
	}
	_ = lk2.Release()
}
