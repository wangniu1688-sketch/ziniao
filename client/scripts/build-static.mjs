// 构建静态前端：把 client 根目录的 index.html / src/main.js / inject/fingerprint.js
// 汇总到 dist/（无打包器依赖，保持零构建链复杂度）。
import { mkdirSync, copyFileSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";

// copyDir 递归复制目录（用于 @tauri-apps/api 的 ESM 文件树）。
function copyDir(from, to) {
  mkdirSync(to, { recursive: true });
  for (const name of readdirSync(from)) {
    const src = join(from, name);
    const dst = join(to, name);
    if (statSync(src).isDirectory()) copyDir(src, dst);
    else if (name.endsWith(".js")) copyFileSync(src, dst);
  }
}
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(root, "dist");
mkdirSync(dist, { recursive: true });

// dist/index.html 自带内联脚本（无外部依赖），直接复制即可
copyFileSync(join(root, "index.html"), join(dist, "index.html"));

// 前端 IPC：把 @tauri-apps/api 的原样 ESM 文件搬进 dist/api
const apiSrc = join(root, "node_modules", "@tauri-apps", "api");
const apiDst = join(dist, "api");
copyDir(apiSrc, apiDst);

mkdirSync(join(dist, "inject"), { recursive: true });
copyFileSync(join(root, "inject", "fingerprint.js"), join(dist, "inject", "fingerprint.js"));

console.log("dist/ 已生成");
