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

/// 本地前置代理（sidecar）句柄：一店一代理端口。
struct ProxyHandle {
    port: u16,
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
        port,
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

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let launch = cli_launch_spec(); // 支持 --server/--token/--launch-env 无人值守启动（便于自动化验证）
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![
            start_proxy,
            report_check,
            server_url,
            launch_env
        ])
        .setup(|app| {
            if let Some(l) = launch {
                let handle = app.handle().clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(std::time::Duration::from_millis(1200)).await;
                    for (shop, url) in l.shops {
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
                });
            }
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("启动紫鸟客户端失败");
}

/// CliLaunch 命令行无人值守启动参数。
struct CliLaunch {
    base_url: String,
    token: String,
    /// shops: (shop_id, 可选的目标 URL)——支持 `--launch-env <shopID>::<url>` 逐环境指定目标页
    shops: Vec<(String, Option<String>)>,
}

/// cli_launch_spec 解析 `--server <url> --token <jwt> --launch-env <shopID>[::<url>]`（可重复）。
fn cli_launch_spec() -> Option<CliLaunch> {
    let args: Vec<String> = std::env::args().collect();
    let mut base = None;
    let mut token = None;
    let mut shops: Vec<(String, Option<String>)> = Vec::new();
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
                    Some((id, url)) => shops.push((id.to_string(), Some(url.to_string()))),
                    None => shops.push((raw, None)),
                }
                i += 2;
            }
            _ => i += 1,
        }
    }
    if let (Some(base_url), Some(token)) = (base, token) {
        if !shops.is_empty() {
            return Some(CliLaunch { base_url, token, shops });
        }
    }
    None
}
