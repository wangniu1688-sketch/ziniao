package browser

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func noLock() *bool { b := false; return &b }

func TestNewLayoutCreatesDirs(t *testing.T) {
	base := t.TempDir()
	l, err := NewLayout(filepath.Join(base, "data"), "")
	if err != nil {
		t.Fatalf("布局初始化失败: %v", err)
	}
	if !filepath.IsAbs(l.DataDir) {
		t.Fatalf("数据目录必须转绝对路径: %s", l.DataDir)
	}
	for _, d := range []string{l.ProfilesDir, l.ExtensionsDir, l.SidepanelDir, l.RuntimeDir, l.LogsDir, l.CADir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("子目录未创建 %s: %v", d, err)
		}
	}
	if got := l.ProfileDir("01H"); got != filepath.Join(l.ProfilesDir, "01H") {
		t.Fatalf("profile 目录不符: %s", got)
	}
	// 空 envID 会打算把 profile 落在 profiles 根目录，必须拒绝。
	if _, err := l.EnsureProfileDir(""); err == nil {
		t.Fatal("空环境 ID 应被拒绝")
	}
}

// TestPrecheckBlocks 启动前置检查：浏览器缺失 / CA 未就绪都必须**明确**阻断，
// 而不是返回一个笼统的「启动失败」让人反复重试。
func TestPrecheckBlocks(t *testing.T) {
	m, err := New(Options{
		DataDir:        t.TempDir(),
		ChromePath:     filepath.Join(t.TempDir(), "no-such-chrome"),
		SingleInstance: noLock(),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	if m.Browser().Installed {
		t.Fatal("不存在的浏览器路径不应判为已安装")
	}
	if m.Capabilities()["launch"] {
		t.Fatal("浏览器缺失时 launch 能力必须为 false")
	}
	if _, err := m.Precheck("env-1"); err != ErrBrowserMissing {
		t.Fatalf("前置检查应返回 ErrBrowserMissing，实际 %v", err)
	}
}

// TestCapabilitiesCAWired CA 就绪与否必须直接决定 launch 能力：
// 本地代理是 MITM 架构，CA 不在信任库里就是「起来了但每个站点都报证书错误」。
func TestCapabilitiesCAWired(t *testing.T) {
	m, err := New(Options{DataDir: t.TempDir(), SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	cap1 := m.Capabilities()
	if cap1["local_proxy"] != true {
		t.Fatal("本地代理能力应恒为 true")
	}
	if cap1["launch"] == (m.Browser().Installed && m.CAReady()) {
		// 一致即通过（表达式恒等，写成断言是为了让失败信息指向具体字段）
		_ = cap1
	} else {
		t.Fatalf("launch 能力应与「浏览器已装 && CA 就绪」一致: browser=%v ca=%v caps=%v",
			m.Browser().Installed, m.CAReady(), cap1)
	}
}

// TestWithEnvLockSerializes 同一环境串行、不同环境并发：
// 前者是「同一环境不能被启动两次」的保证，后者是「第 20 个环境不该等第 1 个环境的
// CDP 超时」的保证（-race 下同时验证无数据竞争）。
func TestWithEnvLockSerializes(t *testing.T) {
	m, err := New(Options{DataDir: t.TempDir(), SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()

	var cur int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.WithEnvLock("env-1", func() error {
				if atomic.LoadInt32(&cur) != 0 {
					t.Error("同一环境的临界区被并发进入")
				}
				atomic.AddInt32(&cur, 1)
				atomic.AddInt32(&cur, -1)
				return nil
			})
		}()
	}
	// 不同环境各起各的（只验证不死锁、无竞争）
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.WithEnvLock("env-"+string(rune('a'+i%20)), func() error { return nil })
		}(i)
	}
	wg.Wait()
}

func TestRuntimeRegisterAndUnregister(t *testing.T) {
	m, err := New(Options{DataDir: t.TempDir(), SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	if len(m.Runtime()) != 0 {
		t.Fatalf("新实例运行时应为空: %v", m.Runtime())
	}
	if err := m.Register(entry("env-9", 4242)); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if e, ok := m.RuntimeOf("env-9"); !ok || e.PID != 4242 {
		t.Fatalf("登记后读回不符: %+v", e)
	}
	// 落盘生效：重载注册表应看到同一条（这是崩溃后能恢复的唯一依据）
	reloaded, err := LoadRegistry(m.Layout().RegistryPath())
	if err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	if reloaded.Len() != 1 {
		t.Fatalf("重载后条目数 = %d，期望 1", reloaded.Len())
	}
	if err := m.Unregister("env-9"); err != nil {
		t.Fatalf("注销失败: %v", err)
	}
	if len(m.Runtime()) != 0 {
		t.Fatalf("注销后应为空: %v", m.Runtime())
	}
	if err := m.Register(RuntimeEntry{}); err == nil {
		t.Fatal("空环境 ID 应被拒绝登记")
	}
}
