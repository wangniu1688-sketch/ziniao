/* 紫鸟浏览器 · 启动器（vanilla JS，无构建依赖）
   M2：店铺管理 / 代理管理 / 分组管理 / 设备与网络 / 日志中心 五个页面接入
   后端聚合接口；工作台接入 system/stats 的环境口径与最近环境列表；
   系统设置展示 system/info 只读信息。
   权限仍由服务端裁决：前端只决定「画不画」，不决定「能不能」——
   403 由 api() 抛成错误，页面统一用 toast 呈现。 */
"use strict";

const state = { token: localStorage.getItem("zn_token") || "", me: null, info: null };

// 导航清单由后端 /api/v1/me 的 nav[] 下发（单一事实源：iam.NavSections）。
// 这里只保留无权限时的兜底顺序；visible=false 的项一律不渲染。
const SECTIONS = [
  { id: "dashboard", title: "工作台" },
  { id: "shops",     title: "店铺管理" },
  { id: "proxies",   title: "代理管理" },
  { id: "groups",    title: "分组管理" },
  { id: "devices",   title: "设备与网络" },
  { id: "logs",      title: "日志中心" },
  { id: "settings",  title: "系统设置" },
];

// visibleSections 当前用户可见的导航项：资源码被拒的项从导航栏消失。
function visibleSections() {
  const nav = state.me && Array.isArray(state.me.nav) ? state.me.nav : null;
  if (!nav || !nav.length) return SECTIONS;
  return nav.filter((n) => n.visible)
            .map((n) => SECTIONS.find((s) => s.id === n.id) || { id: n.id, title: n.title });
}

const ENV_STATUS = { created: "已创建", running: "运行中", idle: "空闲", stopped: "已停用" };
const EXIT_MODE = { manual: "手工代理", pool: "代理商池", direct: "直连" };
const PROXY_TYPE = {
  isp: "ISP", static_residential: "静态住宅", residential: "住宅", datacenter: "机房",
};
// 拨测状态徽章：unknown（没测过）与 fail（测过但不通）必须长得不一样，
// 运营要做的动作完全不同——先点拨测 vs 改地址/口令。
// proxyCell 环境行的出口展示：直连必须明说，绑了代理就把最近一次实测的
// 出口 IP/国家带出来（并标注它是「最近实测」而不是「此刻」）。
function proxyCell(e) {
  if (!e.proxy_id || e.exit_mode === "direct") {
    return '<span class="badge">直连</span>';
  }
  const ip = e.proxy_exit_ip ? `<div class="small muted mono">${esc(e.proxy_exit_ip)}${e.proxy_exit_country ? " · " + esc(e.proxy_exit_country) : ""}</div>` : "";
  return `${esc(e.proxy_name || "已绑定")}${ip}`;
}
function checkBadge(p) {
  const s = p.check_status || "unknown";
  if (s === "ok") return '<span class="badge ok">通过</span>';
  if (s === "fail") return `<span class="badge err" title="${esc(p.last_error || "")}">失败</span>`;
  return '<span class="badge warn">未拨测</span>';
}
const PLATFORMS = ["Amazon", "Shopee", "Lazada", "TikTok", "Temu", "eBay", "Walmart", "AliExpress", "其他"];

/* ---------- 基础 ---------- */
async function api(method, path, body, raw) {
  const opt = { method, headers: {} };
  if (state.token) opt.headers.Authorization = "Bearer " + state.token;
  if (body !== undefined) { opt.headers["Content-Type"] = "application/json"; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (res.status === 401) { logout(); throw new Error("登录已过期"); }
  if (!res.ok && !raw) throw new Error((data && (data.message || data.code)) || text || ("HTTP " + res.status));
  return { status: res.status, body: data, text };
}
const GET = (p) => api("GET", p).then((r) => r.body);
const POST = (p, b) => api("POST", p, b).then((r) => r.body);
const PUT = (p, b) => api("PUT", p, b).then((r) => r.body);
const DEL = (p) => api("DELETE", p).then((r) => r.body);

function toast(msg, isErr) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.className = "toast" + (isErr ? " err" : "");
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add("hidden"), 4200);
}
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const fmt = (t) => (t ? new Date(t).toLocaleString("zh-CN", { hour12: false }) : "-");
const yn = (b) => (b ? '<span class="badge ok">是</span>' : '<span class="badge warn">否</span>');
const statusBadge = (s) => {
  const cls = s === "running" ? "ok" : (s === "stopped" ? "err" : "");
  return `<span class="badge ${cls}">${esc(ENV_STATUS[s] || s || "-")}</span>`;
};

/* ---------- M3 启停状态联动 ----------
 * 口径：数据库状态（statusBadge）与本机进程态（local-runtime）合并显示，
 * 一切以接口返回为准，前端不做乐观更新。 */

// runBadge 运行中（绿点）/ 离线（灰点）。进程态不可用时退化为纯库状态。
const runBadge = (e, rt) => {
  if (rt) return `<span class="badge ok"><i class="dot live"></i>运行中</span>`;
  if (e.status === "running") {
    return `<span class="badge warn" title="库里是 running，但本机没有进程（可能已崩溃，重启启动器会自动复位）">
      <i class="dot"></i>状态待核对</span>`;
  }
  return `<span class="badge"><i class="dot"></i>离线</span>`;
};

// runtimeCell PID / CDP 端口 / 本地代理端口 / 已运行时长。
const runtimeCell = (rt) => {
  if (!rt) return `<span class="muted small">—</span>`;
  return `<div class="mono small">PID ${rt.pid} · CDP ${rt.cdp_port} · 代理 ${rt.local_proxy_port}</div>
    <div class="small muted">${esc(EXIT_MODE[rt.exit_mode] || rt.exit_mode || "-")} · 已运行 ${dur(rt.uptime_seconds)}</div>`;
};

const dur = (sec) => {
  const s = Math.max(0, Math.floor(Number(sec) || 0));
  if (s < 60) return s + " 秒";
  if (s < 3600) return Math.floor(s / 60) + " 分 " + (s % 60) + " 秒";
  return Math.floor(s / 3600) + " 时 " + Math.floor((s % 3600) / 60) + " 分";
};

// startDisabled 启动按钮的禁用条件：**已在本机运行**时禁用并给出提示。
// 停用的环境也不给启动（服务端同样会拒，这里是提前把话说清楚）。
const startDisabled = (e, rt, processState) => {
  if (rt) return 'disabled title="环境已在本机运行，请先停止"';
  if (e.status === "stopped") return 'disabled title="环境已停用，请先启用"';
  if (processState !== "available") return 'disabled title="启动器内核不可用（未装配或只读实例）"';
  return "";
};

/* ---------- 弹窗 ---------- */
/* fields: {key,label,type:text|number|password|url|select,options:[{v,t}],value,placeholder,required,note} */
function openModal(title, fields, submitText, onSubmit) {
  const mask = document.createElement("div");
  mask.className = "mask";
  const html = fields.map((f) => {
    const req = f.required ? " required" : "";
    const ph = f.placeholder ? ` placeholder="${esc(f.placeholder)}"` : "";
    let input;
    if (f.type === "select") {
      input = `<select id="f_${f.key}"${req}>${(f.options || []).map((o) =>
        `<option value="${esc(o.v)}"${String(o.v) === String(f.value ?? "") ? " selected" : ""}>${esc(o.t)}</option>`).join("")}</select>`;
    } else {
      input = `<input id="f_${f.key}" type="${f.type || "text"}" value="${esc(f.value ?? "")}"${ph}${req} />`;
    }
    return `<div><label for="f_${f.key}">${esc(f.label)}</label>${input}` +
      (f.note ? `<div class="note">${esc(f.note)}</div>` : "") + `</div>`;
  }).join("");
  mask.innerHTML = `<form class="modal"><h3>${esc(title)}</h3>${html}
    <div class="modal-actions">
      <button type="button" data-act="cancel">取消</button>
      <button type="submit" class="primary-sm" data-act="ok">${esc(submitText)}</button>
    </div></form>`;
  document.body.appendChild(mask);
  const close = () => mask.remove();
  const form = mask.querySelector("form");
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const values = {};
    for (const f of fields) {
      const el = form.querySelector("#f_" + f.key);
      values[f.key] = el ? String(el.value).trim() : "";
    }
    const btn = form.querySelector('[data-act="ok"]');
    btn.disabled = true;
    try { await onSubmit(values); close(); } catch (e) { toast("操作失败：" + e.message, true); btn.disabled = false; }
  });
  form.querySelector('[data-act="cancel"]').onclick = close;
  mask.addEventListener("click", (ev) => { if (ev.target === mask) close(); });
}

/* ---------- 登录 ---------- */
async function doLogin(ev) {
  ev.preventDefault();
  const errEl = document.getElementById("login-err");
  errEl.textContent = "";
  try {
    const r = await api("POST", "/api/v1/auth/login", {
      username: document.getElementById("login-user").value.trim(),
      password: document.getElementById("login-pass").value,
    }, true);
    if (!r.body || !r.body.token) throw new Error((r.body && r.body.message) || "登录失败");
    state.token = r.body.token;
    localStorage.setItem("zn_token", state.token);
    await boot();
  } catch (e) { errEl.textContent = "登录失败：" + e.message; }
  return false;
}
function logout() {
  state.token = "";
  state.me = null;
  state.info = null;
  localStorage.removeItem("zn_token");
  show("login-view");
}

async function boot() {
  if (!state.token) { show("login-view"); return; }
  try { state.me = await GET("/api/v1/me"); } catch { return; }
  show("app-view");
  document.getElementById("whoami").textContent = `${state.me.username || ""} · ${state.me.tenant_id || ""}`;
  renderNav();
  go(location.hash.replace("#/", "") || "dashboard");
}
function show(id) {
  document.getElementById("login-view").classList.toggle("hidden", id !== "login-view");
  document.getElementById("app-view").classList.toggle("hidden", id !== "app-view");
}

/* ---------- 导航 ---------- */
function renderNav() {
  const list = visibleSections();
  document.getElementById("nav").innerHTML = list.length
    ? list.map((s) => `<a href="#/${s.id}" data-id="${s.id}">${esc(s.title)}</a>`).join("")
    : `<p class="muted small" style="padding:9px 12px">无可访问模块</p>`;
}
async function go(id) {
  const list = visibleSections();
  const sec = list.find((s) => s.id === id.split("?")[0]) || list[0];
  if (!sec) {
    document.getElementById("section-title").textContent = "无访问权限";
    document.getElementById("content").innerHTML =
      `<div class="card hint">当前账号没有任何模块权限，请联系管理员分配角色。</div>`;
    return;
  }
  document.querySelectorAll("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.id === sec.id));
  document.getElementById("section-title").textContent = sec.title;
  document.getElementById("section-note").textContent = "";
  const c = document.getElementById("content");
  c.innerHTML = `<p class="muted">加载中…</p>`;
  try { await RENDERERS[sec.id](c); }
  catch (e) { c.innerHTML = `<div class="card hint err">加载失败：${esc(e.message)}</div>`; }
}
window.addEventListener("hashchange", () => go(location.hash.replace("#/", "")));

/* ---------- 工作台 ---------- */
async function systemInfo() {
  if (!state.info) state.info = await GET("/api/v1/system/info");
  return state.info;
}

async function renderDashboard(c) {
  const [stats, info] = await Promise.all([GET("/api/v1/system/stats"), systemInfo()]);
  const recent = stats.recent || [];
  c.innerHTML = `
    <div class="grid">
      <div class="stat"><b>${stats.environment_total ?? 0}</b><span class="muted">环境总数</span></div>
      <div class="stat"><b>${stats.running ?? 0}</b><span class="muted">运行中</span></div>
      <div class="stat"><b>${stats.offline ?? 0}</b><span class="muted">未运行</span></div>
      <div class="stat"><b>${stats.shops?.total ?? 0}</b><span class="muted">店铺总数</span></div>
      <div class="stat"><b>${stats.proxies?.total ?? 0}</b><span class="muted">代理（启用 ${stats.proxies?.active ?? 0}）</span></div>
      <div class="stat"><b>${stats.groups?.total ?? 0}</b><span class="muted">分组</span></div>
    </div>
    ${guidanceCards(info)}
    <div class="card">
      <h3>最近环境</h3>
      ${recent.length ? `<table>
        <tr><th>店铺</th><th>状态</th><th>国家</th><th>内核</th><th>出口</th><th>最近启动</th></tr>
        ${recent.map((e) => `<tr>
          <td>${esc(e.shop_name || e.shop_id || "-")}</td>
          <td>${statusBadge(e.status)}</td>
          <td>${esc(e.country || "-")}</td>
          <td>${e.kernel_major ?? "-"}</td>
          <td>${esc(EXIT_MODE[e.exit_mode] || e.exit_mode || "-")}</td>
          <td>${fmt(e.last_started_at || e.updated_at)}</td>
        </tr>`).join("")}
      </table>` : `<p class="muted">还没有环境。到「店铺管理」点「+ 新建环境」即可创建（本期只建环境与 profile，不启动窗口）。</p>`}
    </div>
    <div class="card">
      <h3>本机启动器</h3>
      <table>
        <tr><td class="k">版本</td><td>${esc(info.product_version || "-")}</td></tr>
        <tr><td class="k">数据目录</td><td><code>${esc(info.data_dir || info.data?.dir || "-")}</code></td></tr>
        <tr><td class="k">环境 profile 目录</td><td><code>${esc(info.profile_root || info.data?.profiles || "-")}</code></td></tr>
        <tr><td class="k">监听地址</td><td><code>${esc(info.server_addr || info.listen?.http || "-")}</code></td></tr>
        <tr><td class="k">存储驱动</td><td>${esc(info.store?.driver || "-")}</td></tr>
        <tr><td class="k">浏览器</td><td>${esc(info.browser?.name || "-")} ${esc(info.browser?.version || "")}</td></tr>
        <tr><td class="k">CA 就绪</td><td>${yn(info.ca_ready)}</td></tr>
        <tr><td class="k">已运行</td><td>${info.service?.uptime_sec ?? 0} 秒</td></tr>
        <tr><td class="k">统计时间</td><td>${fmt(stats.generated_at)}</td></tr>
      </table>
    </div>`;
}

function guidanceCards(info) {
  const cards = [];
  const b = info.browser || {};
  if (info.launcher?.second_instance) {
    cards.push(`<div class="card hint warn"><h3>启动器已在运行</h3>
      <p class="muted">本次以只读方式打开，不允许重复拉起环境（本期不支持多开）。请先退出另一个实例。</p></div>`);
  }
  if (!b.installed) {
    cards.push(`<div class="card hint err"><h3>未检测到 Chrome / Edge</h3>
      <p class="muted">环境启动需要本机浏览器。请先安装 Chrome，或用 <code>ZINIAO_CHROME_PATH</code> 指定浏览器路径后重启启动器。</p></div>`);
  }
  if (!info.ca_ready) {
    cards.push(`<div class="card hint warn"><h3>MITM 根证书未就绪</h3>
      <p class="muted">本地代理是 MITM 架构，CA 不在信任库里则所有 HTTPS 站点都会报证书错误，因此启动流程会在此阻断。</p>
      <p class="muted">${esc(info.data?.ca_detail || "")} · 证书目录 <code>${esc(info.data?.ca_dir || "-")}</code></p></div>`);
  }
  if (b.installed && info.ca_ready && info.capabilities?.launch) {
    cards.push(`<div class="card hint ok"><h3>环境启动就绪</h3>
      <p class="muted">浏览器：${esc(b.name || "")} ${esc(b.version || "")}（内核 ${b.major ?? "-"}）· 本机运行时条目 ${info.launcher?.runtime_entries ?? 0}</p></div>`);
  }
  return cards.join("");
}

/* ---------- 店铺管理 ---------- */
async function renderShops(c) {
  const [groupsRes, proxiesRes] = await Promise.all([
    GET("/api/v1/groups").catch(() => ({ items: [] })),
    GET("/api/v1/proxies?limit=200").catch(() => ({ items: [] })),
  ]);
  const st = {
    group_id: "", status: "", keyword: "", offset: 0, limit: 20,
    groups: groupsRes.items || [], proxies: proxiesRes.items || [],
  };
  c.innerHTML = `
    <div class="toolbar">
      <select id="f-group">
        <option value="">全部分组</option>
        ${st.groups.map((g) => `<option value="${esc(g.id)}">${esc(g.name)}（${g.environment_count ?? 0}）</option>`).join("")}
      </select>
      <select id="f-status">
        <option value="">全部状态</option>
        ${Object.keys(ENV_STATUS).map((s) => `<option value="${esc(s)}">${esc(ENV_STATUS[s])}</option>`).join("")}
      </select>
      <input id="f-kw" placeholder="店铺名 / 编号 / 环境 ID" />
      <button id="btn-search" class="primary-sm">搜索</button>
      <span class="spacer"></span>
      <button id="btn-new" class="primary-sm">+ 新建环境</button>
    </div>
    <div class="card" id="shop-list"><p class="muted">加载中…</p></div>
    <div class="pager" id="shop-pager"></div>`;

  const listEl = document.getElementById("shop-list");
  const pagerEl = document.getElementById("shop-pager");

  async function load() {
    const qs = new URLSearchParams({ limit: String(st.limit), offset: String(st.offset) });
    if (st.group_id) qs.set("group_id", st.group_id);
    if (st.status) qs.set("status", st.status);
    if (st.keyword) qs.set("keyword", st.keyword);
    let data, rt = { items: [], process_state: "unavailable" };
    try {
      [data, rt] = await Promise.all([
        GET("/api/v1/environments?" + qs.toString()),
        // 本机进程态：拿不到就按「不可用」降级，绝不把「没拿到」当成「没在跑」。
        GET("/api/v1/environments/local-runtime").catch(() => ({ items: [], process_state: "unavailable" })),
      ]);
    } catch (e) { listEl.innerHTML = `<p class="error">加载失败：${esc(e.message)}</p>`; return; }
    const local = {};
    (rt.items || []).forEach((r) => { local[r.env_id] = r; });
    const items = data.items || [];
    if (!items.length) {
      listEl.innerHTML = `<div class="empty"><div class="icon">◻</div>
        <p>没有符合条件的环境</p><p class="small">点右上角「+ 新建环境」创建店铺与环境。</p></div>`;
      pagerEl.innerHTML = "";
      return;
    }
    listEl.innerHTML = `<table>
      <tr><th>店铺 / 编号</th><th>平台</th><th>分组</th><th>状态</th><th>国家</th><th>内核 / 基线</th><th>代理</th><th>最近启动</th><th>操作</th></tr>
      ${items.map((e) => `<tr>
        <td>${esc(e.shop_name || "-")}<div class="small muted mono">${esc(e.code || e.id)}</div></td>
        <td>${esc(e.platform || "-")}${e.site ? " / " + esc(e.site) : ""}</td>
        <td>${groupCell(e)}</td>
        <td>${runBadge(e, local[e.id])}</td>
        <td>${esc(e.country || "-")}</td>
        <td>${e.kernel_major ?? "-"}<div class="small muted mono">${esc((e.baseline_hash || "").slice(0, 12))}</div></td>
        <td>${proxyCell(e)}</td>
        <td>${runtimeCell(local[e.id])}</td>
        <td><div class="row-actions">
          <button class="sm primary-sm" data-act="start" data-id="${esc(e.id)}"
            ${startDisabled(e, local[e.id], rt.process_state)}>启动</button>
          <button class="sm danger" data-act="stop" data-id="${esc(e.id)}"
            ${local[e.id] ? "" : "disabled"} title="${local[e.id] ? "" : "本机没有该环境的进程"}">停止</button>
          <button class="sm" data-act="proxy" data-id="${esc(e.id)}" data-proxy="${esc(e.proxy_id || "")}">${e.proxy_id ? "改代理" : "绑代理"}</button>
          <button class="sm danger" data-act="unbind" data-id="${esc(e.id)}" ${e.proxy_id ? "" : "disabled"}>解绑</button>
        </div></td>
      </tr>`).join("")}
    </table>`;
    // 启停：**不做乐观更新**——点下去只是「发起请求」，有没有真起来以接口为准。
    listEl.querySelectorAll("[data-act='start']").forEach((btn) => {
      btn.onclick = async () => {
        btn.disabled = true; btn.textContent = "启动中…";
        try {
          await POST(`/api/v1/environments/${btn.dataset.id}/start`, {});
          toast("已启动");
        } catch (e) { toast(e.message, true); }
        await load();
      };
    });
    listEl.querySelectorAll("[data-act='stop']").forEach((btn) => {
      btn.onclick = async () => {
        btn.disabled = true; btn.textContent = "停止中…";
        try {
          await POST(`/api/v1/environments/${btn.dataset.id}/stop`, {});
          toast("已停止");
        } catch (e) { toast(e.message, true); }
        await load();
      };
    });
    listEl.querySelectorAll("[data-act='proxy']").forEach((btn) => {
      btn.onclick = () => openModal("绑定代理", [{
        key: "proxy_id", label: "手工代理", type: "select", value: btn.dataset.proxy,
        // 只列「启用且拨测通过」的代理：服务端同样这么判（ZINIAO_PROXY_TEST=on 时），
        // 前端先过滤是为了不让运营点了之后才收到一条 400。
        options: [{ v: "", t: "（不绑定 / 解绑）" }].concat(
          st.proxies.filter((p) => p.status === "active" && p.check_status === "ok")
            .map((p) => ({ v: p.id, t: `${p.name} · ${p.protocol}://${p.host}:${p.port}${p.exit_ip ? " · " + p.exit_ip : ""}` }))),
        note: "绑定后该环境的出口走这条手工代理；解绑则回落代理商池。运行中的环境不能变更出口；只有拨测通过的代理可选。",
      }], "保存", async (v) => {
        const r = await PUT(`/api/v1/environments/${btn.dataset.id}/proxy`, { proxy_id: v.proxy_id || null });
        const warns = r.warnings || [];
        // 软校验告警不阻断绑定，但必须说出来（一号多环境 / 出口国家不一致）。
        toast(warns.length ? `已保存，注意：${warns.join("；")}` : "已保存", warns.length > 0);
        await load();
      });
    });
    listEl.querySelectorAll("[data-act='unbind']").forEach((btn) => {
      btn.onclick = async () => {
        try { await PUT(`/api/v1/environments/${btn.dataset.id}/proxy`, { proxy_id: null }); toast("已解绑"); await load(); }
        catch (e) { toast(e.message, true); }
      };
    });
    // 分组下拉在 innerHTML 里只能生成静态属性，事件必须等 DOM 就绪后挂
    bindGroupSelects();
    const nextOff = data.next_offset || 0;
    pagerEl.innerHTML = `<span>共 ${data.total} 条 · 第 ${Math.floor(st.offset / st.limit) + 1} 页</span>
      <button class="sm" id="p-prev" ${st.offset > 0 ? "" : "disabled"}>上一页</button>
      <button class="sm" id="p-next" ${nextOff ? "" : "disabled"}>下一页</button>`;
    const prev = document.getElementById("p-prev"), next = document.getElementById("p-next");
    if (prev) prev.onclick = () => { st.offset = Math.max(0, st.offset - st.limit); load(); };
    if (next) next.onclick = () => { st.offset = nextOff; load(); };
  }

  function groupCell(e) {
    const opts = [{ v: "", t: "未分组" }].concat(st.groups.map((g) => ({ v: g.id, t: g.name })));
    return `<select class="sel-group" data-id="${esc(e.id)}">
      ${opts.map((o) => `<option value="${esc(o.v)}"${o.v === (e.group_id || "") ? " selected" : ""}>${esc(o.t)}</option>`).join("")}
    </select>`;
  }
  function bindGroupSelects() {
    listEl.querySelectorAll(".sel-group").forEach((sel) => {
      sel.onchange = async () => {
        try { await PUT(`/api/v1/environments/${sel.dataset.id}/group`, { group_id: sel.value }); toast("已改组"); await load(); }
        catch (e) { toast(e.message, true); await load(); }
      };
    });
  }
  document.getElementById("btn-search").onclick = () => {
    st.group_id = document.getElementById("f-group").value;
    st.status = document.getElementById("f-status").value;
    st.keyword = document.getElementById("f-kw").value.trim();
    st.offset = 0; load();
  };
  document.getElementById("btn-new").onclick = () => newEnvModal(st, load);
  await load();
}

function newEnvModal(st, reload) {
  openModal("新建环境", [
    { key: "name", label: "店铺名称", required: true, placeholder: "如：亚马逊美国站 A 店" },
    {
      key: "platform", label: "平台", type: "select", value: "Amazon",
      options: PLATFORMS.map((p) => ({ v: p, t: p })),
    },
    { key: "site", label: "站点", placeholder: "如：US / MY / TH" },
    { key: "code", label: "店铺编号", placeholder: "如：AMZ-001（可留空）" },
    {
      key: "country", label: "国家（指纹与出口国）", placeholder: "US",
      note: "决定画像的时区/语言与代理出口国；改它不会重算已有环境的指纹。",
    },
    {
      key: "group_id", label: "分组", type: "select",
      options: [{ v: "", t: "未分组" }].concat(st.groups.map((g) => ({ v: g.id, t: g.name }))),
    },
    {
      key: "proxy_id", label: "手工代理", type: "select",
      options: [{ v: "", t: "不绑定（走代理商池 / 直连）" }].concat(
        st.proxies.filter((p) => p.status === "active" && p.check_status === "ok")
          .map((p) => ({ v: p.id, t: `${p.name} · ${p.protocol}://${p.host}:${p.port}${p.exit_ip ? " · " + p.exit_ip : ""}` }))),
    },
    { key: "start_url", label: "起始 URL", type: "url", placeholder: "https://sellercentral.amazon.com" },
    {
      key: "fingerprint_mode", label: "指纹模式", type: "select", value: "auto",
      options: [{ v: "auto", t: "自动（服务端采样画像）" }],
      note: "本期只支持 auto：指纹由服务端画像库决定，客户端不得自行随机化。",
    },
  ], "创建", async (v) => {
    await POST("/api/v1/environments", {
      name: v.name, platform: v.platform, site: v.site, code: v.code,
      country: v.country, group_id: v.group_id, proxy_id: v.proxy_id,
      start_url: v.start_url, fingerprint_mode: v.fingerprint_mode,
    });
    toast("环境已创建（本期不启动浏览器窗口）");
    await reload();
  });
}

/* ---------- 代理管理 ---------- */
async function renderProxies(c) {
  const st = { status: "", check_status: "", keyword: "", offset: 0, limit: 50 };
  c.innerHTML = `
    <div class="toolbar">
      <select id="f-status">
        <option value="">全部启停</option>
        <option value="active">启用</option>
        <option value="disabled">停用</option>
      </select>
      <select id="f-check">
        <option value="">全部拨测</option>
        <option value="ok">拨测通过</option>
        <option value="fail">拨测失败</option>
        <option value="unknown">未拨测</option>
      </select>
      <input id="f-kw" placeholder="名称 / 主机 / 备注" />
      <button id="btn-search" class="primary-sm">搜索</button>
      <span class="spacer"></span>
      <button id="btn-new" class="primary-sm">+ 新建代理</button>
    </div>
    <div class="card" id="proxy-list"><p class="muted">加载中…</p></div>
    <div class="pager" id="proxy-pager"></div>
    <div class="card hint"><p class="muted small">
      代理口令只进不出：提交后立即信封加密落库，任何接口都只回 <code>has_password</code> 布尔，
      前端与日志都拿不到明文。删除前必须先解绑引用它的环境。
    </p></div>`;

  const listEl = document.getElementById("proxy-list");
  const pagerEl = document.getElementById("proxy-pager");

  async function load() {
    const qs = new URLSearchParams({ limit: String(st.limit), offset: String(st.offset) });
    if (st.status) qs.set("status", st.status);
    if (st.check_status) qs.set("check_status", st.check_status);
    if (st.keyword) qs.set("keyword", st.keyword);
    let data;
    try { data = await GET("/api/v1/proxies?" + qs.toString()); }
    catch (e) { listEl.innerHTML = `<p class="error">加载失败：${esc(e.message)}</p>`; return; }
    const items = data.items || [];
    listEl.innerHTML = items.length ? `<table>
      <tr><th>名称</th><th>类型</th><th>协议 / 地址</th><th>账号</th><th>口令</th><th>国家 / 城市</th>
          <th>拨测</th><th>出口 IP</th><th>延迟</th><th>最近拨测</th><th>启停</th><th>被引用</th><th>操作</th></tr>
      ${items.map((p) => `<tr>
        <td>${esc(p.name)}</td>
        <td>${esc(PROXY_TYPE[p.type] || p.type || "-")}</td>
        <td class="mono">${esc(p.protocol)}://${esc(p.host)}:${p.port}</td>
        <td>${esc(p.username || "-")}</td>
        <td>${yn(p.has_password)}</td>
        <td>${esc(p.country || "-")}${p.city ? " / " + esc(p.city) : ""}</td>
        <td>${checkBadge(p)}</td>
        <td class="mono">${esc(p.exit_ip || "-")}${p.exit_country ? " (" + esc(p.exit_country) + ")" : ""}</td>
        <td>${p.latency_ms != null ? p.latency_ms + " ms" : "-"}</td>
        <td class="small">${fmt(p.last_checked_at)}</td>
        <td>${p.status === "active" ? '<span class="badge ok">启用</span>' : '<span class="badge warn">停用</span>'}</td>
        <td>${p.bound_env_count ?? 0}</td>
        <td><div class="row-actions">
          <button class="sm" data-act="edit" data-id="${esc(p.id)}">编辑</button>
          <button class="sm" data-act="test" data-id="${esc(p.id)}">拨测</button>
          <button class="sm danger" data-act="del" data-id="${esc(p.id)}" data-name="${esc(p.name)}">删除</button>
        </div></td>
      </tr>`).join("")}
    </table>` : `<div class="empty"><div class="icon">◻</div><p>还没有代理</p></div>`;

    listEl.querySelectorAll("[data-act='edit']").forEach((b) => b.onclick = () => editProxy(b.dataset.id, load));
    listEl.querySelectorAll("[data-act='test']").forEach((b) => b.onclick = async () => {
      b.disabled = true;
      try {
        const r = await POST(`/api/v1/proxies/${b.dataset.id}/test`, { timeout_ms: 5000 });
        if (r.skipped) toast(`未拨测：${r.error || "ZINIAO_PROXY_TEST=off"}`, true);
        else if (!r.ok) toast(`拨测失败：${r.error || "未知原因"}`, true);
        else toast(`拨测成功：${r.elapsed_ms} ms${r.exit_ip ? " · 出口 IP " + r.exit_ip : ""}${r.exit_country ? " · " + r.exit_country : ""}`);
        await load(); // 拨测结论已落库，列表要立刻反映（绑定校验就看它）
      } catch (e) { toast(e.message, true); } finally { b.disabled = false; }
    });
    listEl.querySelectorAll("[data-act='del']").forEach((b) => b.onclick = async () => {
      if (!confirm(`删除代理「${b.dataset.name}」？`)) return;
      try { await DEL(`/api/v1/proxies/${b.dataset.id}`); toast("已删除"); await load(); } catch (e) { toast(e.message, true); }
    });
    const nextOff = data.next_offset;
    pagerEl.innerHTML = items.length ? `<span>共 ${data.total} 条</span>
      <button class="sm" id="p-prev" ${st.offset > 0 ? "" : "disabled"}>上一页</button>
      <button class="sm" id="p-next" ${nextOff ? "" : "disabled"}>下一页</button>` : "";
    const prev = document.getElementById("p-prev"), next = document.getElementById("p-next");
    if (prev) prev.onclick = () => { st.offset = Math.max(0, st.offset - st.limit); load(); };
    if (next) next.onclick = () => { st.offset = nextOff; load(); };
  }

  document.getElementById("btn-search").onclick = () => {
    st.status = document.getElementById("f-status").value;
    st.check_status = document.getElementById("f-check").value;
    st.keyword = document.getElementById("f-kw").value.trim();
    st.offset = 0; load();
  };
  document.getElementById("btn-new").onclick = () => editProxy(null, load);
  await load();
}

/* editProxy：id 为空 = 新建。
   口令是三态（不改 / 清空 / 重设），UI 上用显式的「口令操作」下拉表达——
   用「留空即清空」这种约定，运营改个备注就会把口令抹掉，第二天环境全连不上。 */
async function editProxy(id, reload) {
  let cur = null;
  if (id) {
    // 走详情接口：只有它带 password_mask（列表为了不做 200 次解密不带掩码）。
    try { cur = await GET("/api/v1/proxies/" + id); }
    catch { const data = await GET("/api/v1/proxies?limit=200"); cur = (data.items || []).find((p) => p.id === id); }
  }
  const fields = [
    { key: "name", label: "名称", value: cur?.name, required: true },
    {
      key: "type", label: "线路类型", type: "select", value: cur?.type || "isp",
      options: Object.keys(PROXY_TYPE).map((k) => ({ v: k, t: PROXY_TYPE[k] })),
    },
    {
      key: "protocol", label: "协议", type: "select", value: cur?.protocol || "http",
      options: ["http", "https", "socks5"].map((p) => ({ v: p, t: p })),
      note: "本期只交付 HTTP/HTTPS（CONNECT 隧道，已实测）；SOCKS5 未交付——代码里有拨号实现但未经验证，请勿在生产选用。",
    },
    { key: "host", label: "主机", value: cur?.host, required: true },
    { key: "port", label: "端口", type: "number", value: cur?.port || 8000, required: true },
    { key: "username", label: "用户名", value: cur?.username },
    { key: "country", label: "国家", value: cur?.country, placeholder: "US" },
    { key: "city", label: "城市", value: cur?.city, placeholder: "可空" },
    { key: "note", label: "备注", value: cur?.note },
    {
      key: "status", label: "状态", type: "select", value: cur?.status || "active",
      options: [{ v: "active", t: "启用" }, { v: "disabled", t: "停用" }],
    },
  ];
  if (id) {
    fields.push({
      key: "pwd_action", label: "口令操作", type: "select", value: "keep",
      options: [
        { v: "keep", t: "不修改" },
        { v: "clear", t: "清空口令" },
        { v: "set", t: "重设口令" },
      ],
      note: `${cur && cur.has_password ? "当前已设置口令（" + (cur.password_mask || "***") + "）" : "当前未设置口令"}。口令只进不出：提交后加密落库，之后任何接口都不再返回明文。`,
    });
  }
  fields.push({
    key: "password", label: "新口令", type: "password",
    placeholder: id ? "仅「重设口令」时填写" : "",
    note: id ? "" : "只进不出：提交后加密落库，之后任何接口都不再返回明文。",
  });
  openModal(id ? "编辑代理" : "新建代理", fields, id ? "保存" : "创建", async (v) => {
    const body = {
      name: v.name, type: v.type, protocol: v.protocol, host: v.host, port: Number(v.port),
      username: v.username, country: v.country, city: v.city, note: v.note, status: v.status,
    };
    if (!id) {
      if (v.password !== "") body.password = v.password;
    } else if (v.pwd_action === "clear") {
      body.password = ""; // 空串 = 清空
    } else if (v.pwd_action === "set") {
      if (v.password === "") throw new Error("「重设口令」需要填写新口令");
      body.password = v.password;
    }
    if (id) await PUT(`/api/v1/proxies/${id}`, body); else await POST("/api/v1/proxies", body);
    toast("已保存");
    await reload();
  });
}

/* ---------- 分组管理 ---------- */
async function renderGroups(c) {
  c.innerHTML = `
    <div class="toolbar">
      <span class="muted small">分组用于店铺网格页的归类与筛选；只有空分组才能删除。</span>
      <span class="spacer"></span>
      <button id="btn-new" class="primary-sm">+ 新建分组</button>
    </div>
    <div class="card" id="group-list"><p class="muted">加载中…</p></div>`;
  const listEl = document.getElementById("group-list");

  async function load() {
    let data;
    try { data = await GET("/api/v1/groups"); }
    catch (e) { listEl.innerHTML = `<p class="error">加载失败：${esc(e.message)}</p>`; return; }
    const items = data.items || [];
    listEl.innerHTML = items.length ? `<table>
      <tr><th>名称</th><th>排序</th><th>环境数</th><th>创建时间</th><th>操作</th></tr>
      ${items.map((g) => `<tr>
        <td>${esc(g.name)}</td>
        <td>${g.sort ?? 0}</td>
        <td>${g.environment_count ?? 0}</td>
        <td>${fmt(g.created_at)}</td>
        <td><div class="row-actions">
          <button class="sm" data-act="edit" data-id="${esc(g.id)}" data-name="${esc(g.name)}" data-sort="${g.sort ?? 0}">重命名</button>
          <button class="sm danger" data-act="del" data-id="${esc(g.id)}" data-name="${esc(g.name)}">删除</button>
        </div></td>
      </tr>`).join("")}
    </table>` : `<div class="empty"><div class="icon">◻</div><p>还没有分组</p></div>`;
    listEl.querySelectorAll("[data-act='edit']").forEach((b) => b.onclick = () => {
      openModal("重命名分组", [
        { key: "name", label: "名称", value: b.dataset.name, required: true },
        { key: "sort", label: "排序", type: "number", value: b.dataset.sort },
      ], "保存", async (v) => {
        await PUT(`/api/v1/groups/${b.dataset.id}`, { name: v.name, sort: Number(v.sort || 0) });
        toast("已保存（成员店铺的分组名同步更新）"); await load();
      });
    });
    listEl.querySelectorAll("[data-act='del']").forEach((b) => b.onclick = async () => {
      if (!confirm(`删除分组「${b.dataset.name}」？`)) return;
      try { await DEL(`/api/v1/groups/${b.dataset.id}`); toast("已删除"); await load(); }
      catch (e) { toast(e.message, true); }
    });
  }
  document.getElementById("btn-new").onclick = () => {
    openModal("新建分组", [
      { key: "name", label: "名称", required: true },
      { key: "sort", label: "排序", type: "number", value: 0 },
    ], "创建", async (v) => {
      await POST("/api/v1/groups", { name: v.name, sort: Number(v.sort || 0) });
      toast("已创建"); await load();
    });
  };
  await load();
}

/* ---------- 设备与网络（只读台账） ---------- */
async function renderDevices(c) {
  const st = { offset: 0, limit: 50 };
  c.innerHTML = `
    <div class="card hint"><p class="muted small">
      本页是<b>数据库侧</b>的环境运行台账：只展示环境、代理绑定与内核/基线。
      本机 PID、CDP 端口、运行时长属于进程态，M3 真启动后才会出现，本期不展示（避免把空值当运行态）。
    </p></div>
    <div class="card" id="dev-list"><p class="muted">加载中…</p></div>
    <div class="pager" id="dev-pager"></div>`;
  const listEl = document.getElementById("dev-list");
  const pagerEl = document.getElementById("dev-pager");

  async function load() {
    let data;
    try { data = await GET(`/api/v1/environments/runtimes?limit=${st.limit}&offset=${st.offset}`); }
    catch (e) { listEl.innerHTML = `<p class="error">加载失败：${esc(e.message)}</p>`; return; }
    const items = data.items || [];
    listEl.innerHTML = items.length ? `<table>
      <tr><th>店铺</th><th>状态</th><th>国家</th><th>内核 / TLS</th><th>基线</th><th>出口</th><th>代理</th><th>最近启动</th></tr>
      ${items.map((e) => `<tr>
        <td>${esc(e.shop_name || e.shop_id || "-")}</td>
        <td>${statusBadge(e.status)}</td>
        <td>${esc(e.country || "-")}</td>
        <td>${e.kernel_major ?? "-"}<div class="small muted mono">${esc(e.tls_hello_id || "-")}</div></td>
        <td class="mono small">${esc((e.baseline_hash || "").slice(0, 12)) || "-"}</td>
        <td>${esc(EXIT_MODE[e.exit_mode] || e.exit_mode || "-")}</td>
        <td>${e.proxy_id ? `${esc(e.proxy_name || "")}<div class="small muted mono">${esc(e.proxy_protocol || "")}://${esc(e.proxy_addr || "")}</div>
             <div class="small muted">账号 ${esc(e.proxy_username || "-")} · 口令 ${yn(e.proxy_has_password)}</div>` : '<span class="muted">未绑定</span>'}</td>
        <td>${fmt(e.last_started_at)}</td>
      </tr>`).join("")}
    </table>` : `<div class="empty"><div class="icon">◻</div><p>还没有环境</p></div>`;
    const nextOff = data.next_offset;
    pagerEl.innerHTML = items.length ? `<span>共 ${data.total} 条</span>
      <button class="sm" id="p-prev" ${st.offset > 0 ? "" : "disabled"}>上一页</button>
      <button class="sm" id="p-next" ${nextOff ? "" : "disabled"}>下一页</button>` : "";
    const prev = document.getElementById("p-prev"), next = document.getElementById("p-next");
    if (prev) prev.onclick = () => { st.offset = Math.max(0, st.offset - st.limit); load(); };
    if (next) next.onclick = () => { st.offset = nextOff; load(); };
  }
  await load();
}

/* ---------- 日志中心 ---------- */
async function renderLogs(c) {
  const st = { sources: ["audit", "security"], level: "", type: "", env_id: "", keyword: "", from: "", to: "", offset: 0, limit: 50 };
  c.innerHTML = `
    <div class="toolbar">
      <span class="small muted">来源</span>
      <label class="small"><input type="checkbox" id="src-audit" checked /> 审计</label>
      <label class="small"><input type="checkbox" id="src-security" checked /> 安全事件</label>
      <label class="small"><input type="checkbox" id="src-egress" /> 出口历史</label>
      <select id="f-level">
        <option value="">全部级别</option>
        <option value="info">info</option>
        <option value="medium">medium</option>
        <option value="high">high</option>
      </select>
      <input id="f-type" placeholder="类别 / 类型" style="width:120px" />
      <input id="f-env" placeholder="环境 ID" style="width:180px" />
      <input id="f-kw" placeholder="关键词" style="width:140px" />
      <input id="f-from" type="date" />
      <input id="f-to" type="date" />
      <button id="btn-search" class="primary-sm">查询</button>
    </div>
    <div class="card" id="log-list"><p class="muted">加载中…</p></div>
    <div class="pager" id="log-pager"></div>`;
  const listEl = document.getElementById("log-list");
  const pagerEl = document.getElementById("log-pager");

  async function load() {
    const qs = new URLSearchParams({ limit: String(st.limit), offset: String(st.offset), sources: st.sources.join(",") });
    if (st.level) qs.set("level", st.level);
    if (st.type) qs.set("type", st.type);
    if (st.env_id) qs.set("env_id", st.env_id);
    if (st.keyword) qs.set("keyword", st.keyword);
    if (st.from) qs.set("from", st.from + "T00:00:00Z");
    if (st.to) qs.set("to", st.to + "T23:59:59Z");
    let data;
    try { data = await GET("/api/v1/logs?" + qs.toString()); }
    catch (e) { listEl.innerHTML = `<p class="error">加载失败：${esc(e.message)}</p>`; return; }
    const items = data.items || [];
    listEl.innerHTML = items.length ? `<table>
      <tr><th>时间</th><th>来源</th><th>级别</th><th>类别</th><th>动作</th><th>摘要</th><th>店铺 / 环境</th></tr>
      ${items.map((l) => `<tr>
        <td>${fmt(l.occurred_at)}</td>
        <td>${esc(l.source)}</td>
        <td>${levelBadge(l.level)}</td>
        <td>${esc(l.category || "-")}</td>
        <td class="mono small">${esc(l.action)}</td>
        <td>${esc(l.summary)}</td>
        <td class="small muted mono">${esc(l.shop_id || l.env_id || "-")}</td>
      </tr>`).join("")}
    </table>` : `<div class="empty"><div class="icon">◻</div><p>没有符合条件的日志</p></div>`;
    const nextOff = data.next_offset;
    pagerEl.innerHTML = items.length ? `<span>共 ${data.total} 条</span>
      <button class="sm" id="p-prev" ${st.offset > 0 ? "" : "disabled"}>上一页</button>
      <button class="sm" id="p-next" ${nextOff ? "" : "disabled"}>下一页</button>` : "";
    const prev = document.getElementById("p-prev"), next = document.getElementById("p-next");
    if (prev) prev.onclick = () => { st.offset = Math.max(0, st.offset - st.limit); load(); };
    if (next) next.onclick = () => { st.offset = nextOff; load(); };
  }
  function levelBadge(l) {
    if (l === "high") return '<span class="badge err">high</span>';
    if (l === "medium") return '<span class="badge warn">medium</span>';
    return '<span class="badge">info</span>';
  }
  document.getElementById("btn-search").onclick = () => {
    const src = [];
    if (document.getElementById("src-audit").checked) src.push("audit");
    if (document.getElementById("src-security").checked) src.push("security");
    if (document.getElementById("src-egress").checked) src.push("egress");
    st.sources = src.length ? src : ["audit"];
    st.level = document.getElementById("f-level").value;
    st.type = document.getElementById("f-type").value.trim();
    st.env_id = document.getElementById("f-env").value.trim();
    st.keyword = document.getElementById("f-kw").value.trim();
    st.from = document.getElementById("f-from").value;
    st.to = document.getElementById("f-to").value;
    st.offset = 0; load();
  };
  await load();
}

/* ---------- 系统设置（system/info 只读展示） ---------- */
async function renderSettings(c) {
  const info = await systemInfo();
  const b = info.browser || {};
  const caps = info.capabilities || {};
  const capBadges = Object.keys(caps).map((k) => `${esc(k)} ${yn(caps[k])}`).join(" · ");
  const cands = (b.candidates || []).map((x) => `<tr><td>${esc(x.kind)}</td><td><code>${esc(x.path)}</code></td><td>${esc(x.version || "-")}</td></tr>`).join("")
    || `<tr><td colspan="3" class="muted">未探测到本机浏览器</td></tr>`;
  c.innerHTML = `
    <div class="card">
      <h3>产品与路径</h3>
      <table>
        <tr><td class="k">产品版本</td><td>${esc(info.product_version || "-")}</td></tr>
        <tr><td class="k">数据目录</td><td><code>${esc(info.data_dir || info.data?.dir || "-")}</code></td></tr>
        <tr><td class="k">profile 根目录</td><td><code>${esc(info.profile_root || info.data?.profiles || "-")}</code></td></tr>
        <tr><td class="k">扩展根目录</td><td><code>${esc(info.extension_root || "-")}</code></td></tr>
        <tr><td class="k">运行时注册表</td><td><code>${esc(info.data?.runtime || "-")}</code></td></tr>
        <tr><td class="k">CA 目录</td><td><code>${esc(info.data?.ca_dir || "-")}</code></td></tr>
        <tr><td class="k">CA 就绪</td><td>${yn(info.ca_ready)}${info.data?.ca_detail ? ` <span class="muted small">${esc(info.data.ca_detail)}</span>` : ""}</td></tr>
      </table>
    </div>
    <div class="card">
      <h3>服务</h3>
      <table>
        <tr><td class="k">名称</td><td>${esc(info.service?.name || "-")}</td></tr>
        <tr><td class="k">运行环境</td><td>${esc(info.service?.env || "-")}</td></tr>
        <tr><td class="k">存储驱动</td><td>${esc(info.store?.driver || "-")}</td></tr>
        <tr><td class="k">监听地址</td><td><code>${esc(info.server_addr || info.listen?.http || "-")}</code></td></tr>
        <tr><td class="k">已运行</td><td>${info.service?.uptime_sec ?? 0} 秒</td></tr>
      </table>
    </div>
    <div class="card">
      <h3>浏览器</h3>
      <table>
        <tr><td class="k">已安装</td><td>${yn(b.installed)}</td></tr>
        <tr><td class="k">名称</td><td>${esc(b.name || "-")}</td></tr>
        <tr><td class="k">版本</td><td>${esc(b.version || "-")}（内核 ${b.major ?? "-"}）</td></tr>
        <tr><td class="k">路径</td><td><code>${esc(b.path || "-")}</code></td></tr>
      </table>
      <h3 style="margin-top:16px">候选</h3>
      <table><tr><th>类型</th><th>路径</th><th>版本</th></tr>${cands}</table>
    </div>
    <div class="card">
      <h3>能力位</h3>
      <p>${capBadges || "-"}</p>
      <p class="muted small">launch 需同时满足「浏览器已安装」与「CA 已就绪」；sidepanel 本期默认不加载（规避 Chrome 开发者模式提示条）。</p>
    </div>
    <div class="card">
      <h3>限额</h3>
      <table>
        <tr><td class="k">环境回收时长</td><td>${info.limits?.env_reclaim_hours ?? "-"} 小时</td></tr>
        <tr><td class="k">接口限流</td><td>${info.limits?.api_rate_per_minute ?? "-"} 次/分钟</td></tr>
      </table>
    </div>
    ${await caCard()}`;
}

/* ---------- MITM 根证书（M3/T7：ca_ready 阻断闭环的操作入口） ---------- */

// hasPerm 当前用户是否持有某权限码（只用于「画不画按钮」，能不能仍由服务端裁决）。
const hasPerm = (code) => (state.me && Array.isArray(state.me.permissions) ? state.me.permissions.includes(code) : false);

async function caCard() {
  const ca = await GET("/api/v1/system/ca").catch(() => null);
  if (!ca) return `<div class="card"><h3>MITM 根证书</h3><p class="muted">证书状态读取失败</p></div>`;
  const manage = hasPerm("ca:manage");
  const btns = manage
    ? `<button class="primary-sm" onclick="caAction('install')">安装到当前用户信任库</button>
       <button onclick="caAction('uninstall')">从信任库移除</button>`
    : `<p class="muted small">安装 / 卸载本机信任库证书需要管理员权限（ca:manage）。</p>`;
  return `<div class="card">
      <h3>MITM 根证书</h3>
      <p class="muted small">本地代理是 MITM 架构：证书不在当前用户信任库里，浏览器打开 HTTPS 站点会报证书错误，
      因此 ca_ready=false 时环境启动会被直接阻断（不用 --ignore-certificate-errors 绕过）。</p>
      <table>
        <tr><td class="k">证书路径</td><td><code>${esc(ca.cert_path || "-")}</code></td></tr>
        <tr><td class="k">已生成</td><td>${yn(ca.present)}</td></tr>
        <tr><td class="k">已装入信任库</td><td>${yn(ca.installed)}</td></tr>
        <tr><td class="k">就绪</td><td>${yn(ca.ready)}${ca.detail ? ` <span class="muted small">${esc(ca.detail)}</span>` : ""}</td></tr>
      </table>
      <div class="row-actions">${btns}</div>
    </div>`;
}

async function caAction(kind) {
  try {
    const r = await POST("/api/v1/system/ca/" + kind, {});
    toast(kind === "install" ? "根证书已安装（ca_ready=" + (r.ca?.ready ? "true" : "false") + "）"
                             : "根证书已从信任库移除（启动将再次被阻断）");
  } catch (e) {
    toast("操作失败：" + e.message, true);
    return;
  }
  state.info = null; // system/info 缓存了 ca_ready，必须失效后重画
  await go("settings");
}

const RENDERERS = {
  dashboard: renderDashboard,
  shops:     renderShops,
  proxies:   renderProxies,
  groups:    renderGroups,
  devices:   renderDevices,
  logs:      renderLogs,
  settings:  renderSettings,
};

boot();
