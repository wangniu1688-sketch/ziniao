//! 环境窗口：按店铺环境打开**真实浏览器窗口**。
//!
//! 红线对齐服务端：
//!   1. UA / 注入脚本 / 代理端口 / 内核基线全部来自服务端下发的启动单，
//!      客户端不得随机化任何字段（L2：随机化 = 与画像库脱钩 = 致命）；
//!   2. 每个环境一个独立 data_directory（Cookie / localStorage / IndexedDB 物理隔离），
//!      多账号因此不会串号；
//!   3. 若本机存在 Go 侧 ziniao-proxy（uTLS 前置代理），窗口流量走它，
//!      保证 TLS 指纹与声称的内核基线同源；找不到就直连并在标题标注。

use std::collections::HashMap;
use std::path::PathBuf;

use tauri::Manager;

use serde::{Deserialize, Serialize};

/// LaunchSpec 与服务端 shop.LaunchSpec 对齐（字段缺失即视为异常，拒绝启动）。
#[derive(Debug, Clone, Serialize, Deserialize)]
struct LaunchSpec {
    env_id: String,
    shop_id: String,
    #[serde(default)]
    shop_name: Option<String>,
    baseline_hash: String,
    #[serde(default)]
    kernel_major: i32,
    user_agent: String,
    #[serde(default)]
    proxy_port: i32,
    #[serde(default)]
    tls_hello_id: Option<String>,
    #[serde(default)]
    country: Option<String>,
    script: String,
    #[serde(default)]
    script_bytes: usize,
    #[serde(default)]
    open_url: String,
    #[serde(default)]
    snapshot: Option<HashMap<String, String>>,
}

/// LaunchEnvInput 前端传入：服务端地址、令牌、店铺、可选的打开 URL。
#[derive(Debug, Deserialize)]
pub struct LaunchEnvInput {
    pub base_url: String,
    pub token: String,
    pub shop_id: String,
    #[serde(default)]
    pub open_url: Option<String>,
}

/// launch_env 的真实实现（命令入口在 lib.rs，避免 command 宏的作用域问题）。
pub async fn launch_env(app: tauri::AppHandle, input: LaunchEnvInput) -> Result<String, String> {
    // ---- 1. 向服务端要启动单（裁决在服务端，客户端只是执行者） ----
    let base = input.base_url.trim_end_matches('/').to_string();
    let url = format!(
        "{}/api/v1/shops/{}/environment/launch",
        base,
        urlencoding(&input.shop_id)
    );
    let client = reqwest::Client::builder()
        .build()
        .map_err(|e| format!("构造 HTTP 客户端失败: {e}"))?;
    let resp = client
        .post(&url)
        .bearer_auth(&input.token)
        .json(&serde_json::json!({ "open_url": input.open_url.clone().unwrap_or_default() }))
        .send()
        .await
        .map_err(|e| format!("无法连接服务端（是否已启动 ziniao-server？）: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("服务端拒绝了启动请求（HTTP {status}）：{body}"));
    }
    let spec: LaunchSpec = resp
        .json()
        .await
        .map_err(|e| format!("启动单解析失败: {e}"))?;
    if spec.script.is_empty() || spec.user_agent.is_empty() {
        return Err("启动单缺字段（脚本或 UA 为空），拒绝启动".to_string());
    }

    // ---- 2. 本地 uTLS 前置代理（有则启用，保证 TLS 指纹与内核基线同源） ----
    let proxy_running = ensure_proxy(&app, &spec).await;

    // ---- 3. 目录隔离：一个环境一个 profile 目录 ----
    let profile_dir = app
        .path()
        .app_local_data_dir()
        .map(|d| d.join("profiles").join(&spec.env_id))
        .unwrap_or_else(|_| PathBuf::from(".ziniao-profiles").join(&spec.env_id));
    let _ = std::fs::create_dir_all(&profile_dir);

    // ---- 4. 目标 URL：优先调用方指定 → 服务端指定 → 默认指纹实测页 ----
    let open_url = input
        .open_url
        .filter(|s| !s.is_empty())
        .or_else(|| Some(spec.open_url.clone()).filter(|s| !s.is_empty()))
        .unwrap_or_else(|| format!("{base}/fp-test"));

    // ---- 5. 开窗口（注入脚本在文档创建前执行，时序不能晚） ----
    let label = format!("env-{}", spec.env_id.to_lowercase());
    if let Some(w) = app.get_webview_window(&label) {
        // 同一环境已开着：**导航到本次目标页**并聚焦（重复启动 = 换目标页，
        // 而不是开出第二个不一致的窗口——同一环境永远只有一个窗口、一个 profile）
        let js = format!("location.replace({:?})", open_url);
        let _ = w.eval(&js);
        let _ = w.set_focus();
        return Ok(format!("已导航并聚焦环境窗口 {}", spec.env_id));
    }

    let title = format!(
        "{} · 环境 {} · Chrome {} · {}{}",
        spec.shop_name.clone().unwrap_or_else(|| spec.shop_id.clone()),
        // env_id 是 ULID：前 10 位是毫秒时间戳，同一批量创建的环境前缀会完全相同
        // （实测出现过两窗口都显示 01M681DE 的情况），必须用尾部的随机熵来区分。
        &spec.env_id[spec.env_id.len().saturating_sub(8)..],
        spec.kernel_major,
        spec.country.clone().unwrap_or_else(|| "-".to_string()),
        if proxy_running { " · 代理已启用" } else { " · 直连（未找到 ziniao-proxy）" }
    );

    let target: reqwest::Url = open_url
        .parse()
        .map_err(|_| format!("目标 URL 非法: {open_url}"))?;

    let mut builder = tauri::WebviewWindowBuilder::new(
        &app,
        label.clone(),
        tauri::WebviewUrl::External(target.clone()),
    )
    .title(title.clone())
    .inner_size(1280.0, 860.0)
    .initialization_script(spec.script.clone())
    .user_agent(&spec.user_agent)
    .data_directory(profile_dir.clone());

    if proxy_running {
        let proxy: reqwest::Url = format!("http://127.0.0.1:{}", spec.proxy_port)
            .parse()
            .map_err(|e| format!("代理地址非法: {e}"))?;
        builder = builder.proxy_url(proxy);
    }

    builder
        .build()
        .map_err(|e| format!("创建环境窗口失败: {e}"))?;

    Ok(format!(
        "已打开环境窗口：{title}\n注入脚本 {} 字节 · 基线 {} · profile 目录 {}",
        spec.script_bytes,
        spec.baseline_hash,
        profile_dir.display()
    ))
}

/// ensure_proxy 拉起 Go 侧 uTLS 前置代理（sidecar）；找不到二进制则直连（不伪造 TLS）。
async fn ensure_proxy(app: &tauri::AppHandle, spec: &LaunchSpec) -> bool {
    let listen = format!("127.0.0.1:{}", spec.proxy_port);
    // 已在监听则复用
    if reqwest::Client::new()
        .get(format!("http://{listen}/healthz"))
        .send()
        .await
        .map(|r| r.status().is_success())
        .unwrap_or(false)
    {
        return true;
    }
    let exe = candidate_proxy_paths(app);
    let Some(path) = exe else { return false };
    // 上游代理凭据只经环境变量注入（不进 argv——Windows 下任意进程可读命令行；
    // 不落盘——由部署方/操作者以 ZINIAO_UPSTREAM_* 设置，随进程生命周期消失）
    let upstream_envs: Vec<(String, String)> = std::env::vars()
        .filter(|(k, _)| k.starts_with("ZINIAO_UPSTREAM_"))
        .collect();
    let _ = std::process::Command::new(path)
        .args([
            "--listen".to_string(),
            listen.clone(),
            "--kernel".to_string(),
            spec.kernel_major.to_string(),
        ])
        .envs(upstream_envs)
        .spawn();
    // 等待代理起来（uTLS 握手参数由 Go 侧按内核基线构造）
    for _ in 0..20 {
        std::thread::sleep(std::time::Duration::from_millis(150));
        if reqwest::Client::new()
            .get(format!("http://{listen}/healthz"))
            .send()
            .await
            .map(|r| r.status().is_success())
            .unwrap_or(false)
        {
            return true;
        }
    }
    false
}

/// candidate_proxy_paths 在与客户端同级目录 / PATH 里找 ziniao-proxy。
fn candidate_proxy_paths(app: &tauri::AppHandle) -> Option<PathBuf> {
    let mut names = vec!["ziniao-proxy".to_string(), "ziniao-proxy.exe".to_string()];
    if let Ok(cur) = std::env::current_exe() {
        if let Some(dir) = cur.parent() {
            // 1) 与客户端同级 2) 安装包默认位置 resources 下的 sidecar/ 目录
            names.push(dir.join("ziniao-proxy.exe").to_string_lossy().to_string());
            names.push(
                dir.join("sidecar")
                    .join("ziniao-proxy.exe")
                    .to_string_lossy()
                    .to_string(),
            );
            if let Some(parent) = dir.parent() {
                names.push(parent.join("ziniao-proxy.exe").to_string_lossy().to_string());
            }
        }
    }
    let _ = app;
    names
        .into_iter()
        .find(|p| which_exists(std::path::Path::new(p)))
        .map(PathBuf::from)
}

fn which_exists(p: &std::path::Path) -> bool {
    if p.is_absolute() || p.components().count() > 1 {
        return p.exists();
    }
    std::env::var_os("PATH")
        .map(|paths| {
            std::env::split_paths(&paths)
                .any(|dir| dir.join(p).exists() || dir.join(format!("{}.exe", p.display())).exists())
        })
        .unwrap_or(false)
}

fn urlencoding(s: &str) -> String {
    // 店铺 ID 为 ULID，仅做最小防御
    s.chars()
        .map(|c| match c {
            'a'..='z' | 'A'..='Z' | '0'..='9' | '-' | '_' => c.to_string(),
            _ => format!("%{:02X}", c as u32),
        })
        .collect()
}
