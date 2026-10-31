// 客户端骨架：演示「登录 → 拉环境 → 启动 → 自检」的调用链。
// 红线：所有权限裁决都在服务端，前端只渲染结果（L3/L6）。
const BASE = 'http://127.0.0.1:8080';
let token = '';

async function api(method, path, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: Object.assign(
      { 'Content-Type': 'application/json' },
      token ? { Authorization: `Bearer ${token}` } : {}
    ),
    body: body ? JSON.stringify(body) : undefined
  });
  const text = await res.text();
  try { return { status: res.status, body: JSON.parse(text) }; } catch (_) { return { status: res.status, body: text }; }
}

function out(data) { document.getElementById('out').textContent = JSON.stringify(data, null, 2); }

document.getElementById('login').onclick = async () => {
  const r = await api('POST', '/api/v1/auth/login', {
    username: document.getElementById('username').value,
    password: document.getElementById('password').value,
    terminal: { ip: '127.0.0.1', user_agent: navigator.userAgent, os: navigator.platform }
  });
  token = r.body.token || '';
  document.getElementById('loginState').textContent = token ? '已登录' : `失败 ${r.status}`;
};

document.getElementById('loadShops').onclick = async () => {
  const r = await api('GET', '/api/v1/shops');
  const sel = document.getElementById('shops');
  sel.innerHTML = (r.body.items || []).map((s) => `<option value="${s.id}">${s.name}</option>`).join('');
  out(r.body);
};

document.getElementById('createEnv').onclick = async () => {
  const shopId = document.getElementById('shops').value;
  const r = await api('POST', `/api/v1/shops/${shopId}/environment`, { country: 'US' });
  // 注入计划中的 script 必须在文档创建前执行（见 inject/fingerprint.js）
  out(r.body);
};

document.getElementById('checkEnv').onclick = async () => {
  const shopId = document.getElementById('shops').value;
  const get = await api('GET', `/api/v1/shops/${shopId}/environment`);
  const observed = collectObserved(get.body);
  const r = await api('POST', `/api/v1/shops/${shopId}/environment/check`, observed);
  out(r.body);
};

// 采集观测值：只采集，不下判断（判断在服务端）
function collectObserved(env) {
  return {
    baseline_hash: env.baseline_hash,
    ua: navigator.userAgent,
    platform: navigator.platform,
    cores: String(navigator.hardwareConcurrency || 0),
    dpr: String(window.devicePixelRatio || 1),
    languages: (navigator.languages || []).join(','),
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone || ''
  };
}
