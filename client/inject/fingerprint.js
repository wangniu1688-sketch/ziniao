/**
 * 指纹注入脚本（骨架 / 与服务端 fingerprint.BuildInjectionPlan 对齐）。
 *
 * 三条铁律：
 *   1. 必须在文档创建前执行（CDP Page.addScriptToEvaluateOnNewDocument 或
 *      Tauri initialization_script）。页面已读取 navigator 后再改就是自相矛盾。
 *   2. 注入值必须与服务端下发的画像**逐字一致**（含 UA 大小写与顺序），
 *      禁止客户端"随机化"——随机化 = 与画像库脱钩 = 更容易被识别。
 *   3. 注入只是第一层（JS 层）；CDP 层覆盖与前置代理的 TLS 指纹由 Go 侧完成，
 *      两者必须指向同一内核基线（L2）。
 *
 * @param {Object} plan 服务端下发的注入计划
 * @param {string} plan.env_id        环境 ID
 * @param {string} plan.baseline_hash 画像基线哈希（离线启动校验用）
 * @param {number} plan.kernel_major  内核主版本（决定 UA 与 TLS）
 * @param {Object} plan.profile       画像
 */
(function injectFingerprint(plan) {
  'use strict';
  if (!plan || !plan.profile) return;
  var p = plan.profile;
  var nav = navigator;

  // ---- 第一层：JS 属性覆盖（可检测性 low） ----
  function def(obj, prop, value) {
    try {
      Object.defineProperty(obj, prop, { get: function () { return value; }, configurable: true });
    } catch (_) { /* 只读属性交给 CDP 层覆盖 */ }
  }

  if (p.ua) def(nav, 'userAgent', p.ua);
  if (p.userAgentData) def(nav, 'userAgentData', p.userAgentData);
  if (p.os && p.os.platform) def(nav, 'platform', p.os.platform);
  if (p.cpu && p.cpu.cores) def(nav, 'hardwareConcurrency', p.cpu.cores);
  if (p.memory_gb) def(nav, 'deviceMemory', p.memory_gb);
  if (p.languages) def(nav, 'languages', Object.freeze(p.languages.slice()));
  if (p.languages && p.languages.length) def(nav, 'language', p.languages[0]);

  // ---- 第二层：Canvas / WebGL / Audio 噪声（中可检测性，扰动幅度必须与画像绑定） ----
  var seed = hashSeed(plan.baseline_hash || p.id || '');

  patchCanvas(seed);
  patchWebGL(p, seed);
  patchAudio(seed);
  patchFonts(p);

  // ---- 第三层：屏蔽自动化痕迹 ----
  delete AutomationFlags();

  function hashSeed(s) {
    var h = 2166136261;
    for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = (h * 16777619) >>> 0; }
    return h >>> 0;
  }

  function patchCanvas(seed) {
    var orig = HTMLCanvasElement.prototype.toDataURL;
    HTMLCanvasElement.prototype.toDataURL = function () {
      var res = orig.apply(this, arguments);
      // 与基线绑定的确定性扰动（禁止 Math.random）
      return res.slice(0, res.length - 1) + ((seed % 36).toString(36));
    };
  }

  function patchWebGL(profile, seed) {
    if (!profile.gpu) return;
    var getParam = WebGLRenderingContext.prototype.getParameter;
    WebGLRenderingContext.prototype.getParameter = function (param) {
      if (param === 37445) return profile.gpu.vendor;   // UNMASKED_VENDOR_WEBGL
      if (param === 37446) return profile.gpu.renderer; // UNMASKED_RENDERER_WEBGL
      return getParam.apply(this, arguments);
    };
    try {
      def(HTMLCanvasElement.prototype.getContext('webgl') || {}, 'drawingBufferWidth', seed % 4096);
    } catch (_) {}
  }

  function patchAudio(seed) {
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return;
    var create = Ctx.prototype.createDynamicsCompressor;
    Ctx.prototype.createDynamicsCompressor = function () {
      var node = create.apply(this, arguments);
      try { node.reduction.value = (seed % 100) / 1000; } catch (_) {}
      return node;
    };
  }

  function patchFonts(profile) {
    if (!profile.fonts) return;
    var style = document.createElement('style');
    style.textContent = profile.fonts.map(function (f) {
      return '@font-face{font-family:"' + f + '";src:local("' + f + '")}';
    }).join('\n');
    (document.head || document.documentElement).appendChild(style);
  }

  function delete AutomationFlags() {
    var flags = ['webdriver', '__playwright', '__puppeteer_evaluation_script__', 'callPhantom', '_phantom'];
    flags.forEach(function (f) {
      try { delete window[f]; def(window, f, undefined); } catch (_) {}
    });
    var sel = ['cdc_', '$cdc_', '__webdriver_script_fn'];
    sel.forEach(function (k) {
      Object.keys(window).forEach(function (key) {
        if (key.indexOf(k) === 0) { try { delete window[key]; } catch (_) {} }
      });
    });
  }

  window.__ZINIAO_BASELINE__ = plan.baseline_hash;
})(window.__ZINIAO_PLAN__);
