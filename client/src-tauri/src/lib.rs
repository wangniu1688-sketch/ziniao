//! 紫鸟浏览器客户端外壳。
//!
//! 设计红线：
//!   1. 客户端只做执行，不做权限决策（L3/L6）——裁决结果一律来自服务端；
//!   2. TLS 指纹不在这里构造，交给 Go 侧 uTLS 前置代理（sidecar），
//!      保证"声称的内核版本"与"实际 TLS 指纹"同源（L2，致命项）；
//!   3. 凭证明文不落盘、不进日志（L4）。

use serde::{Deserialize, Serialize};
use std::process::{Child, Command};

mod env_window;
mod probe_shell;

/// 本地前置代理（sidecar）句柄：一店一代理端口。
struct ProxyHandle {
    child: Option<Child>,
}

impl Drop for ProxyHandle {
    fn drop(&mut self) {
        if let Some(child) = self.child.as_mut() {
            let _ = child.kill();
        }
    }
}

/// 启动本地前置代理：把设备出口与内核基线绑定。
/// 真实实现应传入设备代理配置（host/port/协议）与内核版本号，
/// 由 Go 侧按 uTLS ClientHelloID 构造握手。
#[tauri::command]
fn start_proxy(kernel_major: u16, device_host: String, device_port: u16) -> Result<u16, String> {
    let port = 1080u16 + kernel_major % 10; // 占位：真实端口由端口池分配
    let child = Command::new("ziniao-proxy")
        .args([
            "--listen".to_string(),
            format!("127.0.0.1:{port}"),
            "--kernel".to_string(),
            kernel_major.to_string(),
            "--upstream".to_string(),
            format!("{device_host}:{device_port}"),
        ])
        .spawn()
        .map_err(|e| format!("启动前置代理失败: {e}"))?;
    // 句柄需在应用状态中长期持有，此处为骨架示意（Drop 时回收进程）。
    let _handle = ProxyHandle {
        child: Some(child),
    };
    Ok(port)
}

/// 环境自检观测值：由前端 JS 采集后回传服务端比对。
#[derive(Debug, Serialize, Deserialize)]
struct Observed {
    baseline_hash: Option<String>,
    tls_ja3: Option<String>,
    #[serde(flatten)]
    rest: std::collections::HashMap<String, String>,
}

/// 上报自检观测值到云端（服务端计算自洽性/漂移/基线校验）。
#[tauri::command]
async fn report_check(
    base_url: String,
    token: String,
    shop_id: String,
    observed: Observed,
) -> Result<serde_json::Value, String> {
    let client = reqwest::Client::new();
    let resp = client
        .post(format!(
            "{base_url}/api/v1/shops/{shop_id}/environment/check"
        ))
        .bearer_auth(token)
        .json(&observed)
        .send()
        .await
        .map_err(|e| format!("自检上报失败: {e}"))?;
    resp.json().await.map_err(|e| format!("解析自检结果失败: {e}"))
}

/// 服务端地址：默认本机，可用 ZINIAO_SERVER 覆盖。
#[tauri::command]
fn server_url() -> String {
    std::env::var("ZINIAO_SERVER").unwrap_or_else(|_| "http://127.0.0.1:8080".to_string())
}

/// launch_env：向服务端申请环境启动单，并打开一个隔离的浏览器窗口。
///
/// 客户端只做“执行”：UA、注入脚本、代理端口、内核基线全部来自服务端裁决结果。
#[tauri::command]
async fn launch_env(app: tauri::AppHandle, input: env_window::LaunchEnvInput) -> Result<String, String> {
    env_window::launch_env(app, input).await
}

/// stop_env：关闭店铺环境窗口并上报服务端（状态 running → idle）。
#[tauri::command]
async fn stop_env(
    app: tauri::AppHandle,
    input: env_window::StopEnvInput,
) -> Result<env_window::StopEnvOutput, String> {
    env_window::stop_env(app, input).await
}

/// env_nav：环境窗口工具条（browser.html）的导航命令（后退/前进/刷新/地址栏跳转）。
///
/// async 必须保留：Windows 上同步 command 里操作 webview 会死锁（WebView2 已知问题）。
#[tauri::command]
async fn env_nav(app: tauri::AppHandle, input: env_window::EnvNavInput) -> Result<(), String> {
    env_window::env_nav(app, input)
}

/// deactivate_env：停用环境（退出服务，服务端归还出口绑定）。
///
/// 客户端不会因此关窗口：本机还开着窗口时这一条直接拒绝，
/// 否则会留下「环境已停用、窗口与代理进程还在跑」的孤儿。
#[tauri::command]
async fn deactivate_env(
    app: tauri::AppHandle,
    input: env_window::StopEnvInput,
) -> Result<String, String> {
    env_window::deactivate_env(app, input).await
}

/// delete_env：删除环境（终态）——先收本机窗口，再删服务端行，最后清本机 profile。
#[tauri::command]
async fn delete_env(
    app: tauri::AppHandle,
    input: env_window::StopEnvInput,
) -> Result<env_window::DeleteEnvOutput, String> {
    env_window::delete_env(app, input).await
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let cli = cli_spec(); // 支持 --server/--token/--launch-env/--delete-env 无人值守操作（便于自动化验证）
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![
            start_proxy,
            report_check,
            server_url,
            launch_env,
            stop_env,
            env_nav,
            deactivate_env,
            delete_env
        ])
        .setup(|app| {
            // --probe-shell：打开多 webview 原型窗口做可行性验证（探路用，非生产路径）。
            // 放在 async 任务里建 webview：Windows 上同步建 webview 会死锁（WebView2 已知问题）。
            if std::env::args().any(|a| a == "--probe-shell") {
                let handle = app.handle().clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(std::time::Duration::from_millis(600)).await;
                    if let Err(e) = probe_shell::run(&handle) {
                        eprintln!("[probe] 原型失败: {e}");
                        std::process::exit(1);
                    }
                });
            }
            if let Some(l) = cli {
                let handle = app.handle().clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(std::time::Duration::from_millis(1200)).await;
                    for (shop, url) in l.launches {
                        let input = env_window::LaunchEnvInput {
                            base_url: l.base_url.clone(),
                            token: l.token.clone(),
                            shop_id: shop,
                            open_url: url,
                        };
                        match env_window::launch_env(handle.clone(), input).await {
                            Ok(msg) => eprintln!("[ziniao] {msg}"),
                            Err(e) => eprintln!("[ziniao] 启动失败: {e}"),
                        }
                    }
                    // --deactivate-env：停用（服务端会归还出口绑定）。
                    // 与 --launch-env 同进程时，「本机窗口还开着」这条守卫会生效——
                    // 那是用例唯一能断言到的地方（跨进程看的是同一份登记表，不共享）。
                    for shop in l.deactivates {
                        let input = env_window::StopEnvInput {
                            base_url: l.base_url.clone(),
                            token: l.token.clone(),
                            shop_id: shop,
                        };
                        match env_window::deactivate_env(handle.clone(), input).await {
                            Ok(st) => eprintln!("[ziniao] 停用环境成功（{st}）"),
                            Err(e) => eprintln!("[ziniao] 停用环境失败: {e}"),
                        }
                    }
                    // --delete-env：无人值守走完「收窗口 → 删服务端行 → 清本机 profile」。
                    // 这条链是不可逆操作，只靠人手点一次不算验证过。
                    for shop in l.deletes {
                        let input = env_window::StopEnvInput {
                            base_url: l.base_url.clone(),
                            token: l.token.clone(),
                            shop_id: shop,
                        };
                        match env_window::delete_env(handle.clone(), input).await {
                            Ok(r) => eprintln!(
                                "[ziniao] 删除环境 {} 成功（profile_purged={}）：{}",
                                r.env_id, r.profile_purged, r.note
                            ),
                            Err(e) => eprintln!("[ziniao] 删除环境失败: {e}"),
                        }
                    }
                });
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("启动紫鸟客户端失败")
        .run(|app, event| {
            // 进程退出：把仍打开的环境逐个上报关闭。
            // 尽力而为——退出时异步任务未必跑得完，断电/强杀更是一点机会都没有，
            // 所以服务端还有巡检兜底（env.reclaim，默认 12 小时）。
            // 不做这一步，上面每个窗口都要等 12 小时才被收回。
            if let tauri::RunEvent::Exit = event {
                let handle = app.clone();
                tauri::async_runtime::spawn(async move {
                    env_window::stop_all_open(handle).await;
                });
            }
        });
}

/// CliSpec 命令行无人值守操作参数。
struct CliSpec {
    base_url: String,
    token: String,
    /// launches: (shop_id, 可选的目标 URL)——`--launch-env <shopID>::<url>` 逐环境指定目标页
    launches: Vec<(String, Option<String>)>,
    /// deactivates: `--deactivate-env <shopID>`（可重复）——与 --launch-env 同进程时
    /// 正好能验证「本机窗口还开着 → 拒绝停用」这条守卫
    deactivates: Vec<String>,
    /// deletes: `--delete-env <shopID>`（可重复）走完整删除链，供回归 harness 使用
    deletes: Vec<String>,
}

/// cli_spec 解析 `--server <url> --token <jwt> --launch-env <shopID>[::<url>] --delete-env <shopID>`。
///
/// 只认「两个独立参数」：`--launch-env=<id>` 会被静默跳过（解析器不认 `=` 形式），
/// 无人值守启动第一次就栽在这，表现为"客户端起来了但什么都没发生"。
fn cli_spec() -> Option<CliSpec> {
    let args: Vec<String> = std::env::args().collect();
    let mut base = None;
    let mut token = None;
    let mut launches: Vec<(String, Option<String>)> = Vec::new();
    let mut deactivates: Vec<String> = Vec::new();
    let mut deletes: Vec<String> = Vec::new();
    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "--server" if i + 1 < args.len() => {
                base = Some(args[i + 1].clone());
                i += 2;
            }
            "--token" if i + 1 < args.len() => {
                token = Some(args[i + 1].clone());
                i += 2;
            }
            "--launch-env" if i + 1 < args.len() => {
                let raw = args[i + 1].clone();
                match raw.split_once("::") {
                    Some((id, url)) => launches.push((id.to_string(), Some(url.to_string()))),
                    None => launches.push((raw, None)),
                }
                i += 2;
            }
            "--deactivate-env" if i + 1 < args.len() => {
                deactivates.push(args[i + 1].clone());
                i += 2;
            }
            "--delete-env" if i + 1 < args.len() => {
                deletes.push(args[i + 1].clone());
                i += 2;
            }
            _ => i += 1,
        }
    }
    if let (Some(base_url), Some(token)) = (base, token) {
        if !launches.is_empty() || !deactivates.is_empty() || !deletes.is_empty() {
            return Some(CliSpec {
                base_url,
                token,
                launches,
                deactivates,
                deletes,
            });
        }
    }
    None
}
