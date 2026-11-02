package browser

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ziniao/network"
)

// CAStatus 本地 MITM 根证书状态（M1：ca_ready 的来源）。
//
// 为什么它是启动的**前置条件**：本地代理是 MITM 架构（network 包用本地 CA 动态
// 签发目标站点证书），CA 不在信任库里，浏览器打开任何 HTTPS 站点都会报证书错误
// ——而运营看到的第一反应是「这个环境坏了」，不会想到证书。
//
// 因此：ca_ready=false 时启动流程直接阻断并给出明确提示，绝不靠
// --ignore-certificate-errors 绕过（那会让安全中心失去意义，且等于把「证书错误」
// 这个真实信号一起屏蔽掉）。
type CAStatus struct {
	Dir       string `json:"dir"`
	CertPath  string `json:"cert_path"`
	Present   bool   `json:"present"`   // ca.crt / ca.key 是否落盘
	Installed bool   `json:"installed"` // 是否已进入当前用户信任库
	Ready     bool   `json:"ready"`     // Present && Installed
	Detail    string `json:"detail,omitempty"`
}

// ErrCANotReady CA 未就绪：启动流程的前置检查失败。
var ErrCANotReady = errors.New("browser: MITM 根证书未就绪（CA 未安装到当前用户信任库）")

// ---------- 本机信任库动作（可替换，便于单测不真的改信任库）----------

// caOperations 对「当前用户信任库」的读写动作集合。
//
// 抽成变量而不是在函数里直接 exec certutil，只有一个理由：**装/删根证书是有副作用
// 的动作**，单测如果真跑，就会往开发机与 CI 机器的信任库里塞一张临时 CA——那是
// 测试绝不该留下的痕迹。默认实现走 network 包（与 cmd/ziniao-proxy -install-ca
// 同一条路径），单测注入假实现即可覆盖状态机与阻断逻辑。
type caOperations struct {
	install   func(dir string) error
	uninstall func(dir string) error
	installed func(cn string) (bool, string)
}

var caOps = caOperations{
	install:   installToUserStore,
	uninstall: uninstallFromUserStore,
	installed: userStoreHas,
}

func installToUserStore(dir string) error {
	ca, err := network.LoadOrCreateCA(dir)
	if err != nil {
		return err
	}
	return ca.EnsureInstalled()
}

func uninstallFromUserStore(dir string) error {
	ca, err := network.LoadOrCreateCA(dir)
	if err != nil {
		return err
	}
	return ca.Uninstall()
}

// userStoreHas 查询证书是否已装进当前用户信任库。
//
// Windows：certutil -user -store Root 全量列出后按 CN 匹配（certutil 没有
// 「按 CN 查单个」的可靠子命令，-verifystore 对缺证也返回 0）。只在启动期调用
// 一次，不在热路径。
//
// 其它平台：TODO（keychain / NSS）——这里按「文件在即视为就绪」处理，并如实
// 写进 Detail，不让调用方误以为已经验证过。
func userStoreHas(cn string) (bool, string) {
	if runtime.GOOS != "windows" {
		return true, "非 Windows：信任库由系统管理（keychain/NSS），未做安装校验"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "certutil", "-user", "-store", "Root").CombinedOutput()
	if err != nil {
		return false, "查询用户信任库失败：" + strings.TrimSpace(string(out))
	}
	if strings.Contains(string(out), cn) {
		return true, "已安装到当前用户信任库"
	}
	return false, "未安装到当前用户信任库（启动会阻断，请先安装 CA）"
}

// ---------- 对外动作 ----------

// CheckCA 只读检查 CA 状态（不生成、不安装）。
func CheckCA(dir string) CAStatus {
	crt := filepath.Join(dir, "ca.crt")
	key := filepath.Join(dir, "ca.key")
	st := CAStatus{Dir: dir, CertPath: crt}
	_, e1 := os.Stat(crt)
	_, e2 := os.Stat(key)
	st.Present = e1 == nil && e2 == nil
	if !st.Present {
		st.Detail = "尚未生成 MITM 根证书"
		return st
	}
	installed, detail := caOps.installed(network.CACommonName)
	st.Installed, st.Detail = installed, detail
	st.Ready = st.Present && st.Installed
	return st
}

// EnsureCA 生成（若缺）并安装（若未装）本地 CA —— 当前用户信任库，不需要管理员权限。
//
// 这是 M3 启动前置步骤的唯一入口（系统设置页的「安装 CA」按钮走它）。
// 改信任库是**有副作用**的动作，必须由人显式触发：启动器自己只在
// system/info 里如实报告 ca_ready，绝不偷偷往信任库里塞证书。
func EnsureCA(dir string) (CAStatus, error) {
	if err := caOps.install(dir); err != nil {
		return CheckCA(dir), err
	}
	return CheckCA(dir), nil
}

// UninstallCA 从当前用户信任库移除本地 CA（卸载 / 合规要求）。
//
// 幂等：信任库里本来就没有这张证书时，按「已达成目标」返回 nil——
// 「卸载失败」只有在真的移除不掉时才应该报，否则前端会一直红着。
func UninstallCA(dir string) error {
	if err := caOps.uninstall(dir); err != nil {
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "not found") ||
			strings.Contains(msg, "找不到") || strings.Contains(msg, "0x80092004") {
			return nil
		}
		return err
	}
	return nil
}
