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
use std::process::Child;
use std::sync::{Mutex, OnceLock};

use tauri::webview::PageLoadEvent;
use tauri::{LogicalPosition, LogicalSize, Manager, Rect, WebviewUrl};

use serde::{Deserialize, Serialize};

/// 顶部工具条总高度（逻辑像素）：导航行 46 + 环境信息条 26，
/// 必须与 browser.html 里两行 CSS 高度严格一致（改一边必须改另一边）。
pub const BAR_H: f64 = 72.0;

/// 已打开的环境窗口：label → 运行信息（关窗口时要回传服务端）。
///
/// 为什么必须记这份表：服务端把环境状态写成 running 之后，只有"客户端主动上报关闭"
/// 这一条路径能把它收回来。不记的话，关掉的窗口在服务端永远是 running——
/// 运营看到一屏"正在运行"，其实一个窗口都没开，环境列表这条信息就作废了。
#[derive(Debug, Clone)]
pub struct OpenEnv {
    pub env_id: String,
    pub shop_id: String,
    pub base_url: String,
    pub token: String,
    pub proxy_port: i32,
}

fn open_envs() -> &'static Mutex<HashMap<String, OpenEnv>> {
    static M: OnceLock<Mutex<HashMap<String, OpenEnv>>> = OnceLock::new();
    M.get_or_init(|| Mutex::new(HashMap::new()))
}

/// 本进程拉起的 uTLS 前置代理：端口 → 子进程句柄。
///
/// 一个环境一个代理端口、一个进程；窗口关了不回收，端口与内存会一直挂着，
/// 而且下次启动会因为"端口已在监听"复用一个本该随环境结束的代理。
fn proxies() -> &'static Mutex<HashMap<u16, Child>> {
    static M: OnceLock<Mutex<HashMap<u16, Child>>> = OnceLock::new();
    M.get_or_init(|| Mutex::new(HashMap::new()))
}

fn register_open(label: String, info: OpenEnv) {
    if let Ok(mut m) = open_envs().lock() {
        m.insert(label, info);
    }
}

fn take_open(label: &str) -> Option<OpenEnv> {
    open_envs().lock().ok().and_then(|mut m| m.remove(label))
}

/// snapshot_open 当前仍登记为打开的环境（进程退出时逐个上报关闭）。
pub fn snapshot_open() -> Vec<OpenEnv> {
    open_envs()
        .lock()
        .map(|m| m.values().cloned().collect())
        .unwrap_or_default()
}

/// open_label_for_shop 本机是否还开着这个店铺的环境窗口（返回窗口 label）。
///
/// 停用/删除要先问这一句：服务端只能看到自己那份状态，看不到客户端本机
/// 还挂着一个窗口跟一个代理进程——那正是「已停用但线路仍在跑」的来源。
fn open_label_for_shop(shop_id: &str) -> Option<String> {
    open_envs().lock().ok().and_then(|m| {
        m.iter()
            .find(|(_, v)| v.shop_id == shop_id)
            .map(|(k, _)| k.clone())
    })
}

/// profiles_root 本机 profile 根目录（每个环境一个子目录，与 launch 时同一个口径）。
fn profiles_root(app: &tauri::AppHandle) -> PathBuf {
    app.path()
        .app_local_data_dir()
        .map(|d| d.join("profiles"))
        .unwrap_or_else(|_| PathBuf::from(".ziniao-profiles"))
}

/// is_safe_env_id 环境 ID 是否可安全地当成目录名使用。
///
/// 只放行「字母数字 + `-` + `_`」且长度受限：env_id 会拼进 profile 路径
/// （建目录、删目录），带 `..`、分隔符或引号的 id 会让这两处都跑到目录之外。
fn is_safe_env_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id != "."
        && id != ".."
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
}

/// profile_dir_for 由根目录 + env_id 算出该环境的 profile 目录；id 不安全返回 None。
fn profile_dir_for(root: &std::path::Path, env_id: &str) -> Option<PathBuf> {
    if !is_safe_env_id(env_id) {
        return None;
    }
    Some(root.join(env_id))
}

fn keep_proxy(port: u16, child: Child) {
    if let Ok(mut m) = proxies().lock() {
        m.insert(port, child);
    }
}

fn kill_proxy(port: u16) {
    if let Ok(mut m) = proxies().lock() {
        if let Some(mut child) = m.remove(&port) {
            let _ = child.kill();
            let _ = child.wait(); // 回收僵尸进程
        }
    }
}

fn port_u16(port: i32) -> u16 {
    port.clamp(0, 65535) as u16
}

/// StopEnvInput 前端传入：服务端地址、令牌、店铺。
#[derive(Debug, Deserialize)]
pub struct StopEnvInput {
    pub base_url: String,
    pub token: String,
    pub shop_id: String,
}

/// StopEnvOutput 关闭结果（前端据此刷新列表）。
#[derive(Debug, Serialize)]
pub struct StopEnvOutput {
    pub env_id: String,
    pub status: String,
    pub reported: bool,
}

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
    /// 本环境**专属**的出口代理凭据（服务端逐环境分配）。
    /// 为空表示服务端未启用出口 IP 池，此时回退进程级 ZINIAO_UPSTREAM_* 环境变量。
    #[serde(default)]
    upstream: Option<UpstreamCredential>,
    #[serde(default)]
    snapshot: Option<HashMap<String, String>>,
}

/// UpstreamCredential 与服务端 shop.UpstreamCredential 对齐。
/// 口令只在内存与本进程子进程的环境变量里流转，不落盘、不进 argv。
#[derive(Debug, Clone, Serialize, Deserialize)]
struct UpstreamCredential {
    #[serde(default)]
    protocol: String,
    #[serde(default)]
    host: String,
    #[serde(default)]
    port: i32,
    #[serde(default)]
    username: String,
    #[serde(default)]
    password: String,
    #[serde(default)]
    country: Option<String>,
    #[serde(default)]
    city: Option<String>,
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
    // env_id 会被当成 profile 目录名（也是删除时清理目录的唯一依据），形状异常一律拒绝启动：
    // 服务端是被信任的一方，但「信任」不等于「不做边界检查」——一个带 `../` 的 id
    // 会让建目录/删目录跑到应用数据目录之外，而这条路径没有任何其它地方会再看一眼。
    if !is_safe_env_id(&spec.env_id) {
        return Err(format!("启动单里的 env_id 形状异常（{:?}），拒绝启动", spec.env_id));
    }

    // ---- 2. 本地 uTLS 前置代理（有则启用，保证 TLS 指纹与内核基线同源） ----
    let proxy_running = ensure_proxy(&app, &spec).await;

    // ---- 3. 目录隔离：一个环境一个 profile 目录 ----
    let profile_dir = profiles_root(&app).join(&spec.env_id);
    let _ = std::fs::create_dir_all(&profile_dir);

    // ---- 4. 目标 URL：优先调用方指定 → 服务端指定 → 默认指纹实测页 ----
    let open_url = input
        .open_url
        .filter(|s| !s.is_empty())
        .or_else(|| Some(spec.open_url.clone()).filter(|s| !s.is_empty()))
        .unwrap_or_else(|| format!("{base}/fp-test"));

    // ---- 5. 开窗口（注入脚本在文档创建后、页面脚本前执行，时序不能晚） ----
    let label = format!("env-{}", spec.env_id.to_lowercase());
    let bar_label = format!("{label}-bar");
    let page_label = format!("{label}-page");
    if let Some(w) = app.get_window(&label) {
        // 同一环境已开着：**导航到本次目标页**并聚焦（重复启动 = 换目标页，
        // 而不是开出第二个不一致的窗口——同一环境永远只有一个窗口、一个 profile）
        let js = format!("location.replace({:?})", open_url);
        if let Some(p) = app.get_webview(&page_label).or_else(|| app.get_webview(&label)) {
            let _ = p.eval(&js);
        } else if let Some(p) = app.get_webview_window(&label) {
            let _ = p.eval(&js);
        }
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

    // ---- 5a. 浏览器壳：空窗口 + 顶部工具条（browser.html）+ 网页子 webview ----
    // unstable feature 下的实验 API（第 21 条原型实测可行）；任何一步失败都整体回退
    // 裸窗口形态，不让「启动环境」这个主功能跟着实验 API 陪葬。
    // 登记「已打开」放在建窗口之前：关闭上报的回调要靠这份登记取回凭据。
    register_open(
        label.clone(),
        OpenEnv {
            env_id: spec.env_id.clone(),
            shop_id: spec.shop_id.clone(),
            base_url: base.clone(),
            token: input.token.clone(),
            proxy_port: spec.proxy_port,
        },
    );

    match build_shell(
        &app,
        &label,
        &bar_label,
        &page_label,
        &title,
        &spec,
        &open_url,
        proxy_running,
        &profile_dir,
        target.clone(),
    ) {
        Ok((win, bar, page)) => {
            // 子 webview 默认**不**自动跟随窗口 resize，必须手动重排（第 21 条实测结论）；
            // 销毁时上报关闭 + 回收代理。
            let listening = label.clone();
            let bar_ev = bar.clone();
            let page_ev = page.clone();
            let win_ev = win.clone();
            win.on_window_event(move |event| match event {
                tauri::WindowEvent::Resized(size) => {
                    let sf = win_ev.scale_factor().unwrap_or(1.0);
                    let w = size.width as f64 / sf;
                    let h = size.height as f64 / sf;
                    let _ = bar_ev.set_bounds(rect(0.0, 0.0, w, BAR_H));
                    let _ = page_ev.set_bounds(rect(0.0, BAR_H, w, h - BAR_H));
                }
                tauri::WindowEvent::Destroyed => on_destroyed(&listening),
                _ => {}
            });
        }
        Err(e) => {
            eprintln!("[ziniao] 浏览器壳创建失败，回退裸窗口: {e}");
            // 半成品窗口（可能只挂上了工具条）一起收掉，释放 label 供裸窗口复用
            if let Some(w) = app.get_window(&label) {
                let _ = w.close();
            }
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
            let win = builder
                .build()
                .map_err(|e| format!("创建环境窗口失败: {e}"))?;
            let listening = label.clone();
            win.on_window_event(move |event| {
                if matches!(event, tauri::WindowEvent::Destroyed) {
                    on_destroyed(&listening);
                }
            });
        }
    }

    Ok(format!(
        "已打开环境窗口：{title}\n注入脚本 {} 字节 · 基线 {} · profile 目录 {}",
        spec.script_bytes,
        spec.baseline_hash,
        profile_dir.display()
    ))
}

/// build_shell 组装「浏览器壳」：空窗口 + 顶部工具条 webview（browser.html）+ 网页 webview。
///
/// 分工与第 21 条原型实测一致：防关联四件套（UA / 注入脚本 / profile 目录 / 代理）
/// 全部挂在**网页 webview** 上；工具条只是本应用页面，经 invoke(env_nav) 让 Rust 侧
/// 驱动网页 webview。任一步失败返回 Err，由调用方整体回退裸窗口形态。
#[allow(clippy::too_many_arguments)]
fn build_shell(
    app: &tauri::AppHandle,
    label: &str,
    bar_label: &str,
    page_label: &str,
    title: &str,
    spec: &LaunchSpec,
    open_url: &str,
    proxy_running: bool,
    profile_dir: &std::path::Path,
    target: reqwest::Url,
) -> Result<
    (
        tauri::Window<tauri::Wry>,
        tauri::Webview<tauri::Wry>,
        tauri::Webview<tauri::Wry>,
    ),
    String,
> {
    // 空窗口：webview 全部由 add_child 挂上去（unstable feature，仅 desktop）
    let window = tauri::Window::builder(app, label)
        .title(title)
        .inner_size(1280.0, 860.0)
        .build()
        .map_err(|e| format!("创建壳窗口失败: {e}"))?;

    // 工具条：本应用页面，环境信息经注入脚本带入（WebviewUrl::App 不能带 query，第 21 条的坑）
    let env_info = serde_json::json!({
        "window": label,
        "url": open_url,
        "shop": spec.shop_name.clone().unwrap_or_else(|| spec.shop_id.clone()),
        "env": &spec.env_id[spec.env_id.len().saturating_sub(8)..],
        "kernel": spec.kernel_major,
        "country": spec.country.clone().unwrap_or_else(|| "-".to_string()),
        "proxy": if proxy_running {
            format!("代理 127.0.0.1:{}", spec.proxy_port)
        } else {
            "直连（未找到 ziniao-proxy）".to_string()
        },
    })
    .to_string();
    let bar = window
        .add_child(
            tauri::webview::WebviewBuilder::new(bar_label, WebviewUrl::App("browser.html".into()))
                .initialization_script(format!("window.__ZINIAO_ENV__ = {env_info};")),
            LogicalPosition::new(0.0, 0.0),
            LogicalSize::new(1280.0, BAR_H),
        )
        .map_err(|e| format!("挂工具条 webview 失败: {e}"))?;

    // 网页区：UA / 注入脚本 / profile 目录 / 代理，与裸窗口形态一件不缺
    let bar_for_nav = bar.clone();
    let mut page_builder = tauri::webview::WebviewBuilder::new(page_label, WebviewUrl::External(target))
        .initialization_script(spec.script.clone())
        .user_agent(&spec.user_agent)
        .data_directory(profile_dir.to_path_buf())
        // 页面每发生一次导航，把地址回写给工具条（地址栏的最基本职责）。
        // 返回 false 会拦下这次导航——这里只旁观，不拦截。
        .on_navigation(move |url| {
            let js = format!("window.__ZINIAO_SET_URL__({:?})", url.as_str());
            let _ = bar_for_nav.eval(&js);
            true
        })
        .on_page_load(move |w, payload| {
            if matches!(payload.event(), PageLoadEvent::Finished) {
                let js = format!("window.__ZINIAO_SET_URL__({:?})", payload.url());
                let _ = w.eval(&js);
            }
        });
    if proxy_running {
        let proxy: reqwest::Url = format!("http://127.0.0.1:{}", spec.proxy_port)
            .parse()
            .map_err(|e| format!("代理地址非法: {e}"))?;
        page_builder = page_builder.proxy_url(proxy);
    }
    let page = window
        .add_child(
            page_builder,
            LogicalPosition::new(0.0, BAR_H),
            LogicalSize::new(1280.0, 860.0 - BAR_H),
        )
        .map_err(|e| format!("挂网页 webview 失败: {e}"))?;

    Ok((window, bar, page))
}

/// on_destroyed：窗口销毁后上报关闭 + 回收代理。
///
/// 上报不到也没关系（窗口已经关了是既成事实），服务端还有巡检兜底 env.reclaim；
/// 但不做这一步，服务端就会永远显示 running：环境列表等于不可信。
fn on_destroyed(label: &str) {
    if let Some(info) = take_open(label) {
        tauri::async_runtime::spawn(async move {
            let _ = report_stop(&info).await;
            kill_proxy(port_u16(info.proxy_port));
        });
    }
}

fn rect(x: f64, y: f64, w: f64, h: f64) -> Rect {
    Rect {
        position: LogicalPosition::new(x, y).into(),
        size: LogicalSize::new(w, h).into(),
    }
}

/// EnvNavInput 环境窗口工具条（browser.html）传入：目标窗口 label + 动作/地址。
#[derive(Debug, Deserialize)]
pub struct EnvNavInput {
    pub window: String,
    /// back | forward | reload | go（go = 按 url 导航）
    #[serde(default)]
    pub action: String,
    #[serde(default)]
    pub url: String,
}

/// env_nav 环境窗口工具条的导航命令（lib.rs 的 command 入口）。
///
/// 只操作**该窗口自己的网页 webview**（label 前缀强校验）——地址栏的输入
/// 不可能导航到别的环境的 profile 上。
pub fn env_nav(app: tauri::AppHandle, input: EnvNavInput) -> Result<(), String> {
    if !input.window.starts_with("env-") {
        return Err("非法的窗口标识".to_string());
    }
    let page_label = format!("{}-page", input.window);
    let page = app
        .get_webview(&page_label)
        .or_else(|| app.get_webview(&input.window))
        .ok_or_else(|| "该环境的网页区已不存在".to_string())?;
    match input.action.as_str() {
        "back" => page.eval("history.back()").map_err(|e| e.to_string()),
        "forward" => page.eval("history.forward()").map_err(|e| e.to_string()),
        "reload" => page.eval("location.reload()").map_err(|e| e.to_string()),
        _ => {
            // navigate() 会重跑 initialization_script——指纹脚本在地址栏导航后仍然生效
            // （docs/TODO-phase2.md 第 21a 条核对过这条 API 语义），比 eval location.replace 更可靠
            let url = tauri::Url::parse(&normalize_url(&input.url)?)
                .map_err(|e| format!("URL 非法: {e}"))?;
            page.navigate(url).map_err(|e| format!("导航失败: {e}"))
        }
    }
}

/// normalize_url 地址栏输入缺 scheme 时按 https 补全（浏览器地址栏的惯例）。
fn normalize_url(raw: &str) -> Result<String, String> {
    let s = raw.trim();
    if s.is_empty() {
        return Err("地址为空".to_string());
    }
    if s.contains("://") {
        Ok(s.to_string())
    } else {
        Ok(format!("https://{s}"))
    }
}

/// close_env_window 关掉环境窗口（兼容壳窗口与回退的裸窗口两种形态）。
fn close_env_window(app: &tauri::AppHandle, label: &str) {
    if let Some(w) = app.get_window(label) {
        let _ = w.close();
    } else if let Some(w) = app.get_webview_window(label) {
        let _ = w.close();
    }
}

/// stop_env 关闭一个店铺的环境窗口（前端「关闭环境」按钮）。
///
/// 顺序不能反：先让服务端把状态收回 idle，再关窗口、再回收代理。
/// 先关窗口的话，Destroyed 事件里那次上报会抢先执行，这里再报一次也只是幂等——
/// 服务端对"已经不是 running"的情况直接原样返回，不会写重复审计。
pub async fn stop_env(app: tauri::AppHandle, input: StopEnvInput) -> Result<StopEnvOutput, String> {
    let info = match open_label_for_shop(&input.shop_id) {
        Some(l) => take_open(&l).ok_or_else(|| "环境窗口已不在本客户端".to_string())?,
        // 窗口不在本客户端（例如服务端状态卡住了）：仍然上报一次，服务端幂等。
        None => OpenEnv {
            env_id: String::new(),
            shop_id: input.shop_id.clone(),
            base_url: input.base_url.clone(),
            token: input.token.clone(),
            proxy_port: 0,
        },
    };
    let reported = match report_stop(&info).await {
        Ok(_) => true,
        // 关不掉也要继续关窗口：窗口已经关了是既成事实，
        // 上报失败只是让服务端状态晚一点更新（还有巡检兜底）。
        Err(e) => {
            eprintln!("[ziniao] 关闭上报失败（窗口仍会关闭）: {e}");
            false
        }
    };
    kill_proxy(port_u16(info.proxy_port));
    let key = format!("env-{}", info.env_id.to_lowercase());
    close_env_window(&app, &key);
    Ok(StopEnvOutput {
        env_id: info.env_id,
        status: "idle".to_string(),
        reported,
    })
}

/// env_endpoint 拼环境相关接口的 URL（suffix 形如 "" 或 "/deactivate"）。
fn env_endpoint(input: &StopEnvInput, suffix: &str) -> String {
    format!(
        "{}/api/v1/shops/{}/environment{}",
        input.base_url.trim_end_matches('/'),
        urlencoding(&input.shop_id),
        suffix
    )
}

/// EnvRow 环境行的最小投影：删除流程只用到 id（清 profile 的依据）与 status。
#[derive(Debug, Deserialize)]
struct EnvRow {
    id: String,
    #[serde(default)]
    status: String,
}

/// fetch_env 读一次环境行（服务端裁决，客户端不猜）。
async fn fetch_env(input: &StopEnvInput) -> Result<EnvRow, String> {
    let resp = reqwest::Client::builder()
        .build()
        .map_err(|e| format!("构造 HTTP 客户端失败: {e}"))?
        .get(env_endpoint(input, ""))
        .bearer_auth(&input.token)
        .send()
        .await
        .map_err(|e| format!("无法连接服务端: {e}"))?;
    let status = resp.status();
    if status == reqwest::StatusCode::NOT_FOUND {
        return Err("该店铺还没有环境".to_string());
    }
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("查询环境失败（HTTP {status}）：{body}"));
    }
    resp.json::<EnvRow>()
        .await
        .map_err(|e| format!("环境行解析失败: {e}"))
}

/// post_lifecycle 调用停用/启用这类「无入参、返回环境行」的接口，回传新状态。
async fn post_lifecycle(input: &StopEnvInput, action: &str) -> Result<String, String> {
    let resp = reqwest::Client::builder()
        .build()
        .map_err(|e| format!("构造 HTTP 客户端失败: {e}"))?
        .post(env_endpoint(input, &format!("/{action}")))
        .bearer_auth(&input.token)
        .json(&serde_json::json!({ "url": "/client" }))
        .send()
        .await
        .map_err(|e| format!("无法连接服务端: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("服务端拒绝了这次操作（HTTP {status}）：{body}"));
    }
    let row: EnvRow = resp
        .json()
        .await
        .map_err(|e| format!("环境行解析失败: {e}"))?;
    Ok(row.status)
}

/// deactivate_env 停用环境（退出服务 + 服务端归还出口绑定）。
///
/// 为什么客户端要先自己拦一道：服务端只按 `status == running` 判 409，而客户端本机
/// 还挂着窗口这件事服务端看不见（例如状态已被巡检收回 idle）。**窗口在本机还开着**
/// 就停用，会留下一个没人管的窗口 + 一个继续跑的前置代理进程，而环境已经被标成
/// 「退出服务」——正是「已停用但线路仍在跑」的来源。
pub async fn deactivate_env(app: tauri::AppHandle, input: StopEnvInput) -> Result<String, String> {
    if let Some(label) = open_label_for_shop(&input.shop_id) {
        return Err(format!(
            "本机还开着该店铺的环境窗口（{label}），请先关闭窗口再停用"
        ));
    }
    let status = post_lifecycle(&input, "deactivate").await?;
    let _ = app; // 停用不动窗口：只是为了让签名与其它生命周期入口一致
    Ok(status)
}

/// DeleteEnvOutput 删除结果：profile 清理是尽力而为，但结果必须如实回传给运营。
#[derive(Debug, Serialize)]
pub struct DeleteEnvOutput {
    pub env_id: String,
    pub profile_purged: bool,
    pub note: String,
}

/// delete_env 删除环境（终态）：先收本机窗口 → 服务端删行 → 清本机 profile。
///
/// 为什么不能只调 DELETE 接口了事：profile 目录在**客户端本机**
/// （`profiles/<env_id>`，一个 WebView2 完整数据目录），服务端删不到。
/// 服务端在 `env.deleted` 事件里带了 env_id 就是留给客户端做这件事的；
/// 没人清的话，删一个环境就在本机留下几十 MB 且永远不回收的目录，只增不减。
///
/// 顺序：先关窗口（running 时服务端会 409 拒删，而窗口在本机）、
/// 再删服务端（裁决在那里）、最后清目录（删成功之后才清——先清目录会变成
/// 「本机数据没了，服务端还挂着一个环境」，两边对不上）。
pub async fn delete_env(
    app: tauri::AppHandle,
    input: StopEnvInput,
) -> Result<DeleteEnvOutput, String> {
    let closed_local = open_label_for_shop(&input.shop_id).is_some();
    if closed_local {
        // 关窗口这条路径本身会上报 stop（running → idle），删之前必须走完它。
        let _ = stop_env(app.clone(), StopEnvInput {
            base_url: input.base_url.clone(),
            token: input.token.clone(),
            shop_id: input.shop_id.clone(),
        })
        .await;
    }
    // env_id 只从服务端取：它就是 profile 目录名。让调用方传的话，
    // 等于把「按任意字符串删本机目录」的能力开放给 UI 层。
    let env = fetch_env(&input).await?;
    let resp = reqwest::Client::builder()
        .build()
        .map_err(|e| format!("构造 HTTP 客户端失败: {e}"))?
        .delete(env_endpoint(&input, ""))
        .bearer_auth(&input.token)
        .send()
        .await
        .map_err(|e| format!("无法连接服务端: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("服务端拒绝了删除（HTTP {status}）：{body}"));
    }
    let (profile_purged, note) = purge_profile(&app, &env.id);
    Ok(DeleteEnvOutput {
        env_id: env.id,
        profile_purged,
        note: if closed_local {
            format!("已先关闭本机窗口；{note}")
        } else {
            note
        },
    })
}

/// purge_profile 清理本机 profile 目录（尽力而为：失败只留提示，不阻塞已完成的删除）。
///
/// 两道防线：env_id 形状校验（不得含分隔符/上跳）+ canonicalize 后必须仍在 profiles
/// 根目录下——`remove_dir_all` 是不可逆操作，路径上任何一处"想当然"都代价极大。
fn purge_profile(app: &tauri::AppHandle, env_id: &str) -> (bool, String) {
    let root = profiles_root(app);
    let Some(dir) = profile_dir_for(&root, env_id) else {
        return (
            false,
            format!("环境 ID 形状异常（{env_id:?}），未清理本机目录，请人工确认"),
        );
    };
    if !dir.exists() {
        return (true, "本机没有该环境的 profile 目录".to_string());
    }
    if let (Ok(root_real), Ok(dir_real)) = (root.canonicalize(), dir.canonicalize()) {
        if !dir_real.starts_with(&root_real) {
            return (
                false,
                format!("profile 路径不在数据目录内，已跳过清理：{}", dir_real.display()),
            );
        }
    }
    // WebView2 关闭后目录句柄可能晚几百毫秒才释放，第一次删常常失败；
    // 不重试的话表现就是「环境删了、本机目录还在」，而且没人会注意到。
    let mut last = String::new();
    for attempt in 0..5 {
        match std::fs::remove_dir_all(&dir) {
            Ok(_) => return (true, format!("已清理本机 profile（{}）", dir.display())),
            Err(e) => {
                last = e.to_string();
                if attempt < 4 {
                    std::thread::sleep(std::time::Duration::from_millis(250));
                }
            }
        }
    }
    (
        false,
        format!(
            "本机 profile 清理失败（可手动删除 {}）：{last}",
            dir.display()
        ),
    )
}

/// stop_all_open 进程退出前把仍打开的环境逐个上报关闭。
///
/// 这是**尽力而为**：退出时异步任务未必跑得完（断电/强杀更是一点机会都没有），
/// 所以服务端还有巡检兜底（env.reclaim，默认 12 小时）。
pub async fn stop_all_open(app: tauri::AppHandle) {
    for info in snapshot_open() {
        let _ = report_stop(&info).await;
        kill_proxy(port_u16(info.proxy_port));
        let key = format!("env-{}", info.env_id.to_lowercase());
        close_env_window(&app, &key);
        let _ = take_open(&key);
    }
}

/// report_stop 上报「环境已关闭」。
async fn report_stop(info: &OpenEnv) -> Result<String, String> {
    let base = info.base_url.trim_end_matches('/').to_string();
    let url = format!(
        "{}/api/v1/shops/{}/environment/stop",
        base,
        urlencoding(&info.shop_id)
    );
    let resp = reqwest::Client::builder()
        .build()
        .map_err(|e| format!("构造 HTTP 客户端失败: {e}"))?
        .post(&url)
        .bearer_auth(&info.token)
        .json(&serde_json::json!({}))
        .send()
        .await
        .map_err(|e| format!("无法连接服务端: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("服务端拒绝了关闭上报（HTTP {status}）：{body}"));
    }
    Ok("已上报关闭".to_string())
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
    // 出口 IP 凭据来源：**优先用启动单里本环境专属的那一份**。
    // 这是多账号防关联的命脉——同一客户端上所有窗口若共用一组进程级环境变量，
    // 就等于所有店铺从同一个出口 IP 出网，平台据此可直接把账号关联起来。
    let upstream_envs: Vec<(String, String)> = match &spec.upstream {
        Some(u) if !u.host.is_empty() && u.port > 0 => vec![
            ("ZINIAO_UPSTREAM_HOST".to_string(), u.host.clone()),
            ("ZINIAO_UPSTREAM_PORT".to_string(), u.port.to_string()),
            ("ZINIAO_UPSTREAM_USER".to_string(), u.username.clone()),
            ("ZINIAO_UPSTREAM_PASS".to_string(), u.password.clone()),
            ("ZINIAO_UPSTREAM_PROTO".to_string(), u.protocol.clone()),
        ],
        // 回退：由部署方设置的进程级环境变量（此时所有环境共用一个出口，仅本地自测可接受）
        _ => std::env::vars()
            .filter(|(k, _)| k.starts_with("ZINIAO_UPSTREAM_"))
            .collect(),
    };
    // 句柄必须留着：窗口关闭时要按端口把这个代理进程收掉，
    // 否则一个环境一个代理进程会越积越多（端口与内存都挂着）。
    let spawned = std::process::Command::new(path)
        .args([
            "--listen".to_string(),
            listen.clone(),
            "--kernel".to_string(),
            spec.kernel_major.to_string(),
        ])
        .envs(upstream_envs)
        // 该环境声称的 UA：sidecar 用它改写请求头里的 sec-ch-ua。
        // 浏览器在发出请求时会自动带上真实内核版本的 UA-CH（本机 WebView2 是 156），
        // JS 改不了这一层，只能由代理在明文处对齐，否则 UA 与请求头自相矛盾。
        .env("ZINIAO_ENV_UA", &spec.user_agent)
        .spawn();
    match spawned {
        Ok(child) => keep_proxy(port_u16(spec.proxy_port), child),
        Err(e) => {
            eprintln!("[ziniao] 启动前置代理失败: {e}");
            return false;
        }
    }
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn env_id_must_be_a_safe_directory_name() {
        // 服务端真实 env_id 是 ULID：26 位 Crockford base32
        assert!(is_safe_env_id("01J8ZK3M9F4CNDDZ7QWERTYVBN"));
        assert!(is_safe_env_id("env-1_A"));
        let long = "x".repeat(65);
        let bad: Vec<&str> = vec!["", ".", "..", "../x", "a/b", "a\\b", "a b", "a\tb", &long];
        for b in bad {
            assert!(!is_safe_env_id(b), "应拒绝 {b:?}");
        }
    }

    #[test]
    fn profile_dir_never_escapes_root() {
        let root = std::path::Path::new("C:/data/profiles");
        assert_eq!(profile_dir_for(root, "01ABC"), Some(root.join("01ABC")));
        // 不可逆操作（remove_dir_all）的入参不能是「拼出来看着像」的东西
        assert_eq!(profile_dir_for(root, "../../etc"), None);
        assert_eq!(profile_dir_for(root, ""), None);
    }

    #[test]
    fn address_bar_completes_scheme() {
        assert_eq!(normalize_url(" example.com ").unwrap(), "https://example.com");
        assert_eq!(normalize_url("http://a.b/c").unwrap(), "http://a.b/c");
        assert!(normalize_url("   ").is_err());
    }
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
