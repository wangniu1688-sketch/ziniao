package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ziniao/iam"
	"ziniao/network"
	"ziniao/platform/apierr"
	"ziniao/platform/types"
	"ziniao/shop"
)

// ---------- 假实现：让 8 个阶段的失败分支都能在没有 Chrome 的机器上断言 ----------

type fakeEnvControl struct {
	mu         sync.Mutex
	spec       *shop.LaunchSpec
	launchErr  error
	runningErr error
	// envIDFn 店铺 ID → 环境 ID：真实实现是 shop → env 反查，测试替身显式指定。
	envIDFn func(shopID string) string
	running int
	failed  []shop.MarkLaunchFailedInput
	stopped int
	status  string
	reset   int
}

func (f *fakeEnvControl) Launch(_ context.Context, in shop.LaunchInput) (*shop.LaunchSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	spec := f.spec
	if spec == nil {
		spec = &shop.LaunchSpec{EnvID: "env-1", ShopID: in.ShopID, Script: "/*fp*/", ExitMode: ExitDirect}
	}
	cp := *spec
	cp.ShopID = in.ShopID
	// 真实实现按 shop → env 反查；测试替身在这里显式指定映射（并发用例要开多个环境）。
	if f.envIDFn != nil {
		cp.EnvID = f.envIDFn(in.ShopID)
	}
	return &cp, nil
}

func (f *fakeEnvControl) MarkRunning(_ context.Context, in shop.MarkRunningInput) (*types.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return nil, f.runningErr
	}
	f.running++
	f.status = "running"
	return &types.Environment{ID: in.EnvID, ShopID: "shop-1", Status: "running"}, nil
}

func (f *fakeEnvControl) MarkLaunchFailed(_ context.Context, in shop.MarkLaunchFailedInput) (*types.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, in)
	f.status = "idle"
	return &types.Environment{ID: in.EnvID, Status: "idle"}, nil
}

func (f *fakeEnvControl) StopEnv(_ context.Context, _ shop.StopInput) (*types.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	// 与真实语义一致：running/created 收回 idle；空值（测试里没跑过启动）按 idle 处理。
	if f.status == "running" || f.status == "" {
		f.status = "idle"
	}
	return &types.Environment{ID: "env-1", ShopID: "shop-1", Status: f.status}, nil
}

func (f *fakeEnvControl) SaveThumbnail(_ context.Context, _ shop.ThumbnailInput) (*types.Environment, error) {
	return &types.Environment{ID: "env-1", Status: f.status}, nil
}

func (f *fakeEnvControl) ResetStaleRunning(_ context.Context, _ []string, _ map[string]bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset++
	return 0, nil
}

func (f *fakeEnvControl) lastFailure() shop.MarkLaunchFailedInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failed) == 0 {
		return shop.MarkLaunchFailedInput{}
	}
	return f.failed[len(f.failed)-1]
}

type fakeProc struct {
	pid      int
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	killed   bool
	portFile string
}

func (f *fakeProc) PID() int { return f.pid }

func (f *fakeProc) Done() <-chan struct{} { return f.done }

func (f *fakeProc) Terminate() error {
	f.once.Do(func() {
		f.mu.Lock()
		f.killed = true
		f.mu.Unlock()
		close(f.done)
	})
	return nil
}

func (f *fakeProc) wasKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

type fakeProxy struct {
	port   int
	mu     sync.Mutex
	closed bool
}

func (f *fakeProxy) Port() int { return f.port }

func (f *fakeProxy) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeProxy) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// newFakeCDP 起一个最小 CDP WebSocket 服务：应答所有调用，并记录收到的方法。
//
// 它同时是 cdp.go 的测试替身与被测对象：client 走真实协议，服务器是假的——
// 这样「参数有没有发对、事件有没有收到」都能断言，而不需要一台真 Chrome。
func newFakeCDP(t *testing.T, methods *[]string) string {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			var env cdpEnvelope
			if err := ws.ReadJSON(&env); err != nil {
				return
			}
			if methods != nil {
				*methods = append(*methods, env.Method)
			}
			var result any
			switch env.Method {
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "t1", "type": "page", "url": "about:blank"},
				}}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "s1"}
			case "Page.captureScreenshot":
				result = map[string]any{"data": "aGk="}
			default:
				result = map[string]any{}
			}
			raw, _ := json.Marshal(result)
			if err := ws.WriteJSON(cdpEnvelope{ID: env.ID, Result: raw}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// newManager 构造一个装好假件的管理器：真 CA 状态、真注册表、假进程/代理/CDP。
func newManager(t *testing.T) (*Manager, *fakeEnvControl, *fakeProxy, *fakeProc, *[]string) {
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
	// CA 就绪：启动前置条件之一，用假信任库（不真动本机证书）。
	var installed bool
	fakeCAOps(t, &installed)
	if _, err := m.InstallCA(); err != nil {
		t.Fatalf("CA 就绪失败: %v", err)
	}

	envs := &fakeEnvControl{}
	proxy := &fakeProxy{port: 45678}
	// 用真实 PID：procAlive 是本机的真实判断，假 PID 会被当成「进程已死」。
	proc := &fakeProc{pid: os.Getpid(), done: make(chan struct{})}
	var methods []string
	wsURL := newFakeCDP(t, &methods)

	m.envs = envs
	m.startProxy = func(network.TLSProxyConfig) (ProxyHandle, error) { return proxy, nil }
	m.startProc = func(_ string, _ []string) (ChromeProc, error) {
		// 模拟 Chrome 拉起后写出 DevToolsActivePort。
		if proc.portFile != "" {
			_ = os.WriteFile(proc.portFile, []byte("9333\n/devtools/browser/abc\n"), 0o600)
		}
		return proc, nil
	}
	m.cdpDial = func(ctx context.Context, _ string) (*CDPConn, error) { return DialCDP(ctx, wsURL) }
	m.layout = Layout{
		DataDir: m.layout.DataDir, ProfilesDir: m.layout.ProfilesDir,
		ExtensionsDir: m.layout.ExtensionsDir, SidepanelDir: m.layout.SidepanelDir,
		RuntimeDir: m.layout.RuntimeDir, ThumbnailsDir: m.layout.ThumbnailsDir,
		LogsDir: m.layout.LogsDir, CADir: m.layout.CADir,
	}
	return m, envs, proxy, proc, &methods
}

func actor() iam.Subject {
	return iam.Subject{UserID: "u1", TenantID: "t1", Username: "owner",
		Roles: []types.Role{{Code: "owner", DataScope: "all", Permissions: []string{"env:launch", "env:stop"}}}}
}

// ---------- 阶段与回滚矩阵 ----------

// TestStartHappyPath 全链路成功：CDP 连通后才提交 running，并登记运行时。
func TestStartHappyPath(t *testing.T) {
	m, envs, proxy, proc, methods := newManager(t)
	proc.portFile = m.layout.DevToolsActivePort("env-1")

	res, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()})
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if res.Status != "running" || res.PID != proc.pid || res.CDPPort != 9333 {
		t.Fatalf("启动结果不符: %+v", res)
	}
	if envs.running != 1 {
		t.Fatalf("running 应提交一次，实际 %d", envs.running)
	}
	if len(envs.failed) != 0 {
		t.Fatalf("成功路径不应有失败复位: %+v", envs.failed)
	}
	e, ok := m.RuntimeOf("env-1")
	if !ok || e.ProcState != ProcRunning {
		t.Fatalf("运行时未登记: %+v", e)
	}
	// 注入必须真的发出去了：这是指纹生效的唯一通道。
	if !containsMethod(*methods, "Page.addScriptToEvaluateOnNewDocument") {
		t.Fatalf("未注册 document_start 注入: %v", *methods)
	}
	// 再来一次：同一个环境不允许开第二个窗口。
	if _, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err == nil {
		t.Fatal("重复启动应被拒绝")
	}
	if proxy.isClosed() {
		t.Fatal("成功路径不应关掉本地代理")
	}
}

// TestStartRollbackMatrix 逐阶段打断，断言「本机资源收干净 + 状态复位」。
//
// 这是 M3 的核心保证：任何阶段失败都不能留下「端口占着 / 进程在跑 / 库里 running」。
func TestStartRollbackMatrix(t *testing.T) {
	cases := []struct {
		name      string
		stage     string
		mutate    func(t *testing.T, m *Manager, envs *fakeEnvControl, proxy *fakeProxy, proc *fakeProc)
		wantProxy bool // 期望本地代理已被关闭
		wantKill  bool // 期望进程已被收掉
	}{
		{
			name:  "阶段2-CA未就绪",
			stage: "2",
			mutate: func(_ *testing.T, m *Manager, _ *fakeEnvControl, _ *fakeProxy, _ *fakeProc) {
				m.ca = CAStatus{Present: false} // 卸载 CA 的效果
			},
			wantProxy: false, wantKill: false,
		},
		{
			name:  "阶段4-拉起进程失败",
			stage: "4",
			mutate: func(_ *testing.T, m *Manager, _ *fakeEnvControl, _ *fakeProxy, _ *fakeProc) {
				m.startProc = func(string, []string) (ChromeProc, error) {
					return nil, errors.New("拉起失败")
				}
			},
			wantProxy: true, wantKill: false,
		},
		{
			name:  "阶段5-没有端口文件",
			stage: "5",
			mutate: func(_ *testing.T, _ *Manager, _ *fakeEnvControl, _ *fakeProxy, proc *fakeProc) {
				proc.portFile = "" // Chrome 没写出 DevToolsActivePort
			},
			wantProxy: true, wantKill: true,
		},
		{
			name:  "阶段6-CDP连不上",
			stage: "6",
			mutate: func(_ *testing.T, m *Manager, _ *fakeEnvControl, _ *fakeProxy, proc *fakeProc) {
				proc.portFile = m.layout.DevToolsActivePort("env-1")
				m.cdpDial = func(context.Context, string) (*CDPConn, error) {
					return nil, errors.New("连接被拒")
				}
			},
			wantProxy: true, wantKill: true,
		},
		{
			name:  "阶段8-状态提交失败",
			stage: "8",
			mutate: func(_ *testing.T, m *Manager, envs *fakeEnvControl, _ *fakeProxy, proc *fakeProc) {
				proc.portFile = m.layout.DevToolsActivePort("env-1")
				envs.runningErr = errors.New("数据库不可用")
			},
			wantProxy: true, wantKill: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, envs, proxy, proc, _ := newManager(t)
			proc.portFile = m.layout.DevToolsActivePort("env-1")
			// 端口文件等不到时别让用例跑满 20s。
			m.cdpTimeout = 400 * time.Millisecond
			tc.mutate(t, m, envs, proxy, proc)

			if _, err := m.Start(context.Background(), StartInput{
				EnvID: "env-1", ShopID: "shop-1", Actor: actor(),
			}); err == nil {
				t.Fatalf("阶段 %s 应当失败", tc.stage)
			}
			if got := envs.lastFailure(); got.Stage != tc.stage {
				t.Fatalf("失败阶段应为 %s，实际 %q（%+v）", tc.stage, got.Stage, got)
			}
			if envs.running != 0 {
				t.Fatal("失败路径不得提交 running")
			}
			if _, ok := m.RuntimeOf("env-1"); ok {
				t.Fatal("失败路径不得留下运行时登记")
			}
			if tc.wantProxy && !proxy.isClosed() {
				t.Fatal("失败后本地代理应关闭（端口不能占着）")
			}
			if !tc.wantProxy && proxy.isClosed() {
				t.Fatal("该阶段之前不应动本地代理")
			}
			if tc.wantKill && !proc.wasKilled() {
				t.Fatal("失败后进程应收掉")
			}
			if !tc.wantKill && proc.wasKilled() {
				t.Fatal("该阶段之前不应杀进程")
			}
		})
	}
}

// TestStartRefusesSecretInArgv argv 里出现上游口令时宁可启动失败。
//
// 构造方式：把 UA 设成与口令相同——真实的「口令进 argv」就是这种拼错的结果，
// 断言的是扫描这道防线本身，而不是某个具体的拼接 bug。
func TestStartRefusesSecretInArgv(t *testing.T) {
	m, envs, _, proc, _ := newManager(t)
	proc.portFile = m.layout.DevToolsActivePort("env-1")
	m.envs.(*fakeEnvControl).spec = &shop.LaunchSpec{
		EnvID: "env-1", ShopID: "shop-1", UserAgent: "SECRET-PASS-123",
		ExitMode: ExitManual,
		Upstream: &shop.UpstreamCredential{Protocol: "http", Host: "1.2.3.4", Port: 8080,
			Username: "u", Password: "SECRET-PASS-123"},
	}
	if _, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err == nil {
		t.Fatal("argv 含口令时应拒绝启动")
	}
	if got := envs.lastFailure(); got.Stage != "4" {
		t.Fatalf("应在阶段 4 拦下，实际 %q", got.Stage)
	}
}

// TestStartResetsOnPhase1EgressProbeFailure P3：阶段 1 的业务拒绝（典型是启动前
// 出口拨测失败）既要走 MarkLaunchFailed 复位，也要**保留原始错误码**。
//
// 前半句是「禁止悬空 running」：即便此刻本机什么都没建，状态复位也必须发生，
// 否则一条坏代理会把环境留在「看起来在跑」的状态上。
// 后半句是「不要误报」：把 409（代理不通）包成 500（服务故障），运营会去查服务
// 而不是查代理——那正是 P3 想省掉的那一轮排障。
func TestStartResetsOnPhase1EgressProbeFailure(t *testing.T) {
	m, envs, proxy, proc, _ := newManager(t)
	// 自己接管两个「本机副作用」入口，才能断言它们**没被碰过**。
	proxyStarted, procStarted := false, false
	m.startProxy = func(network.TLSProxyConfig) (ProxyHandle, error) {
		proxyStarted = true
		return proxy, nil
	}
	m.startProc = func(_ string, _ []string) (ChromeProc, error) {
		procStarted = true
		return proc, nil
	}
	envs.launchErr = apierr.Conflict("出口代理拨测未通过（代理 CONNECT 失败: 407），已阻断启动")

	_, err := m.Start(context.Background(), StartInput{
		EnvID: "env-1", ShopID: "shop-1", Actor: actor(),
	})
	if err == nil {
		t.Fatal("阶段 1 的业务拒绝必须让启动失败")
	}
	if got := envs.lastFailure(); got.Stage != "1" {
		t.Fatalf("应在阶段 1 复位，实际 %q", got.Stage)
	}
	if proxyStarted {
		t.Fatal("阶段 1 失败不应起本地代理")
	}
	if procStarted {
		t.Fatal("阶段 1 失败不应拉起 Chrome")
	}
	// 原始错误码保留：不得被 failStart 包成 Internal。
	if strings.Contains(err.Error(), "环境启动失败（阶段") {
		t.Fatalf("阶段 1 的拒绝不得被包成通用内部错误: %v", err)
	}
	if !strings.Contains(err.Error(), "拨测未通过") {
		t.Fatalf("应保留可读的业务原因: %v", err)
	}
}

// TestUpstreamPasswordNeverSerialized 上游口令永不进 JSON（M3/T8）。
func TestUpstreamPasswordNeverSerialized(t *testing.T) {
	spec := shop.LaunchSpec{
		EnvID: "env-1", ShopID: "shop-1", ExitMode: ExitManual,
		Upstream: &shop.UpstreamCredential{Protocol: "http", Host: "1.2.3.4",
			Port: 8080, Username: "u", Password: "p@ss"},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "p@ss") || strings.Contains(string(b), "password") {
		t.Fatalf("启动单序列化后仍含口令: %s", b)
	}
	if !strings.Contains(string(b), "1.2.3.4") {
		t.Fatalf("地址应保留（排障要看）: %s", b)
	}
}

// ---------- 停止与崩溃复位 ----------

// TestStopReleasesEverything 停止：关窗口 → 收代理 → 摘注册表 → 状态回 idle。
func TestStopReleasesEverything(t *testing.T) {
	m, envs, proxy, proc, _ := newManager(t)
	proc.portFile = m.layout.DevToolsActivePort("env-1")
	if _, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	res, err := m.Stop(context.Background(), StopInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()})
	if err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if res.Status != "idle" {
		t.Fatalf("停止后应为 idle，实际 %s", res.Status)
	}
	if !proc.wasKilled() {
		t.Fatal("停止应结束浏览器进程")
	}
	if !proxy.isClosed() {
		t.Fatal("停止应关闭本地代理")
	}
	if _, ok := m.RuntimeOf("env-1"); ok {
		t.Fatal("停止后不应保留运行时登记")
	}
	if envs.stopped == 0 {
		t.Fatal("停止应调用业务侧关闭语义")
	}
	// 缩略图：尽力而为，拿到了就写盘。
	if res.Thumbnail == "" {
		t.Fatal("应当留下缩略图")
	}
	if _, err := os.Stat(res.Thumbnail); err != nil {
		t.Fatalf("缩略图应落盘: %v", err)
	}
}

// TestCrashResetsStatus 进程崩溃（或用户在窗口外关掉）后自动复位，
// 否则前端会一直显示「运行中」，而窗口早没了。
func TestCrashResetsStatus(t *testing.T) {
	m, envs, proxy, proc, _ := newManager(t)
	proc.portFile = m.layout.DevToolsActivePort("env-1")
	if _, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	// 模拟浏览器进程退出。
	_ = proc.Terminate()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if envs.stopped > 0 && !proxy.isClosed() == false {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if envs.stopped == 0 {
		t.Fatal("进程退出后应复位业务状态")
	}
	if !proxy.isClosed() {
		t.Fatal("进程退出后应关闭本地代理")
	}
	if _, ok := m.RuntimeOf("env-1"); ok {
		t.Fatal("进程退出后不应保留运行时登记")
	}
}

// TestStopWithoutLocalSession 换机器/重启后：本机没有会话，业务状态仍要能收回。
func TestStopWithoutLocalSession(t *testing.T) {
	m, envs, _, _, _ := newManager(t)
	res, err := m.Stop(context.Background(), StopInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()})
	if err != nil {
		t.Fatalf("无会话停止不应报错: %v", err)
	}
	if envs.stopped == 0 {
		t.Fatal("无会话时仍应收回业务状态（否则关不掉悬空 running）")
	}
	if res.Status != "idle" {
		t.Fatalf("应回到 idle，实际 %s", res.Status)
	}
}

// TestStartRejectsUnwired 未装配业务服务时启动必须报错，而不是开出一个没人管的窗口。
func TestStartRejectsUnwired(t *testing.T) {
	m, _, _, _, _ := newManager(t)
	m.envs = nil
	if _, err := m.Start(context.Background(), StartInput{EnvID: "env-1", ShopID: "shop-1", Actor: actor()}); err == nil {
		t.Fatal("未装配时应拒绝启动")
	}
}

// TestDevToolsActivePortParsing 端口文件解析（含缺第二行的情况）。
func TestDevToolsActivePortParsing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(p, []byte("9222\n/devtools/browser/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port, ws, err := readDevToolsActivePort(p)
	if err != nil || port != 9222 || ws != "/devtools/browser/x" {
		t.Fatalf("解析失败: %d %q %v", port, ws, err)
	}
	// Chrome 有时只写端口一行。
	if err := os.WriteFile(p, []byte("9333"), 0o600); err != nil {
		t.Fatal(err)
	}
	port, ws, err = readDevToolsActivePort(p)
	if err != nil || port != 9333 || ws != "" {
		t.Fatalf("单行解析失败: %d %q %v", port, ws, err)
	}
}

// TestWaitDevToolsPortRejectsHalfWrittenFile 端口文件只写了一半（缺 WS 路径）时
// 必须继续等，绝不能带着空路径进入 CDP 阶段。
//
// Chrome 是「先落端口号、再落 WS 路径」两次写的。抢在中间读到的文件看起来
// 完全合法（端口号能解析、mtime 也是本轮的），但拿去拼 ws://127.0.0.1:9333
// 一定连不上——而失败会报在阶段 6 的「连接被拒」上，根因（文件没写完）看不见。
func TestWaitDevToolsPortRejectsHalfWrittenFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(p, []byte("9333\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := &fakeProc{pid: os.Getpid(), done: make(chan struct{})}
	port, ws, err := waitDevToolsPort(context.Background(), p, 300*time.Millisecond, proc,
		time.Now().Add(-time.Second))
	if err == nil {
		t.Fatalf("半截文件不应被接受: port=%d ws=%q", port, ws)
	}
	if !strings.Contains(err.Error(), "缺 WS 路径") {
		t.Fatalf("错误信息应指出缺 WS 路径，实际: %v", err)
	}
	// 等一个不存在的文件：超时后必须报错，不能一直等。
	if _, _, err := waitDevToolsPort(context.Background(), filepath.Join(dir, "nope"),
		300*time.Millisecond, nil, time.Time{}); err == nil {
		t.Fatal("等待不存在的文件应超时报错")
	}
	// 上一轮遗留的端口文件（时间戳是 1 小时前）不能当本轮的用。
	stale := filepath.Join(dir, "stale")
	if err := os.WriteFile(stale, []byte("9222\n/devtools/browser/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := waitDevToolsPort(context.Background(), stale, 400*time.Millisecond,
		nil, time.Now()); err == nil {
		t.Fatal("上一轮遗留的端口文件不应被当成本轮的")
	}
}

// TestEnvIDFromCmdline 孤儿判据：必须认出「用我们的 profile 启动的 Chrome」，
// 同时不能误伤运营自己开的浏览器（没有 --user-data-dir 或目录不匹配）。
func TestEnvIDFromCmdline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	sep := string(separator())
	got := envIDFromCmdline(`"C:\Chrome\chrome.exe" --user-data-dir=`+root+sep+`env-9 --remote-debugging-port=0`, root)
	if got != "env-9" {
		t.Fatalf("应解析出 env-9，实际 %q", got)
	}
	if envIDFromCmdline(`"C:\Chrome\chrome.exe" --user-data-dir=C:\Users\me\Chrome\Default`, root) != "" {
		t.Fatal("别人的 profile 不应被认成我们的")
	}
	if envIDFromCmdline(`"C:\Chrome\chrome.exe"`, root) != "" {
		t.Fatal("没有 --user-data-dir 不应被认成我们的")
	}
}

// TestReconcileClosesOrphans 对账：陈旧条目删掉、孤儿进程关掉、库里残留 running 复位。
func TestReconcileClosesOrphans(t *testing.T) {
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

	// 陈旧条目：PID 指向一个不可能存在的进程。
	_ = m.Register(RuntimeEntry{EnvID: "dead-1", PID: 999997, ProcState: ProcRunning, TenantID: "t1"})
	// 活条目：PID 是本机当前进程（必然活着）。
	_ = m.Register(RuntimeEntry{EnvID: "alive-1", PID: os.Getpid(), ProcState: ProcRunning, TenantID: "t1", CDPPort: 1})
	// 端口文件存在 ⇒ 触发重挂。
	_ = os.MkdirAll(m.layout.ProfileDir("alive-1"), 0o700)
	_ = os.WriteFile(m.layout.DevToolsActivePort("alive-1"), []byte("9222\n/devtools/browser/x\n"), 0o600)

	// 孤儿：一个用我们 profile 启动、但注册表里没有的进程。
	//
	// 这里真的起一个无关子进程再让对账关掉它——绝不能拿 os.Getpid() 当孤儿，
	// 那样对账会把测试进程自己杀掉（表现为「用例静默失败，没有任何断言输出」）。
	orphanPID := spawnSleepProcess(t)
	orig := procSnapshot
	t.Cleanup(func() { procSnapshot = orig })
	procSnapshot = func() ([]ProcInfo, error) {
		return []ProcInfo{{PID: orphanPID, Cmdline: fmt.Sprintf(`chrome.exe --user-data-dir=%s%sorphan-1`,
			m.layout.ProfilesDir, string(separator()))}}, nil
	}

	res := m.reconcile()
	if res.Dead != 1 {
		t.Fatalf("陈旧条目应删除 1 条，实际 %d", res.Dead)
	}
	if res.Alive != 1 || res.Relinked != 1 {
		t.Fatalf("活条目应重挂: %+v", res)
	}
	if res.Orphans != 1 || res.Closed != 1 {
		t.Fatalf("应发现并关闭 1 个孤儿，实际 %+v", res)
	}
	if procAlive(orphanPID) {
		t.Fatal("孤儿进程应已被关闭")
	}
	if envs.reset == 0 {
		t.Fatal("应对账复位库里残留的 running")
	}
	if _, ok := m.RuntimeOf("dead-1"); ok {
		t.Fatal("陈旧条目不应残留")
	}
	if e, ok := m.RuntimeOf("alive-1"); !ok || e.CDPPort != 9222 {
		t.Fatalf("重挂后端口应更新为 9222: %+v", e)
	}
}

// TestReconcileDegradesWithoutSnapshot 进程快照拿不到 → 降级，绝不把「没查」当「没有」。
func TestReconcileDegradesWithoutSnapshot(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{DataDir: t.TempDir(), ChromePath: exe, SingleInstance: noLock()})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	orig := procSnapshot
	t.Cleanup(func() { procSnapshot = orig })
	procSnapshot = func() ([]ProcInfo, error) { return nil, errors.New("wmic 被禁用") }

	res := m.reconcile()
	if res.SnapshotOK {
		t.Fatal("快照不可用时应标记 SnapshotOK=false")
	}
	if m.Capabilities()["reconcile"] {
		t.Fatal("快照不可用时 reconcile 能力位应为 false")
	}
}

// TestConcurrentStartDifferentEnvs 多环境并发启动：不同环境必须能并行（perEnv 锁
// 只串行同一环境），注册表在并发写入下不能坏。
//
// 本机没有 C 编译器（跑不了 -race），所以用并发度 + 事后校验来兜：
// 真有数据竞争时，5 个环境的注册表条目会互相覆盖，条目数必然不满。
func TestConcurrentStartDifferentEnvs(t *testing.T) {
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
	defer func() { _ = m.Close() }()
	var installed bool
	fakeCAOps(t, &installed)
	if _, err := m.InstallCA(); err != nil {
		t.Fatal(err)
	}
	m.envs = &fakeEnvControl{envIDFn: func(shopID string) string { return strings.TrimPrefix(shopID, "shop-") }}
	m.startProxy = func(network.TLSProxyConfig) (ProxyHandle, error) { return &fakeProxy{port: 45678}, nil }
	wsURL := newFakeCDP(t, nil)
	m.cdpDial = func(ctx context.Context, _ string) (*CDPConn, error) { return DialCDP(ctx, wsURL) }
	var mu sync.Mutex
	m.startProc = func(_ string, argv []string) (ChromeProc, error) {
		// 从 argv 反推 profile 目录，模拟 Chrome 把端口写进对应 profile。
		dir := ""
		for _, a := range argv {
			if strings.HasPrefix(a, "--user-data-dir=") {
				dir = strings.TrimPrefix(a, "--user-data-dir=")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if dir != "" {
			_ = os.MkdirAll(dir, 0o700)
			_ = os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("9333\n/devtools/browser/x\n"), 0o600)
		}
		return &fakeProc{pid: os.Getpid(), done: make(chan struct{})}, nil
	}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("env-%d", i)
			_, errs[i] = m.Start(context.Background(), StartInput{EnvID: id, ShopID: "shop-" + id, Actor: actor()})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("环境 %d 启动失败: %v", i, err)
		}
	}
	if got := m.registry.Len(); got != n {
		t.Fatalf("并发启动后注册表应有 %d 条，实际 %d", n, got)
	}
	// 全部停止后应清空。
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("env-%d", i)
		if _, err := m.Stop(context.Background(), StopInput{EnvID: id, ShopID: "shop-" + id, Actor: actor()}); err != nil {
			t.Fatalf("环境 %s 停止失败: %v", id, err)
		}
	}
	if got := m.registry.Len(); got != 0 {
		t.Fatalf("全部停止后注册表应为空，实际 %d", got)
	}
}

// spawnSleepProcess 起一个能活一会儿的无关子进程（对账关闭孤儿时用）。
func spawnSleepProcess(t *testing.T) int {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "30", "127.0.0.1")
	} else {
		cmd = exec.Command("sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("无法起子进程，跳过孤儿关闭断言: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

func containsMethod(list []string, want string) bool {
	for _, m := range list {
		if m == want {
			return true
		}
	}
	return false
}
