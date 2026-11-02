//! 多 webview 浏览器壳**原型**（探路用，不是交付代码路径）。
//!
//! 要回答的唯一问题：把网页从「窗口自带 webview」换成「挂在空窗口里的子 webview」之后，
//! 防关联的四条命脉还在不在——UA、注入脚本、profile 目录（data_directory）、以及
//! 「两个 webview 拼成一个窗口」的布局能不能跟着窗口 resize。
//!
//! 结果用 document.title 回传（不走 invoke）：原型不该为了一次性验证去动 ACL。
//! 报告落在 %TEMP%/ziniao-probe.log，跑完进程自己退出，便于脚本化验证。
//!
//! 已知前提（docs.rs tauri 2.12.5）：
//!   * WebviewBuilder 上 user_agent / initialization_script / data_directory / proxy_url 都在，
//!     所以子 webview **不会**丢掉致命能力——这是选这条路的唯一理由；
//!   * Window::add_child 与 Window::builder 需要 `unstable` feature，且仅 desktop；
//!   * Windows 上在**同步** command / 事件回调里建 webview 会死锁（WebView2），
//!     所以这里一律在 async 任务里建。

use std::fs::OpenOptions;
use std::io::Write;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use tauri::webview::PageLoadEvent;
use tauri::{LogicalPosition, LogicalSize, Manager, Rect, WebviewUrl};

/// 顶部工具条高度（逻辑像素）
const BAR_H: f64 = 48.0;
const W: f64 = 900.0;
const H: f64 = 640.0;

fn report_path() -> PathBuf {
    std::env::temp_dir().join("ziniao-probe.log")
}

fn record(path: &PathBuf, line: &str) {
    eprintln!("[probe] {line}");
    if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(path) {
        let _ = writeln!(f, "{line}");
    }
}

/// record_title 只认 PROBE: 开头的上报。
///
/// 页面没加载出来时 title 会是 "localhost"（tauri 错误页），若照单全收就会把
/// "页面 404" 误读成"能力验证通过"——这类误判比验证失败更贵。
fn record_title(path: &PathBuf, who: &str, title: &str) {
    if title.starts_with("PROBE:") {
        record(path, &format!("{who} OK {title}"));
    } else {
        record(path, &format!("{who} 异常（页面可能没加载）title={title}"));
    }
}

fn rect(x: f64, y: f64, w: f64, h: f64) -> Rect {
    Rect {
        position: LogicalPosition::new(x, y).into(),
        size: LogicalSize::new(w, h).into(),
    }
}

/// run 打开一个「空窗口 + 两个子 webview」的原型窗口并采集结果。
pub fn run(app: &tauri::AppHandle) -> Result<(), String> {
    let log = report_path();
    let _ = std::fs::remove_file(&log);
    record(&log, "start");

    // ---- 1. 空窗口（不带 webview；webview 全部由 add_child 挂上去） ----
    let window = tauri::Window::builder(app, "probe-win")
        .title("ziniao probe shell")
        .inner_size(W, H)
        .build()
        .map_err(|e| format!("建空窗口失败: {e}"))?;
    record(&log, "window ok（空窗口，无自带 webview）");

    // ---- 2. 顶部工具条：本地页面，占满宽度、固定 48px 高 ----
    let log_bar = log.clone();
    let log_bar_load = log.clone();
    let bar = window
        .add_child(
            tauri::webview::WebviewBuilder::new("probe-bar", WebviewUrl::App("probe.html".into()))
                .initialization_script("window.__ZINIAO_ROLE__ = 'bar';")
                .on_document_title_changed(move |_w, title| {
                    record_title(&log_bar, "bar", &title);
                })
                .on_page_load(move |_w, payload| {
                    let ev = match payload.event() {
                        PageLoadEvent::Started => "started",
                        PageLoadEvent::Finished => "finished",
                    };
                    record(&log_bar_load, &format!("load[bar] {ev} {}", payload.url()));
                }),
            LogicalPosition::new(0.0, 0.0),
            LogicalSize::new(W, BAR_H),
        )
        .map_err(|e| format!("挂顶部工具条 webview 失败: {e}"))?;
    record(&log, "bar webview ok");

    // ---- 3. 网页区：UA / 注入脚本 / profile 目录全挂在这个子 webview 上 ----
    let profile = std::env::temp_dir().join("ziniao-probe-profile");
    let _ = std::fs::create_dir_all(&profile);

    let log_page = log.clone();
    let log_page_load = log.clone();
    let log_finish = log.clone();
    let profile_finish = profile.clone();
    let bar_finish = bar.clone();
    let win_finish = window.clone();
    let app_finish = app.clone();
    let finished = Arc::new(AtomicBool::new(false));

    let page = window
        .add_child(
            tauri::webview::WebviewBuilder::new("probe-page", WebviewUrl::App("probe.html".into()))
            // 致命项 1：UA（子 webview 自己设的）
            .user_agent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 ZiniaoProbe/1.0")
            // 致命项 2：注入脚本必须在文档创建后、页面脚本前执行
            .initialization_script("window.__ZINIAO_ROLE__ = 'page'; window.__ZINIAO_PROBE__ = 'yes';")
            .data_directory(profile.clone())
            .on_page_load(move |_w, payload| {
                let ev = match payload.event() {
                    PageLoadEvent::Started => "started",
                    PageLoadEvent::Finished => "finished",
                };
                record(&log_page_load, &format!("load[page] {ev} {}", payload.url()));
            })
            .on_document_title_changed(move |_w, title| {
                record_title(&log_page, "page", &title);
                if finished.swap(true, Ordering::SeqCst) {
                    return; // 只收一次：title 可能被多次改写
                }
                // 回调是 Fn（会被多次调用），捕获的是借用——先克隆出 owned 值再交给线程
                let bar_finish = bar_finish.clone();
                let app_finish = app_finish.clone();
                let log_finish = log_finish.clone();
                let win_finish = win_finish.clone();
                let profile_finish = profile_finish.clone();
                // 拿到页面报告后顺带验证：子 webview 默认不跟随窗口 resize，
                // 必须能手动 set_bounds（否则浏览器壳一拉窗口就错位）。
                thread::spawn(move || {
                    let _ = bar_finish.set_bounds(rect(0.0, 0.0, W, BAR_H));
                    if let Some(p) = app_finish.get_webview("probe-page") {
                        let _ = p.set_bounds(rect(0.0, BAR_H, W, H - BAR_H));
                    }
                    record(&log_finish, "set_bounds ok");
                    // 触发一次真实 resize，看事件回调里的重排是否也工作
                    let _ = win_finish.set_size(LogicalSize::new(1000.0, 700.0));
                    thread::sleep(Duration::from_millis(900));
                    // profile 目录是否真的在用：WebView2 会在 data_directory 下建 EBWebView
                    let entries = std::fs::read_dir(&profile_finish)
                        .map(|rd| {
                            rd.filter_map(|e| e.ok().map(|e| e.file_name().to_string_lossy().to_string()))
                                .collect::<Vec<_>>()
                                .join(",")
                        })
                        .unwrap_or_else(|e| format!("读取失败: {e}"));
                    record(&log_finish, &format!("profile dir entries: {entries}"));
                    record(&log_finish, "DONE");
                    std::process::exit(0);
                });
            }),
            LogicalPosition::new(0.0, BAR_H),
            LogicalSize::new(W, H - BAR_H),
        )
        .map_err(|e| format!("挂网页 webview 失败: {e}"))?;
    record(&log, "page webview ok");

    // ---- 4. 窗口 resize：子 webview 不会自动跟随，必须自己重排 ----
    let log_resize = log.clone();
    let bar_resize = bar.clone();
    let app_resize = app.clone();
    let win_resize = window.clone();
    window.on_window_event(move |event| {
        if let tauri::WindowEvent::Resized(size) = event {
            let sf = win_resize.scale_factor().unwrap_or(1.0);
            let w = size.width as f64 / sf;
            let h = size.height as f64 / sf;
            let _ = bar_resize.set_bounds(rect(0.0, 0.0, w, BAR_H));
            if let Some(p) = app_resize.get_webview("probe-page") {
                let _ = p.set_bounds(rect(0.0, BAR_H, w, h - BAR_H));
            }
            record(
                &log_resize,
                &format!("resized {}x{} → 重排 ok", size.width, size.height),
            );
        }
    });

    let _ = page.url(); // 保活引用，避免编译器优化掉未使用变量告警
    Ok(())
}
