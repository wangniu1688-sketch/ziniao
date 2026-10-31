#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

//! 桌面客户端入口：登录服务端 → 列出店铺环境 → 按环境打开隔离浏览器窗口。
//! 窗口创建在 Rust 侧（`env_window`），前端只发启动请求。

fn main() {
    ziniao_client_lib::run()
}
