package browser

import (
	"os"
	"path/filepath"
	"testing"

	"ziniao/network"
)

// fakeCAOps 用假实现替换「当前用户信任库」的读写动作。
//
// 为什么必须替换：真的 certutil -addstore/-delstore 会往开发机与 CI 机器的信任库里
// 装/删证书——那是有副作用的操作，测试不该留下痕迹，更不该让单测依赖机器上有没有
// certutil。状态机与阻断逻辑用假实现覆盖，真实安装留在 M3 手动闭环里验证。
func fakeCAOps(t *testing.T, installed *bool) {
	t.Helper()
	orig := caOps
	t.Cleanup(func() { caOps = orig })
	caOps = caOperations{
		// 生成证书材料是真的（离线、无副作用），只有「进信任库」这一步是假的。
		install: func(dir string) error {
			if _, err := network.LoadOrCreateCA(dir); err != nil {
				return err
			}
			*installed = true
			return nil
		},
		uninstall: func(dir string) error { *installed = false; return nil },
		installed: func(string) (bool, string) {
			if *installed {
				return true, "已安装到当前用户信任库"
			}
			return false, "未安装到当前用户信任库（启动会阻断，请先安装 CA）"
		},
	}
}

// TestCheckCAEmptyDir 没有证书材料时：present=false / ready=false，并给出可读原因。
func TestCheckCAEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	st := CheckCA(dir)
	if st.Present || st.Ready || st.Installed {
		t.Fatalf("空目录不应判为就绪: %+v", st)
	}
	if st.Detail == "" {
		t.Fatal("未就绪时必须给出可读原因（前端直接展示这段话）")
	}
	if st.CertPath != filepath.Join(dir, "ca.crt") {
		t.Fatalf("证书路径不符: %s", st.CertPath)
	}
}

// TestCAInstallUninstallCycle 装 → 查 → 卸 → 查 的状态机：
// ready 必须严格等于 present && installed，卸载后立刻回到未就绪。
func TestCAInstallUninstallCycle(t *testing.T) {
	var installed bool
	fakeCAOps(t, &installed)
	dir := filepath.Join(t.TempDir(), "ca")

	if _, err := EnsureCA(dir); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	st := CheckCA(dir)
	if !st.Present || !st.Installed || !st.Ready {
		t.Fatalf("安装后应就绪: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatalf("安装应顺带生成证书材料: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatalf("安装应顺带生成私钥: %v", err)
	}
	// 幂等：重复安装不报错、状态不变。
	if _, err := EnsureCA(dir); err != nil {
		t.Fatalf("重复安装应幂等: %v", err)
	}
	if err := UninstallCA(dir); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	after := CheckCA(dir)
	if !after.Present {
		t.Fatal("卸载只动信任库，证书材料应保留（下次安装可直接复用）")
	}
	if after.Installed || after.Ready {
		t.Fatalf("卸载后不应再判为就绪: %+v", after)
	}
}

// TestUninstallCAIsIdempotent 信任库里本来就没有这张证书时，卸载按「已达成目标」处理。
func TestUninstallCAIsIdempotent(t *testing.T) {
	orig := caOps
	t.Cleanup(func() { caOps = orig })
	caOps = caOperations{
		install:   func(string) error { return nil },
		uninstall: func(string) error { return errNoSuchCert },
		installed: func(string) (bool, string) { return false, "" },
	}
	if err := UninstallCA(t.TempDir()); err != nil {
		t.Fatalf("证书本就不存在时卸载应返回 nil，实际 %v", err)
	}
}

type fakeErr struct{}

func (fakeErr) Error() string {
	return "mitm: 移除根证书失败: CertUtil: -delstore 命令失败: 0x80092004"
}

var errNoSuchCert = fakeErr{}

// TestCABlocksLaunchUntilInstalled ca_ready 阻断闭环：
// 装 CA 之前 Precheck 必须是 ErrCANotReady 且 launch 能力为 false；装完之后放行。
func TestCABlocksLaunchUntilInstalled(t *testing.T) {
	var installed bool
	fakeCAOps(t, &installed)

	// 用一个真实存在的文件冒充浏览器可执行文件：Locator.Detect 只做 os.Stat，
	// 这样「浏览器已安装」这一项为 true，剩下的变量就只有 CA。
	exe := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		DataDir: t.TempDir(), ChromePath: exe, SingleInstance: noLock(),
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = m.Close() }()

	if _, err := m.Precheck("env-1"); err != ErrCANotReady {
		t.Fatalf("CA 未就绪时启动必须被阻断，实际 %v", err)
	}
	if m.Capabilities()["launch"] {
		t.Fatal("CA 未就绪时 launch 能力必须为 false")
	}

	if _, err := m.InstallCA(); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if !m.CAReady() || !m.Capabilities()["launch"] {
		t.Fatalf("安装后应放行: ca=%+v caps=%v", m.CA(), m.Capabilities())
	}
	dir, err := m.Precheck("env-1")
	if err != nil {
		t.Fatalf("安装后启动前置检查应通过，实际 %v", err)
	}
	if dir == "" {
		t.Fatal("前置检查应返回 profile 目录")
	}

	if _, err := m.UninstallCA(); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if m.CAReady() {
		t.Fatal("卸载后 ca_ready 必须为 false")
	}
	if _, err := m.Precheck("env-1"); err != ErrCANotReady {
		t.Fatalf("卸载后启动应重新被阻断，实际 %v", err)
	}
}
