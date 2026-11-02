package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"ziniao/network"
)

// ---------- M3 重启闭环：启动 → 停止 → 再启动 ----------
//
// 这一组用例对应人工验收发现的缺陷：停止后 profile 里的 DevToolsActivePort 与
// 单实例三件套没被清掉，第二次启动读到旧端口（或新进程被残留锁顶掉），
// 表现为「CDP 连接失败: dial tcp 127.0.0.1:<旧端口>」，且手动杀光 chrome.exe 也没用。

// restartFixture 重启闭环测试台。
//
// 每次「拉起 Chrome」都换一个新端口（真 Chrome 每次也是新端口），并把拉起那一刻
// 端口文件是否已经存在记下来——存在就意味着这一轮有机会读到旧值。
type restartFixture struct {
	m        *Manager
	portFile string
	mu       sync.Mutex
	nextPort int
	// writePort 为 false 时模拟「新 Chrome 没写出端口文件」：被残留单实例锁顶掉、
	// 或把启动请求转发给不存在的旧实例后自己退出，就是这个表现。
	writePort bool
	// staleAtLaunch 每次拉起时端口文件是否已存在（true = 会读到上一轮的值）。
	staleAtLaunch []bool
	// dialed 实际拨出去的 CDP 次数（>0 才可能连到旧端口上）。
	dialed int
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		DataDir: t.TempDir(), ChromePath: exe, SingleInstance: noLock(),
		CDPTimeout: 2 * time.Second, StopGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	var installed bool
	fakeCAOps(t, &installed)
	if _, err := m.InstallCA(); err != nil {
		t.Fatalf("CA 就绪失败: %v", err)
	}
	f := &restartFixture{
		m: m, portFile: m.layout.DevToolsActivePort("env-1"),
		nextPort: 50300, writePort: true,
	}
	m.envs = &fakeEnvControl{}
	m.startProxy = func(network.TLSProxyConfig) (ProxyHandle, error) { return &fakeProxy{port: 45678}, nil }
	wsURL := newFakeCDP(t, nil)
	m.cdpDial = func(ctx context.Context, _ string) (*CDPConn, error) {
		f.mu.Lock()
		f.dialed++
		f.mu.Unlock()
		return DialCDP(ctx, wsURL)
	}
	m.startProc = func(_ string, _ []string) (ChromeProc, error) { return f.launch() }
	return f
}

// launch 模拟 Chrome 拉起：先记录端口文件是否在（在 = 残留），再写出本轮的新端口。
func (f *restartFixture) launch() (ChromeProc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := os.Stat(f.portFile)
	f.staleAtLaunch = append(f.staleAtLaunch, err == nil)
	if f.writePort {
		f.nextPort++
		_ = os.WriteFile(f.portFile, []byte(strconv.Itoa(f.nextPort)+"\n/devtools/browser/x\n"), 0o600)
	}
	return &fakeProc{pid: os.Getpid(), done: make(chan struct{})}, nil
}

func (f *restartFixture) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialed
}

func (f *restartFixture) staleSeen() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.staleAtLaunch))
	copy(out, f.staleAtLaunch)
	return out
}

// TestRestartGetsFreshCDPPort 启动 → 停止 → 再启动：
// 第二次必须拿到**新的** CDP 端口，且拉起前磁盘上不能还留着旧的端口文件。
func TestRestartGetsFreshCDPPort(t *testing.T) {
	f := newRestartFixture(t)
	m := f.m
	ctx := context.Background()
	start := StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}

	r1, err := m.Start(ctx, start)
	if err != nil {
		t.Fatalf("第一次启动失败: %v", err)
	}

	// 手工补上「被强杀的 Chrome 会留下」的单实例锁——停止流程必须把它们一起清掉。
	dir := m.layout.ProfileDir("env-1")
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := m.Stop(ctx, StopInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	assertNoResidue(t, dir, "停止后")

	r2, err := m.Start(ctx, start)
	if err != nil {
		t.Fatalf("第二次启动失败: %v", err)
	}
	if r2.CDPPort == r1.CDPPort {
		t.Fatalf("第二次启动不应复用第一次的 CDP 端口（两次都是 %d）", r2.CDPPort)
	}
	if seen := f.staleSeen(); len(seen) != 2 || seen[1] {
		t.Fatalf("第二次拉起前端口文件不应存在（否则会读到旧端口）: %v", seen)
	}
	if r2.Status != "running" || r2.PID == 0 {
		t.Fatalf("第二次启动结果不符: %+v", r2)
	}
}

// TestStartNeverReusesStaleDevToolsPort 上一轮遗留的端口文件必须在新 Chrome 拉起
// **之前**就被清掉：本次启动只能等本轮写出的文件，绝不能拿旧端口去连。
func TestStartNeverReusesStaleDevToolsPort(t *testing.T) {
	f := newRestartFixture(t)
	// 找一个当前一定连不上的端口当「旧端口」，再把文件时间戳拨到 1 小时前。
	if err := os.MkdirAll(f.m.layout.ProfileDir("env-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, port, err := ListenLocal()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // 立刻关掉 ⇒ 这个端口现在必然不可连
	if err := os.WriteFile(f.portFile, []byte(strconv.Itoa(port)+"\n/devtools/browser/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.portFile, old, old); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.writePort = false // 新 Chrome 没写出新端口文件（被残留锁顶掉的典型表现）
	f.mu.Unlock()

	if _, err := f.m.Start(context.Background(), StartInput{
		EnvID: "env-1", ShopID: "shop-1", Actor: actor(),
	}); err == nil {
		t.Fatal("新 Chrome 没写出端口文件时应启动失败")
	}
	if got := f.m.envs.(*fakeEnvControl).lastFailure(); got.Stage != "5" {
		t.Fatalf("应在阶段 5 失败，实际 %q", got.Stage)
	}
	if f.dialCount() != 0 {
		t.Fatal("不应拿上一轮的旧端口去连 CDP")
	}
	if seen := f.staleSeen(); len(seen) != 1 || seen[0] {
		t.Fatalf("拉起 Chrome 之前端口文件就应已被清掉: %v", seen)
	}
	// 失败回滚也要清干净，否则下次启动踩同一个坑。
	assertNoResidue(t, f.m.layout.ProfileDir("env-1"), "启动失败回滚后")
}

// TestWaitDevToolsPortAcceptsLiveStalePort 唯一允许复用旧端口文件的情形：
// 文件虽是上一轮写的，但那个端口现在确实连得上（说明服务这个 profile 的是真实例）。
func TestWaitDevToolsPortAcceptsLiveStalePort(t *testing.T) {
	dir := t.TempDir()
	ln, port, err := ListenLocal()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	p := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(p, []byte(strconv.Itoa(port)+"\n/devtools/browser/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	// 进程还活着（没退出）⇒ 走「旧文件但端口可达」分支。
	proc := &fakeProc{pid: os.Getpid(), done: make(chan struct{})}
	got, ws, err := waitDevToolsPort(context.Background(), p, 2*time.Second, proc, time.Now())
	if err != nil {
		t.Fatalf("端口确实连得上时应接受旧端口文件，实际报错: %v", err)
	}
	if got != port || ws != "/devtools/browser/x" {
		t.Fatalf("端口/路径不符: %d %q", got, ws)
	}
}

// TestFailedStartPurgesProfileResidue 阶段 6 失败回滚：进程收掉后必须清掉残留，
// 否则「再点一次启动」会读到上一次失败留下的端口文件。
func TestFailedStartPurgesProfileResidue(t *testing.T) {
	f := newRestartFixture(t)
	f.m.cdpDial = func(context.Context, string) (*CDPConn, error) {
		return nil, errors.New("连接被拒")
	}
	if _, err := f.m.Start(context.Background(), StartInput{
		EnvID: "env-1", ShopID: "shop-1", Actor: actor(),
	}); err == nil {
		t.Fatal("CDP 连不上时应启动失败")
	}
	if got := f.m.envs.(*fakeEnvControl).lastFailure(); got.Stage != "6" {
		t.Fatalf("应在阶段 6 失败，实际 %q", got.Stage)
	}
	assertNoResidue(t, f.m.layout.ProfileDir("env-1"), "启动失败回滚后")
}

// TestStopWithoutSessionKeepsLiveProfile 启动器重启后重挂的条目（进程在本机活着，
// 但本实例没有会话句柄）：停止只收回业务状态，**绝不能**删活实例的端口文件。
func TestStopWithoutSessionKeepsLiveProfile(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{DataDir: t.TempDir(), ChromePath: exe, SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	envs := &fakeEnvControl{}
	m.envs = envs
	// 重挂条目：PID 是本机当前进程，必然活着。
	_ = m.Register(RuntimeEntry{EnvID: "env-1", PID: os.Getpid(), ProcState: ProcRunning})
	portFile := m.layout.DevToolsActivePort("env-1")
	_ = os.MkdirAll(m.layout.ProfileDir("env-1"), 0o700)
	_ = os.WriteFile(portFile, []byte("50309\n/devtools/browser/x\n"), 0o600)

	if _, err := m.Stop(context.Background(), StopInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err != nil {
		t.Fatalf("无会话停止不应报错: %v", err)
	}
	if envs.stopped == 0 {
		t.Fatal("无会话时仍应收回业务状态")
	}
	if _, err := os.Stat(portFile); err != nil {
		t.Fatalf("活实例的端口文件不应被删（删了就再也连不回去）: %v", err)
	}
}

// TestStopWithoutSessionCleansDeadProfile 无会话 + 进程已死：残留必须清掉，
// 否则下次启动会读到上一次崩溃留下的端口。
func TestStopWithoutSessionCleansDeadProfile(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{DataDir: t.TempDir(), ChromePath: exe, SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	m.envs = &fakeEnvControl{}
	// 陈旧条目：PID 指向一个不可能存在的进程。
	_ = m.Register(RuntimeEntry{EnvID: "env-1", PID: 999997, ProcState: ProcRunning})
	dir := m.layout.ProfileDir("env-1")
	_ = os.MkdirAll(dir, 0o700)
	for _, name := range ProfileResidueFiles() {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}

	if _, err := m.Stop(context.Background(), StopInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err != nil {
		t.Fatalf("无会话停止不应报错: %v", err)
	}
	assertNoResidue(t, dir, "停止（本机无活进程）后")
}

// TestPurgeProfileResidue 清理函数本身：能删的删掉，删不掉的要报出来（不许假装成功）。
func TestPurgeProfileResidue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, name := range ProfileResidueFiles() {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		want = append(want, name)
	}
	// 一个不该被动的文件（profile 真内容）。
	keep := filepath.Join(dir, "Preferences")
	if err := os.WriteFile(keep, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, remaining := purgeProfileResidue(dir)
	if len(remaining) != 0 || len(removed) != len(want) {
		t.Fatalf("清理结果不符: removed=%v remaining=%v", removed, remaining)
	}
	if got := residueFilesPresent(dir); len(got) != 0 {
		t.Fatalf("复核仍报残留: %v", got)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("profile 里的其它文件不应被动: %v", err)
	}
	// 幂等：再清一次没有副作用，也不会报错。
	if r2, m2 := purgeProfileResidue(dir); len(r2) != 0 || len(m2) != 0 {
		t.Fatalf("重复清理应为空操作: %v %v", r2, m2)
	}
	if r3, m3 := purgeProfileResidue(""); len(r3) != 0 || len(m3) != 0 {
		t.Fatal("空目录应安全返回")
	}
}

// assertNoResidue 断言指定 profile 下四种残留文件都不存在。
func assertNoResidue(t *testing.T, dir, why string) {
	t.Helper()
	for _, name := range ProfileResidueFiles() {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s残留文件仍存在: %s", why, p)
		}
	}
}
