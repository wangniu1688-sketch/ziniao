/* 紫鸟浏览器管理系统 · Web 控制台（vanilla JS，无构建依赖）
   角色权限由服务端裁决：无权限的按钮点击会收到 403，前端只做展示分组。 */
"use strict";

const state = { token: localStorage.getItem("zn_token") || "", me: null };

const SECTIONS = [
  { id: "dashboard",     title: "工作台",    roles: null },
  { id: "shops",         title: "店铺管理",  roles: null },
  { id: "approvals",     title: "审批工单",  roles: null },
  { id: "devices",       title: "设备与网络", roles: null },
  { id: "security",      title: "安全中心",  roles: null },
  { id: "audit",         title: "审计日志",  roles: null },
  { id: "notifications", title: "通知中心",  roles: null },
  { id: "team",          title: "团队与成员", roles: null },
  { id: "system",        title: "系统管理",  roles: null },
];

/* ---------- 基础 ---------- */
async function api(method, path, body, raw) {
  const opt = { method, headers: {} };
  if (state.token) opt.headers.Authorization = "Bearer " + state.token;
  if (body !== undefined) { opt.headers["Content-Type"] = "application/json"; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (res.status === 401) { logout(); throw new Error("登录已过期"); }
  if (!res.ok && !raw) {
    const msg = (data && (data.message || data.error)) || text || res.status;
    throw new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
  }
  return { status: res.status, body: data, text };
}
const GET = (p) => api("GET", p).then(r => r.body);
const POST = (p, b) => api("POST", p, b || {});
const PUT = (p, b) => api("PUT", p, b);
const DEL = (p) => api("DELETE", p);

function toast(msg, isErr) {
  const el = document.getElementById("toast");
  el.textContent = msg; el.className = "toast" + (isErr ? " err" : "");
  clearTimeout(el._t); el._t = setTimeout(() => el.classList.add("hidden"), 4200);
}
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const badge = (s) => {
  const cls = /正常|approved|active|ok/i.test(s) ? "ok" : /受限|reject|fail|deny|expired/i.test(s) ? "err" : /待|pending|养号|warn/i.test(s) ? "warn" : "";
  return `<span class="badge ${cls}">${esc(s)}</span>`;
};
const fmt = (t) => t ? new Date(t).toLocaleString("zh-CN", { hour12: false }) : "-";

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
    state.token = r.body.token;
    localStorage.setItem("zn_token", state.token);
    await boot();
  } catch (e) { errEl.textContent = "登录失败：" + e.message; }
  return false;
}
function logout() { state.token = ""; state.me = null; localStorage.removeItem("zn_token"); show("login-view"); }

async function boot() {
  if (!state.token) { show("login-view"); return; }
  try { state.me = await GET("/api/v1/me"); } catch { return; }
  show("app-view");
  document.getElementById("whoami").textContent = `${state.me.username || ""} · ${state.me.tenant_id || ""}`;
  renderNav(); go(location.hash.replace("#/", "") || "dashboard");
}
function show(id) {
  document.getElementById("login-view").classList.toggle("hidden", id !== "login-view");
  document.getElementById("app-view").classList.toggle("hidden", id !== "app-view");
}

/* ---------- 导航 ---------- */
function renderNav() {
  document.getElementById("nav").innerHTML = SECTIONS
    .map(s => `<a href="#/${s.id}" data-id="${s.id}">${s.title}</a>`).join("");
}
async function go(id) {
  const sec = SECTIONS.find(s => s.id === id) || SECTIONS[0];
  document.querySelectorAll("#nav a").forEach(a => a.classList.toggle("active", a.dataset.id === sec.id));
  document.getElementById("section-title").textContent = sec.title;
  const c = document.getElementById("content");
  c.innerHTML = `<p class="muted">加载中…</p>`;
  try { await RENDERERS[sec.id](c); }
  catch (e) { c.innerHTML = `<div class="card error">加载失败：${esc(e.message)}</div>`; }
}
window.addEventListener("hashchange", () => go(location.hash.replace("#/", "")));

/* ---------- 工作台 ---------- */
async function renderDashboard(c) {
  const [shops, devices, approvalsP, approvalsAll, events, notif, verify] = await Promise.all([
    GET("/api/v1/shops?limit=100"),
    GET("/api/v1/devices?limit=100").catch(() => ({ items: [], total: 0 })),
    GET("/api/v1/approvals?status=pending&limit=1").catch(() => ({ total: 0 })),
    GET("/api/v1/approvals?limit=1").catch(() => ({ total: 0 })),
    GET("/api/v1/security/events?limit=1").catch(() => ({ total: 0 })),
    GET("/api/v1/notifications?limit=1").catch(() => ({ total: 0 })),
    GET("/api/v1/audit/verify").catch(() => null),
  ]);
  const running = (shops.items || []).filter(s => s.status === "正常运营").length;
  c.innerHTML = `
    <div class="grid">
      <div class="stat"><b>${shops.total ?? 0}</b><span class="muted">店铺总数（运营中 ${running}）</span></div>
      <div class="stat"><b>${devices.total ?? 0}</b><span class="muted">网络设备</span></div>
      <div class="stat"><b>${approvalsP.total ?? 0}</b><span class="muted">待审批工单</span></div>
      <div class="stat"><b>${approvalsAll.total ?? 0}</b><span class="muted">累计审批单</span></div>
      <div class="stat"><b>${events.total ?? 0}</b><span class="muted">安全事件</span></div>
      <div class="stat"><b>${notif.total ?? 0}</b><span class="muted">未读/站内通知</span></div>
    </div>
    <div class="card">
      <h3>审计哈希链完整性（L5 防篡改）</h3>
      ${verify ? `<p>校验结果 <b class="${verify.ok ? "" : "error"}">${verify.ok ? "通过" : "失败"}</b>，检查 ${verify.checked ?? "-"} 条</p>`
               : `<p class="muted">不可用</p>`}
    </div>
    <div class="card"><h3>最近店铺</h3>${shopTable((shops.items || []).slice(0, 5))}</div>`;
}

/* ---------- 店铺 ---------- */
function shopTable(items, withActions) {
  if (!items.length) return `<p class="muted">暂无数据</p>`;
  return `<table><tr><th>名称</th><th>平台</th><th>站点</th><th>状态</th><th>分组</th>${withActions ? "<th>操作</th>" : ""}</tr>` +
    items.map(s => `<tr>
      <td>${esc(s.name)}</td><td>${esc(s.platform)}</td><td>${esc(s.site)}</td>
      <td>${badge(s.status)}</td><td>${esc(s.group || "-")}</td>
      ${withActions ? `<td>
        <select onchange="transitionShop('${s.id}', this.value)">
          <option value="">状态迁移…</option>
          <option>注册中</option><option>养号期</option><option>正常运营</option>
          <option>审核中</option><option>受限</option><option>已弃用</option>
        </select>
        <button onclick="shopCred('${s.id}', '${esc(s.name)}')">凭证</button>
        <button class="danger" onclick="trashShop('${s.id}')">删除</button>
      </td>` : ""}</tr>`).join("") + `</table>`;
}
async function renderShops(c) {
  const [shops, trash] = await Promise.all([
    GET("/api/v1/shops?limit=100"),
    GET("/api/v1/shops/trash?limit=100").catch(() => ({ items: [], total: 0 })),
  ]);
  c.innerHTML = `
    <div class="card"><h3>新建店铺</h3>
      <div class="row">
        <label>名称 <input id="sp-name" placeholder="亚马逊美国站01"></label>
        <label>平台 <select id="sp-platform"><option>Amazon</option><option>Shopee</option><option>Lazada</option><option>eBay</option><option>TikTok Shop</option></select></label>
        <label>站点 <input id="sp-site" value="US" style="width:70px"></label>
        <button class="primary" onclick="createShop()">创建</button>
      </div>
    </div>
    <div class="card"><h3>批量导入（每行：名称,平台,站点 —— 逐行校验，部分成功返回 207）</h3>
      <textarea id="sp-batch" placeholder="店A,Amazon,US&#10;店B,Shopee,MY"></textarea>
      <div class="row" style="margin-top:8px"><button class="primary" onclick="batchImport()">导入</button><span id="sp-batch-res" class="muted small"></span></div>
    </div>
    <div class="card"><h3>店铺列表（${shops.total}）</h3>${shopTable(shops.items, true)}</div>
    <div class="card"><h3>回收站（${trash.total}）</h3>
      ${(trash.items || []).length ? `<table><tr><th>名称</th><th>软删时间</th><th>操作</th></tr>` +
        trash.items.map(s => `<tr><td>${esc(s.name)}</td><td>${fmt(s.deleted_at)}</td>
          <td><button onclick="restoreShop('${s.id}')">恢复</button>
          <button class="danger" onclick="purgeShop('${s.id}')">彻底删除</button></td></tr>`).join("") + `</table>`
        : `<p class="muted">空</p>`}
    </div>`;
}
async function createShop() {
  try {
    await POST("/api/v1/shops", {
      name: val("sp-name"), platform: val("sp-platform"), site: val("sp-site"), url: "/shops",
    });
    toast("店铺已创建"); go("shops");
  } catch (e) { toast(e.message, true); }
}
async function batchImport() {
  const rows = val("sp-batch").split("\n").map(l => l.trim()).filter(Boolean)
    .map(l => { const [name, platform, site] = l.split(",").map(x => (x || "").trim()); return { name, platform: platform || "Amazon", site: site || "US" }; });
  if (!rows.length) { toast("请输入导入内容", true); return; }
  try {
    const r = await api("POST", "/api/v1/shops/batch/import", { rows, url: "/shops" }, true);
    document.getElementById("sp-batch-res").textContent =
      `总计 ${r.body.total}，成功 ${r.body.created}，失败 ${r.body.failed}` +
      (r.body.failures || []).map(f => `；第${f.row}行「${f.name}」：${f.reason}`).join("");
    toast("批量导入完成（HTTP " + r.status + "）"); go("shops");
  } catch (e) { toast(e.message, true); }
}
async function transitionShop(id, to) { if (!to) return; try { await POST(`/api/v1/shops/${id}/status`, { to, url: "/shops" }); toast("状态已迁移到 " + to); go("shops"); } catch (e) { toast(e.message, true); go("shops"); } }
async function trashShop(id) { if (!confirm("确认删除（进入回收站，30 天后自动清理）？")) return; try { await DEL(`/api/v1/shops/${id}`); toast("已删除"); go("shops"); } catch (e) { toast(e.message, true); } }
async function restoreShop(id) { try { await POST(`/api/v1/shops/${id}/restore`); toast("已恢复"); go("shops"); } catch (e) { toast(e.message, true); } }
async function purgeShop(id) { if (!confirm("彻底删除后不可恢复，确认？")) return; try { await POST(`/api/v1/shops/${id}/restore`).catch(() => {}); await DEL(`/api/v1/shops/${id}`); toast("已彻底删除"); go("shops"); } catch (e) { toast(e.message, true); } }

/* 凭证面板：列表只显示类型；明文必须走审批工单（服务端硬拦，反例 8） */
async function shopCred(id, name) {
  let kinds = { kinds: [] };
  try { kinds = await GET(`/api/v1/shops/${id}/credentials`); } catch (e) { toast(e.message, true); return; }
  const c = document.getElementById("content");
  c.innerHTML = `
    <h2>凭证托管 · ${esc(name)}</h2>
    <div class="card"><h3>已存凭证类型</h3><p>${(kinds.kinds || []).map(k => badge(k)).join(" ") || "无"}</p></div>
    <div class="card"><h3>写入凭证（信封加密，密文不落日志）</h3>
      <div class="row">
        <label>类型 <select id="cr-kind"><option>password</option><option>cookie</option><option>totp_secret</option><option>api_key</option></select></label>
        <label>用户名 <input id="cr-user"></label>
        <label>密文 <input id="cr-secret" type="password"></label>
        <button class="primary" onclick="credPut('${id}')">保存</button>
      </div></div>
    <div class="card"><h3>查看明文（需审批工单，一次性核销）</h3>
      <div class="row">
        <label>类型 <select id="cr-kind2"><option>password</option></select></label>
        <label>事由 <input id="cr-reason" placeholder="排查登录异常"></label>
        <label>审批单号 <input id="cr-ticket" placeholder="留空则自动申请" style="width:230px"></label>
        <button onclick="credReveal('${id}')">查看明文</button>
      </div><pre class="json" id="cr-out"></pre></div>
    <button onclick="go('shops')">返回店铺列表</button>`;
}
async function credPut(id) {
  try {
    await PUT(`/api/v1/shops/${id}/credentials`, {
      kind: val("cr-kind"), username: val("cr-user"), secret: val("cr-secret"), url: "/shops",
    });
    toast("凭证已加密保存"); shopCred(id, val("cr-user") || "店铺");
  } catch (e) { toast(e.message, true); }
}
async function credReveal(id) {
  const out = document.getElementById("cr-out");
  const kind = val("cr-kind2"), reason = val("cr-reason");
  let ticket = val("cr-ticket");
  try {
    if (!ticket) {
      const t = await POST("/api/v1/approvals", {
        type: "shop.credential.reveal", subject_type: "shop", subject_id: id, reason, url: "/approvals",
      });
      ticket = t.id;
      out.textContent = `审批单已提交：${ticket}\n状态：pending\n提示：需要另一位有审批权限的成员在「审批工单」页通过后，回到本页粘贴单号查看明文。\n（自己不能审批自己的申请）`;
      document.getElementById("cr-ticket").value = ticket;
      return;
    }
    const r = await api("GET", `/api/v1/shops/${id}/credentials/${kind}?reason=${encodeURIComponent(reason)}&ticket=${encodeURIComponent(ticket)}`, undefined, true);
    out.textContent = "明文：" + (r.body.secret ?? JSON.stringify(r.body));
    toast("查看成功（审批单已核销）");
  } catch (e) {
    out.textContent = "失败：" + e.message + "\n（若单据已核销/过期，请留空单号重新申请）";
    toast(e.message, true);
  }
}

/* ---------- 审批工单 ---------- */
async function renderApprovals(c) {
  const status = new URLSearchParams(location.hash.split("?")[1] || "").get("status") || "";
  const data = await GET(`/api/v1/approvals?status=${status}&limit=100`);
  c.innerHTML = `
    <div class="card"><h3>提交审批单（高危动作必须二次审批：审计明文导出 / 凭证明文 / 批量删除 / 设备购买）</h3>
      <div class="row">
        <label>类型 <select id="ap-type">
          <option value="audit.export.plain">审计明文导出</option>
          <option value="shop.credential.reveal">凭证明文查看</option>
          <option value="shop.bulk_delete">店铺批量删除</option>
          <option value="device.purchase">设备购买</option>
        </select></label>
        <label>对象ID <input id="ap-subject" placeholder="可选"></label>
        <label>事由 <input id="ap-reason" style="width:220px"></label>
        <button class="primary" onclick="createApproval()">提交</button>
      </div></div>
    <div class="card">
      <h3>工单列表（${data.total}）</h3>
      <div class="row" style="margin-bottom:6px">
        ${["", "pending", "approved", "rejected", "expired", "cancelled", "used"].map(s =>
          `<a class="badge" href="#/approvals${s ? "?status=" + s : ""}">${s || "全部"}</a>`).join(" ")}
      </div>
      ${approvalTable(data.items)}
    </div>`;
}
function approvalTable(items) {
  if (!items || !items.length) return `<p class="muted">暂无工单</p>`;
  return `<table><tr><th>单号</th><th>类型</th><th>申请人</th><th>审批人</th><th>状态</th><th>事由</th><th>提交时间</th><th>有效期至</th><th>操作</th></tr>` +
    items.map(t => `<tr>
      <td class="small">${esc(t.id)}</td><td>${esc(t.type)}</td><td>${esc(t.requester_name || t.requester_id)}</td>
      <td>${esc(t.approver_name || t.approver_id || "-")}</td><td>${badge(t.status)}</td>
      <td>${esc(t.reason || "-")}</td><td class="small">${fmt(t.created_at)}</td><td class="small">${fmt(t.expires_at)}</td>
      <td>${t.status === "pending" ? `
        <button onclick="decide('${t.id}','approve')">通过</button>
        <button class="danger" onclick="decide('${t.id}','reject')">拒绝</button>
        <button onclick="decide('${t.id}','cancel')">撤销</button>` : ""}</td></tr>`).join("") + `</table>`;
}
async function createApproval() {
  try {
    const t = await POST("/api/v1/approvals", {
      type: val("ap-type"), subject_type: "shop", subject_id: val("ap-subject"),
      reason: val("ap-reason"), url: "/approvals",
    });
    toast("已提交：" + t.id); go("approvals");
  } catch (e) { toast(e.message, true); }
}
async function decide(id, act) {
  try { await POST(`/api/v1/approvals/${id}/${act}`, { comment: "", url: "/approvals" }); toast("已" + (act === "approve" ? "通过" : act === "reject" ? "拒绝" : "撤销")); go("approvals"); }
  catch (e) { toast(e.message, true); }
}

/* ---------- 设备 ---------- */
async function renderDevices(c) {
  const devices = await GET("/api/v1/devices?limit=100").catch(() => ({ items: [], total: 0 }));
  const shops = await GET("/api/v1/shops?limit=100");
  c.innerHTML = `
    <div class="card"><h3>购买云网络设备</h3>
      <div class="row">
        <label>名称 <input id="dv-name" placeholder="美国住宅-洛杉矶"></label>
        <label>国家 <input id="dv-country" value="US" style="width:60px"></label>
        <label>地区 <input id="dv-region" placeholder="California"></label>
        <label>周期(月) <input id="dv-months" type="number" value="1" style="width:70px"></label>
        <button class="primary" onclick="purchaseDevice()">购买</button>
      </div></div>
    <div class="card"><h3>绑定到店铺（配额模型：一台设备可绑多账号，并发按人去重）</h3>
      <div class="row">
        <label>设备 <select id="dv-id">${(devices.items || []).map(d => `<option value="${d.id}">${esc(d.name)}</option>`).join("")}</select></label>
        <label>店铺 <select id="dv-shop">${(shops.items || []).map(s => `<option value="${s.id}">${esc(s.name)}</option>`).join("")}</select></label>
        <button onclick="bindDevice()">绑定</button>
        <button onclick="releaseDevice()">释放占用</button>
      </div></div>
    <div class="card"><h3>设备列表（${devices.total ?? 0}）</h3>
      ${(devices.items || []).length ? `<table><tr><th>名称</th><th>类型</th><th>国家</th><th>到期</th><th>绑定账号上限</th><th>并发上限</th><th>操作</th></tr>` +
        devices.items.map(d => `<tr>
          <td>${esc(d.name)}</td><td>${esc(d.type || "cloud")}</td><td>${esc(d.country)}</td>
          <td>${fmt(d.expires_at)}</td><td>${d.max_bound_accounts ?? "-"}</td><td>${d.max_concurrent ?? "-"}</td>
          <td><button onclick="renewDevice('${d.id}')">续费1月</button></td></tr>`).join("") + `</table>`
        : `<p class="muted">暂无设备</p>`}</div>`;
}
const dvId = () => val("dv-id");
async function purchaseDevice() {
  try {
    await POST("/api/v1/devices/purchase", {
      name: val("dv-name"), type: "cloud", country: val("dv-country"), region: val("dv-region"),
      period_months: Number(val("dv-months") || 1), url: "/devices",
    });
    toast("设备已购入"); go("devices");
  } catch (e) { toast(e.message, true); }
}
async function bindDevice() { try { await POST(`/api/v1/devices/${dvId()}/bind`, { shop_id: val("dv-shop"), url: "/devices" }); toast("已绑定"); go("devices"); } catch (e) { toast(e.message, true); } }
async function releaseDevice() { try { await POST(`/api/v1/devices/${dvId()}/release`, { url: "/devices" }); toast("已释放"); go("devices"); } catch (e) { toast(e.message, true); } }
async function renewDevice(id) { try { await POST(`/api/v1/devices/${id}/renew`, { months: 1, url: "/devices" }); toast("已续费"); go("devices"); } catch (e) { toast(e.message, true); } }

/* ---------- 安全中心 ---------- */
async function renderSecurity(c) {
  const [events, policies, templates] = await Promise.all([
    GET("/api/v1/security/events?limit=50"),
    GET("/api/v1/security/policies").catch(() => ({ items: [] })),
    GET("/api/v1/security/templates").catch(() => ({ items: [] })),
  ]);
  c.innerHTML = `
    <div class="card"><h3>套用策略模板（事前拦截）</h3>
      <div class="row">
        <label>模板 <select id="se-tpl">${(templates.items || []).map(t => `<option value="${t.code}">${esc(t.name)}</option>`).join("")}</select></label>
        <label>名称 <input id="se-name" value="高危操作拦截"></label>
        <button class="primary" onclick="applyTemplate()">套用</button>
      </div>
      ${(policies.items || []).length ? `<table><tr><th>名称</th><th>范围</th><th>规则数</th><th>启用</th><th>操作</th></tr>` +
        policies.items.map(p => `<tr><td>${esc(p.name)}</td><td>${esc(p.scope)}</td><td>${(p.rules || []).length}</td>
          <td>${p.enabled ? "是" : "否"}</td>
          <td><button class="danger" onclick="delPolicy('${p.id}')">删除</button></td></tr>`).join("") + `</table>`
        : `<p class="muted">暂无策略</p>`}</div>
    <div class="card"><h3>安全事件（事中告警 + 事后追溯）</h3>
      ${(events.items || []).length ? `<table><tr><th>时间</th><th>级别</th><th>类别</th><th>动作</th><th>店铺</th><th>设备</th></tr>` +
        events.items.map(e => `<tr><td class="small">${fmt(e.created_at)}</td>
          <td>${badge(e.level)}</td><td>${esc(e.category)}</td><td>${esc(e.action)}</td>
          <td class="small">${esc(e.shop_id || "-")}</td><td class="small">${esc(e.device_id || "-")}</td></tr>`).join("") + `</table>`
        : `<p class="muted">暂无事件</p>`}</div>`;
}
async function applyTemplate() {
  try {
    await POST("/api/v1/security/policies", { name: val("se-name"), template: val("se-tpl"), enabled: true, url: "/security" });
    toast("策略已套用"); go("security");
  } catch (e) { toast(e.message, true); }
}
async function delPolicy(id) { try { await DEL(`/api/v1/security/policies/${id}`); toast("已删除"); go("security"); } catch (e) { toast(e.message, true); } }

/* ---------- 审计 ---------- */
async function renderAudit(c) {
  const logs = await GET("/api/v1/audit/logs?limit=50");
  c.innerHTML = `
    <div class="card"><h3>操作（含水印导出与审批联动）</h3>
      <div class="row">
        <button onclick="verifyChain()">校验哈希链</button>
        <label>导出审批单号 <input id="au-ticket" placeholder="留空=先提交审批" style="width:250px"></label>
        <button onclick="exportAudit()">导出 CSV（带水印）</button>
      </div>
      <pre class="json" id="au-out"></pre></div>
    <div class="card"><h3>审计日志（最近 50 条）</h3>
      ${(logs.items || []).length ? `<table><tr><th>时间</th><th>类别</th><th>操作者</th><th>动作</th><th>结果</th><th>IP</th></tr>` +
        logs.items.map(l => `<tr><td class="small">${fmt(l.occurred_at)}</td><td>${esc(l.category)}</td>
          <td>${esc(l.actor_name || l.actor || "-")}</td><td>${esc(l.action)}</td>
          <td>${esc(l.result || "-")}</td><td class="small">${esc(l.ip || l.terminal_ip || "-")}</td></tr>`).join("") + `</table>`
        : `<p class="muted">暂无日志</p>`}</div>`;
}
async function verifyChain() {
  try { const v = await GET("/api/v1/audit/verify"); document.getElementById("au-out").textContent = `哈希链校验：${v.ok ? "通过" : "失败"}（检查 ${v.checked} 条）`; }
  catch (e) { toast(e.message, true); }
}
async function exportAudit() {
  const out = document.getElementById("au-out");
  try {
    let ticket = val("au-ticket");
    if (!ticket) {
      const t = await POST("/api/v1/approvals", { type: "audit.export.plain", subject_type: "audit", reason: "控制台导出", url: "/approvals" });
      ticket = t.id;
      out.textContent = `导出需二次审批（反例 8）。\n已提交审批单：${ticket}\n请让另一位成员在「审批工单」页通过后，把单号粘贴到输入框再次导出。`;
      document.getElementById("au-ticket").value = ticket;
      return;
    }
    const r = await api("GET", `/api/v1/audit/logs/export?ticket=${encodeURIComponent(ticket)}`, undefined, true);
    const blob = new Blob([r.text], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob); a.download = "audit-export.csv"; a.click();
    out.textContent = "已下载（含水印头）。审批单已核销。";
  } catch (e) { out.textContent = "导出失败：" + e.message; }
}

/* ---------- 通知 ---------- */
async function renderNotifications(c) {
  const data = await GET("/api/v1/notifications?limit=50");
  c.innerHTML = (data.items || []).length
    ? `<div class="card"><table><tr><th>时间</th><th>类别</th><th>标题</th><th>内容</th><th>操作</th></tr>` +
      data.items.map(n => `<tr><td class="small">${fmt(n.created_at)}</td><td>${esc(n.category)}</td>
        <td>${esc(n.title)}</td><td class="small">${esc(n.body)}</td>
        <td>${n.read_at ? `<span class="muted small">已读</span>` : `<button onclick="readNotif('${n.id}')">标记已读</button>`}</td></tr>`).join("") + `</table></div>`
    : `<p class="muted">暂无通知</p>`;
}
async function readNotif(id) { try { await POST(`/api/v1/notifications/${id}/read`); go("notifications"); } catch (e) { toast(e.message, true); } }

/* ---------- 团队 ---------- */
async function renderTeam(c) {
  const tenantID = state.me.tenant_id;
  const [members, roles] = await Promise.all([
    GET(`/api/v1/tenants/${tenantID}/members`).catch(() => ({ items: [] })),
    GET("/api/v1/iam/roles").catch(() => ({ items: [] })),
  ]);
  c.innerHTML = `
    <div class="card"><h3>邀请成员</h3>
      <div class="row">
        <label>用户名 <input id="tm-user"></label>
        <label>初始口令 <input id="tm-pass" type="password" value="Zn@12345678"></label>
        <label>昵称 <input id="tm-nick"></label>
        <button class="primary" onclick="inviteMember()">邀请</button>
      </div></div>
    <div class="card"><h3>成员（${(members.items || []).length}）</h3>
      ${(members.items || []).length ? `<table><tr><th>用户名</th><th>昵称</th><th>授予角色</th></tr>` +
        members.items.map(m => `<tr><td>${esc(m.username)}</td><td>${esc(m.nickname || "-")}</td>
          <td><div class="row">
            <select id="role-${m.id}">${(roles.items || []).map(r => `<option value="${r.code}">${esc(r.name || r.code)}</option>`).join("")}</select>
            <button onclick="grantRole('${tenantID}','${m.id}')">授权</button>
          </div></td></tr>`).join("") + `</table>`
        : `<p class="muted">暂无成员</p>`}</div>
    <div class="card"><h3>角色与权限矩阵</h3>
      ${(roles.items || []).map(r => `<p><b>${esc(r.code)}</b>（${esc(r.name || "")}）：${(r.permissions || []).length} 项权限</p>`).join("")}</div>`;
}
async function inviteMember() {
  try {
    await POST(`/api/v1/tenants/${state.me.tenant_id}/members`, {
      username: val("tm-user"), password: val("tm-pass"), nickname: val("tm-nick"),
    });
    toast("成员已加入"); go("team");
  } catch (e) { toast(e.message, true); }
}
async function grantRole(tid, uid) {
  try { await POST(`/api/v1/tenants/${tid}/members/${uid}/roles`, { role: val(`role-${uid}`) }); toast("已授权"); go("team"); }
  catch (e) { toast(e.message, true); }
}

/* ---------- 系统 ---------- */
async function renderSystem(c) {
  const jobs = await GET("/api/v1/jobs").catch(() => ({ jobs: [] }));
  const tls = await GET("/api/v1/fingerprint/tls-baselines").catch(() => null);
  c.innerHTML = `
    <div class="card"><h3>定时任务（自动化同样落审计；手动触发立即执行一遍）</h3>
      <p>${(jobs.jobs || []).map(j => badge(j)).join(" ")}</p>
      <button class="primary" onclick="runJobs()">立即执行全部任务</button>
      <pre class="json" id="sys-out"></pre></div>
    ${tls ? `<div class="card"><h3>TLS 指纹基线（uTLS 实算，${tls.computed ? "已回写" : "未计算"}）</h3>
      <table><tr><th>内核</th><th>JA3 目标</th><th>JA3 实算</th><th>一致性</th><th>来源</th></tr>` +
      (tls.items || []).map(b => `<tr><td>${esc(b.kernel)}</td>
        <td class="small">${esc((b.ja3 || "").slice(0, 32))}…</td>
        <td class="small">${esc((b.computed_ja3 || b.ja3 || "").slice(0, 32))}…</td>
        <td>${b.computed ? badge("approved") : badge("pending")}</td><td>${esc(b.source || "-")}</td></tr>`).join("") + `</table></div>` : ""}
    <div class="card"><h3>关于</h3>
      <p class="muted small">后端 Go · 多租户行级隔离（PostgreSQL RLS）· 事件总线事务发件箱 · 审计哈希链 · 五层权限 · 信封加密。
      全部 API 见 <a href="/openapi.json" target="_blank">/openapi.json</a>。</p></div>`;
}
async function runJobs() {
  try {
    const r = await POST("/api/v1/jobs/run");
    document.getElementById("sys-out").textContent = (r.results || [])
      .map(x => `${x.job} [${x.tenant_id}] scanned=${x.scanned} affected=${x.affected} ${x.err ? "ERROR:" + x.err : ""}`).join("\n");
    toast("任务已执行");
  } catch (e) { toast(e.message, true); }
}

/* ---------- 工具 ---------- */
function val(id) { const el = document.getElementById(id); return el ? el.value.trim() : ""; }

const RENDERERS = {
  dashboard: renderDashboard, shops: renderShops, approvals: renderApprovals,
  devices: renderDevices, security: renderSecurity, audit: renderAudit,
  notifications: renderNotifications, team: renderTeam, system: renderSystem,
};

boot();
