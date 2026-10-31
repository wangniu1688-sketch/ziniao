package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 账号隔离实测站点（默认关闭）——用来证明：同一台电脑上的两个店铺环境，
// 在同域名同路径下互相看不到对方的登录态（Cookie / localStorage 各自独立）。
//
// 为什么需要它：JD/亚马逊这类平台无法在 Seller Central 里给你做自动化验证，
// 而"看窗口标题不同"并不能证明"账号不串号"。这里的判定依据是浏览器真实带上的 Cookie：
// 若两个环境共用存储，第二个环境必然带上第一个环境写下的 Cookie → 实测立刻穿帮。
//
// 红线：
//   - 仅当 cfg.DemoSite(-demo-site) 显式开启时挂载，生产不得开启；
//   - 不接触任何业务数据，报告只存在内存里。

// DemoReport 一次页面加载时该环境真实持有的登录态。
type DemoReport struct {
	ReportID   string `json:"report_id"`
	CookieUser string `json:"cookie_user"` // 服务端读到的 Cookie（HttpOnly，页面 JS 取不到）
	LSUser     string `json:"ls_user"`     // 页面 JS 从 localStorage 读到的账号
	UA         string `json:"ua"`
	// UACH 浏览器侧 UA-CH（navigator.userAgentData）——必须与 UA 的 Chrome 版本同源，
	// 若这里仍是 WebView2 真实内核版本则暴露「UA 被改过」
	UACH string `json:"uach"`
	// SecCHUA 服务端收到的 sec-ch-ua 请求头（HTTP 层，JS 改不了，只能靠代理层改写）
	SecCHUA  string `json:"sec_ch_ua"`
	Baseline string `json:"baseline"`
	At       string `json:"at"`
}

// DemoStore 报告暂存（进程内存，demo 专用）。
type DemoStore struct {
	mu      sync.Mutex
	reports map[string]DemoReport
}

func newDemoStore() *DemoStore {
	return &DemoStore{reports: map[string]DemoReport{}}
}

func (d *DemoStore) put(r DemoReport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reports[r.ReportID] = r
}

func (d *DemoStore) list() []DemoReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DemoReport, 0, len(d.reports))
	for _, r := range d.reports {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReportID < out[j].ReportID })
	return out
}

// demoRoutes 挂载 demo 站点（仅测试用）。
func demoRoutes(m *http.ServeMux, store *DemoStore) {
	const cookieName = "demo_shop_user"
	m.HandleFunc("GET /demo/shop", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if as := q.Get("as"); as != "" {
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: as, Path: "/demo", MaxAge: 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
		}
		if q.Get("logout") == "1" {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/demo", MaxAge: -1, HttpOnly: true})
		}
		var cookieUser string
		if c, err := r.Cookie(cookieName); err == nil {
			cookieUser = c.Value
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(demoShopPage(cookieUser)))
	})
	m.HandleFunc("GET /demo/clear", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/demo", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, "/demo/shop?report="+r.URL.Query().Get("report"), http.StatusSeeOther)
	})
	m.HandleFunc("POST /demo/report", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReportID string `json:"report_id"`
			LSUser   string `json:"ls_user"`
			Baseline string `json:"baseline"`
			UACH     string `json:"uach"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var cookieUser string
		if c, err := r.Cookie(cookieName); err == nil {
			cookieUser = c.Value
		}
		store.put(DemoReport{
			ReportID: body.ReportID, CookieUser: cookieUser, LSUser: body.LSUser,
			UA: r.UserAgent(), UACH: body.UACH, SecCHUA: r.Header.Get("sec-ch-ua"),
			Baseline: body.Baseline,
			At:       time.Now().UTC().Format(time.RFC3339),
		})
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("GET /demo/reports", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reports": store.list()})
	})
}

// demoShopPage 实测页：把 localStorage 与服务端 Cookie 的账号都显示出来，并自动上报。
// ls 的写入时机：只要 URL 带了 as=，页面就把账号写进 localStorage（模拟平台记住本地设备）。
func demoShopPage(cookieUser string) string {
	cu := "null"
	if cookieUser != "" {
		cu = strconv.Quote(cookieUser)
	}
	page := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>账号隔离实测</title>
<style>
 body{font:15px/1.7 "Segoe UI",system-ui;margin:0;background:#0f1420;color:#e8eef7}
 .wrap{max-width:760px;margin:40px auto;padding:28px 32px;background:#161d2c;border-radius:14px}
 h1{font-size:20px;margin:0 0 4px}
 .sub{color:#8fa3c0;font-size:13px;margin-bottom:18px}
 table{width:100%;border-collapse:collapse;margin:14px 0}
 td,th{padding:9px 10px;border-bottom:1px solid #2a3650}
 th{text-align:left;color:#8fa3c0;font-weight:500;width:150px}
 code{background:#0d1220;padding:2px 7px;border-radius:5px;color:#7fd2ff}
 .ok{color:#4ade80;font-weight:600}
 .bad{color:#f87171;font-weight:600}
 a.btn{display:inline-block;margin:6px 6px 0 0;padding:8px 14px;background:#2563eb;color:#fff;border-radius:8px;text-decoration:none;font-size:14px}
</style></head><body><div class="wrap">
<h1>账号隔离实测站点</h1>
<div class="sub">同一域名 / 同一路径下，检验各环境是否互不串号</div>
<table>
<tr><th>服务端 Cookie</th><td><code id="cu"></code></td></tr>
<tr><th>localStorage</th><td><code id="ls"></code></td></tr>
<tr><th>UA</th><td><code id="ua"></code></td></tr>
<tr><th>注入基线</th><td><code id="bl"></code></td></tr>
<tr><th>报告 ID</th><td><code id="rid"></code></td></tr>
</table>
<div id="acts"></div>
<script>window.__COOKIE_USER__ = <PLACEHOLDER_COOKIE>;</script>
<script>
var q=new URLSearchParams(location.search);
var as=q.get('as');
var rid=q.get('report')||'';
if(as){try{localStorage.setItem('demo_shop_account',as);}catch(e){}}
var lsUser='';
try{lsUser=localStorage.getItem('demo_shop_account')||'';}catch(e){}
document.getElementById('cu').textContent=window.__COOKIE_USER__||'（无）';
document.getElementById('ls').textContent=lsUser||'（无）';
document.getElementById('ua').textContent=navigator.userAgent.slice(0,72);
document.getElementById('bl').textContent=(window.__ZINIAO_BASELINE__||'（未注入）').slice(0,16);
document.getElementById('rid').textContent=rid||'（无）';
var acts=document.getElementById('acts');
if(q.get('logout')!=='1'){
  acts.innerHTML='<a class="btn" href="/demo/shop?as=seller_a%40amazon.com&report='+encodeURIComponent(rid)+'">以 seller_a@amazon.com 登录</a>'+
    '<a class="btn" href="/demo/shop?as=seller_b%40ebay.co.uk&report='+encodeURIComponent(rid)+'">以 seller_b@ebay.co.uk 登录</a>'+
    '<a class="btn" href="/demo/shop?report='+encodeURIComponent(rid)+'">仅刷新（不登录）</a>'+
    '<a class="btn" href="/demo/clear?report='+encodeURIComponent(rid)+'">退出</a>';
}
fetch('/demo/report',{method:'POST',headers:{'Content-Type':'application/json'},
  body:JSON.stringify({report_id:rid,ls_user:lsUser,baseline:window.__ZINIAO_BASELINE__||'',
    uach:(navigator.userAgentData&&navigator.userAgentData.toJSON)?JSON.stringify(navigator.userAgentData.toJSON()):'(none)'})});
</script></div></body></html>`
	// 服务端把 HttpOnly Cookie 里的账号注入页面（页面 JS 取不到 HttpOnly cookie，
	// 这正是要验证的东西：每个环境是否随身带着只属于自己的登录态）
	return strings.Replace(page, "<PLACEHOLDER_COOKIE>", cu, 1)
}
