# M1 前置设计（三）：BrowserManager 状态机与流程

> 状态：**待人工确认**。确认前不写业务代码。
> 配套：`docs/design-m1-schema.md`、`docs/design-m1-api.md`。
> 已拍板、本文不再讨论的两条：**方案 A**（Go 内每环境本地 TLSProxy，Chrome 连 `127.0.0.1:<动态端口>`，上游口令只存 Go 内存）、**CDP 端口 `--remote-debugging-port=0` + 读 `DevToolsActivePort`**。

---

## 1. 组件与文件布局（新增 `browser/` 包）

```
browser/
  manager.go      Manager：对外编排入口（Launch / Stop / Status / Reconcile），持有注册表与锁
  registry.go     内存注册表 + data/runtime/registry.json 读写（原子落盘）
  chrome.go       ChromeLocator：探测本机 Chrome/Edge（版本 + 路径）
  ports.go        动态端口分配（本地 TLSProxy：持 listener；CDP：OS 分配后读取）
  argv.go         构造 Chrome argv（硬保证不含口令）
  cdp.go          最小 CDP 客户端（WebSocket + 方法封装）
  inject.go       document_start 注入（Page.addScriptToEvaluateOnNewDocument）
  reconcile.go    启动器重启后的残留进程对账
  proc.go         跨平台进程查询（Windows：CIM 快照；macOS/Linux：ps）
```

**不新建**：不 fork Chromium、不引入 Electron/Tauri、不做 ziniao-proxy sidecar 子进程、不做标签栏/地址栏/书签栏（用 Chrome 原生 UI）。

依赖装配：`gateway.Deps` 增加 `Browser *browser.Manager`；`cmd/server/main.go` 里构造并 `SetIPPool` 之后注入 `EnvService`（`EnvService.SetBrowser`）。

---

## 2. 核心数据结构

```go
// RuntimeEntry 本机进程态。权威业务状态永远在 PostgreSQL environments.status；
// 这里只是「本机这一刻」的缓存，进程崩溃/换机器都可能让它与库不一致。
type RuntimeEntry struct {
    EnvID           string    `json:"env_id"`
    ShopID          string    `json:"shop_id"`
    TenantID        string    `json:"tenant_id"`
    PID             int       `json:"pid"`
    CDPPort         int       `json:"cdp_port"`
    LocalProxyPort  int       `json:"local_proxy_port"`
    ProfileDir      string    `json:"profile_dir"`
    ChromePath      string    `json:"chrome_path"`
    ExitMode        string    `json:"exit_mode"`   // manual|pool|direct
    StartedAt       time.Time `json:"started_at"`
    ProcState       string    `json:"proc_state"`  // starting|running|stopping|unknown
}

// ProcState 与业务状态严格分离（批复 3.2）：
//   created / running / idle / stopped  →  PostgreSQL（业务事实）
//   starting / running / stopping / unknown → 本机进程态（易失，可重建）
```

`Manager`：

```go
type Manager struct {
    mu        sync.RWMutex                 // 保护下面的 map 结构
    entries   map[string]*RuntimeEntry     // envID → 运行时
    perEnv    map[string]*sync.Mutex       // envID → 单环境串行化（启动/停止互斥）
    dataDir   string                       // 绝对路径
    caDir     string
    chrome    string                       // ZINIAO_CHROME_PATH 覆盖，否则自动探测
    envs      *shop.EnvService             // 复用其权限/数据级收口/出口分配
    log       *slog.Logger
    // 超时与策略
    cdpTimeout      time.Duration          // 默认 20s
    stopGrace       time.Duration          // 默认 3s（优雅关页）后 Browser.close
    orphanPolicy    string                 // close|keep，默认 close
}
```

---

## 3. 启动时序（M3）

**设计要点：绝不在数据库事务里等浏览器。**
现有 `EnvService.Launch()` 是「分配出口 → 开事务写 running → 返回启动单」，事务内做的是毫秒级写。CDP 连通要等 10–20 秒；如果把它塞进事务（mem 驱动是全局串行锁、pg 是长事务持锁），一次卡顿会拖垮整库。因此**只调整事务边界，不改业务逻辑**：

- `EnvService.Launch()`：保留权限裁决、数据级收口、注入脚本生成、出口分配（`Allocator.AllocateEx`）与出口轮换告警，**移除事务里的「写 running」**；返回 `LaunchSpec`（含 `Upstream`，但**不写进任何响应/审计**）；
- 新增 `EnvService.MarkRunning(ctx, MarkRunningInput)`：`WithTx` 改 `status=running` + `last_started_at` + emit `env.launched`（**这就是 running 的唯一提交点**）；
- 新增 `EnvService.MarkLaunchFailed(ctx, MarkLaunchFailedInput)`：仅在状态为 `created|idle` 时复位（**绝不覆盖 `running`/`stopped`**）+ emit `env.launch_failed`。

```
┌── Stage 0 ── 入参校验与并发收口
│  0.1 取 perEnv[envID] 互斥锁（同一环境并发启动排队；不同环境互不影响）
│  0.2 注册表已存在该 env → 409 conflict「该环境已在本机运行」
│
├─ Stage 1 ── 业务侧启动单（复用）
│  1.1 EnvService.Launch()：权限（env:launch）、数据级收口、脚本、出口分配
│      - exit_mode=manual：upstream ← proxies 行（信封解密，明文只留内存）
│      - exit_mode=pool  ：upstream ← Allocator.AllocateEx()（现有链路不变）
│      - exit_mode=direct：upstream ← ProxyConfig{Protocol: direct}
│  1.2 失败 → 直接返回（未产生任何副作用，无需回滚）
│
├─ Stage 2 ── 预检（失败即回滚 Stage 1 的出口分配？→ 不回滚，见下方注）
│  2.1 ChromeLocator 探测（ZINIAO_CHROME_PATH 优先）
│  2.2 MkdirAll(data/profiles/<env-id>)
│  2.3 CA 就绪检查（ca_ready=false → 直接失败，不启动）
│  2.4 失败 → goto Rollback(2)
│
├─ Stage 3 ── 本地 TLSProxy（端口零竞态）
│  3.1 ln, _ := net.Listen("tcp", "127.0.0.1:0")   ← 拿到即持有，不关闭再重绑
│  3.2 p := network.NewTLSProxy(TLSProxyConfig{Upstream, HelloID, CADir, UA, ChromeMajor, ...})
│  3.3 go p.ServeListener(ctx, ln)                  ← 复用现有 uTLS/JA3/JA4 出站链路
│  3.4 轮询 http://127.0.0.1:<port>/healthz 至就绪（超时 5s）
│  3.5 失败 → goto Rollback(3)
│
├─ Stage 4 ── 拉起 Chrome
│  4.0 清 profile 残留（见 §4.5）：DevToolsActivePort / SingletonLock / SingletonSocket / SingletonCookie
│  4.1 argv := buildArgv(...)（见 §6，硬保证不含口令）
│  4.2 cmd := exec.Command(chromePath, argv...)；cmd.Start()
│  4.3 记录 PID + 时间基准 launchMark（只有晚于它的端口文件才算本轮写的）
│  4.4 失败 → goto Rollback(4)（收进程 → 等退出 → 清残留）
│
├─ Stage 5 ── 取 CDP 端口（方案 A）
│  5.1 轮询 <profile>/DevToolsActivePort（默认 20s，可配 ZINIAO_CDP_TIMEOUT）
│      文件格式：第 1 行 = 端口，第 2 行 = /devtools/browser/<uuid>
│      只认本轮写出的文件（mtime >= launchMark）；更早的一律按上一轮遗留处理
│      ——唯一例外：旧端口现在确实连得上（说明服务该 profile 的是活实例）
│  5.2 进程已退出却没写出端口 → 立即失败（多半被残留单实例锁顶掉）
│  5.3 超时 → goto Rollback(5)
│
├─ Stage 6 ── CDP 连接 + 注入
│  6.1 ws://127.0.0.1:<cdpPort>/devtools/browser/<uuid>
│  6.2 Target.setDiscoverTargets{true}
│  6.3 对每个 page target：Target.attachToTarget{flatten:true} → Page.enable
│      → Page.addScriptToEvaluateOnNewDocument{source: env.inject_script}
│  6.4 对启动瞬间已存在的 target 补注入一次（并记录日志）
│  6.5 失败（连接不上 / 注入报错）→ goto Rollback(6)
│
├─ Stage 7 ── 打开页面
│  7.1 Page.navigate{url: openURL ?: env.start_url ?: "about:blank"}
│  7.2 失败不回滚（页面打不开不等于环境不可用，记 warn）
│
├─ Stage 8 ── 提交业务状态（running 的唯一提交点）
│  8.1 EnvService.MarkRunning()：WithTx(status=running, last_started_at=now) + env.launched
│  8.2 失败 → goto Rollback(8)（关闭浏览器，状态回 idle）
│
└─ Stage 9 ── 登记运行时
   9.1 entries[envID] = &RuntimeEntry{...}; ProcState=running
   9.2 原子落盘 data/runtime/registry.json（写 .tmp + rename）
   9.3 释放 perEnv 锁，返回 {env_id, status:"running", pid, cdp_port, local_proxy_port, ...}
```

**关于 Stage 2 的出口分配不回滚**：`Allocator` 的绑定语义是「重启不换 IP」（`egress_bindings` 存在的意义）。启动失败就把 IP 还回去，意味着下次启动换个 IP——对本就脆弱的账号是额外的关联风险。因此：**启动失败不释放出口绑定**（与 `StopEnv` 同口径），只在 `Deactivate`/`DeleteEnv` 时释放。失败只复位状态、释放端口与进程。

### 回滚矩阵

| 失败 Stage | 已产生的副作用 | 回滚动作 | 最终业务状态 | 事件 |
|---|---|---|---|---|
| 1（权限/出口分配） | 无（或仅分配器内部日志） | 无 | 保持原状 | 无 |
| 2（Chrome 缺失/CA 缺失/目录建不了） | 出口绑定（保留） | 无额外清理 | 原状（created/idle） | `env.launch_failed` |
| 3（本地代理起不来） | 出口绑定 | 关 listener | 原状 | `env.launch_failed` |
| 4（Chrome Start 失败） | 出口绑定、listener | 关 listener、清 profile 残留 | 原状 | `env.launch_failed` |
| 5（DevToolsActivePort 超时） | + Chrome 进程 | kill 进程 → **等它真退出** → 清 profile 残留、关 listener | 原状 | `env.launch_failed` |
| 6（CDP 连不上/注入失败） | + Chrome 进程 | 断 WS、kill 进程 → 等退出 → 清 profile 残留、关 listener | 原状 | `env.launch_failed` |
| 8（MarkRunning 事务失败） | + Chrome 进程 + CDP | 断 WS、优雅关 Chrome、关 listener、清 profile 残留 | 原状（**绝不留在 running**） | `env.launch_failed` |

**硬保证**：任何 stage 失败后，数据库里都不会出现「status=running 但本机没有进程」的悬空态；`env.launch_failed` 事件里 `data` 只含 `env_id / stage / reason`，**不含** upstream 地址、端口、账号、口令。

---

## 4. 停止时序（M3）

```
1. 取 perEnv[envID] 锁
2. CDP：断开 WebSocket（先断控制通道，避免关闭过程中回调打到已失效连接）
3. Chrome：
   3.1 先对每个 target 发 Page.close（优雅关页，给 3s）
   3.2 超时/失败 → Browser.close
   3.3 cmd.Wait()（宽限期 StopGrace）；再超时 → Process.Kill()（Windows: TerminateProcess）
       → **必须再 Wait 一次**（stopForceWait=5s）确认进程真退出，不能发完 kill 就返回
   3.4 进程确认退出后清 profile 残留（DevToolsActivePort / Singleton*）
       ——没确认退出就不清：清了也会被正在退出的 Chrome 写回
4. 本地 TLSProxy：cancel(ctx) 触发 srv.Close()；close(listener)
5. 端口登记：从 entries 删除、注册表原子落盘
6. 业务状态：调用 EnvService.StopEnv()（复用现有幂等 + 审计 + 不动出口绑定的逻辑）
7. 最后更新 updated_at（由 StopEnv 的 Update 完成；last_started_at 只在启动成功时写，停止不改）
8. 释放 perEnv 锁
```

> 复用而非重写：停止的**业务语义**（幂等、running→idle、`env.stopped` 事件、不释放出口绑定）全部走既有的 `EnvService.StopEnv`，BrowserManager 只负责进程与端口的收尾。

### 4.5 profile 残留文件（重启闭环的硬要求）

Chrome 在 `--user-data-dir` 根目录写的这 4 个文件**只属于某一次进程实例**：

| 文件 | 作用 |
|---|---|
| `DevToolsActivePort` | `--remote-debugging-port=0` 时回写的实际 CDP 端口 |
| `SingletonLock` / `SingletonSocket` / `SingletonCookie` | 单实例锁三件套 |

进程正常退出时 Chrome 自己会删；**被强杀 / 崩溃 / 运营在任务管理器里结束进程时一定留在盘上**。留着的后果（M3 人工验收实测）：下次启动先读到上一轮的旧端口，而新 Chrome 又因为看到残留单实例锁，把启动请求转发给一个已经不存在的旧实例后自己退出 —— 症状是「环境启动失败（阶段 6）：CDP 连接失败: dial tcp 127.0.0.1:\<旧端口\>」，并且手动杀光 chrome.exe 也修不好（要读的文件还在盘上）。

清理时机（四处，缺一不可）：

1. **启动前（Stage 4.0）**：拉起新 Chrome 之前先清一遍，并记 `launchMark` 作为「本轮」时间基准；
2. **停止后（§4 步骤 3.4）**：进程确认退出之后清；
3. **启动失败回滚**：`rb` 里的顺序固定为「Terminate → 等退出 → 清残留」（反了会被正在退出的进程写回）；
4. **崩溃 / 窗口外关闭**：`watchProcess` 看到进程退出后清（与停止流程并发时以「文件已不在」视为成功，不报错误）。

> 启动器重启后的对账（§5 b 档，进程已没的条目）同样清一遍——那是崩溃留下的最典型残留。

---

## 5. 启动器重启后的残留进程对账（reconcile）

时机：`browser.New()` 内、**对外提供服务之前**完成（此时还没有任何 launch 请求，天然排除「对账关掉正在启动的进程」的竞态）。

```
1. 读 data/runtime/registry.json
2. 抓一次本机进程快照（Windows：CIM Win32_Process 过滤 chrome.exe/msedge.exe，取 CommandLine；
   macOS/Linux：ps -eo pid,args 过滤）
3. 判定「是不是我们起的」：命令行同时含
     --user-data-dir=<dataDir>/profiles/<envID>   且   --remote-debugging-port=0
4. 三种结果：
   a) 注册表有 + 进程活      → 重挂：重读 DevToolsActivePort，ProcState=running
                                若库里 status != running → 库是权威：关闭进程 + 复位，事件 env.reconciled(action=closed)
   b) 注册表有 + 进程没了    → 删注册表项 + 清 profile 残留（§4.5）；
                                若库里 status == running → StopEnv 复位 idle，事件 env.reconciled(action=reset)
   c) 进程有 + 注册表没有    → 孤儿（上次崩溃遗留）
                                orphanPolicy=close（默认）→ 关闭 + 事件 env.reconciled(action=closed)
                                orphanPolicy=keep        → 只登记 + warn 日志
5. 原子落盘修正后的注册表；打印对账摘要（ scanned / relinked / closed / reset ）
```

- 进程命令行采集只在对账这一次执行（非热路径），结果缓存到内存；
- 对账失败（CIM 不可用等）不阻塞启动，只 warn + 在 `system/info.capabilities.reconcile` 标 false。

---

## 6. Chrome argv（硬保证不含口令）

```go
func buildArgv(cfg argvCfg) []string {
    return []string{
        "--user-data-dir=" + cfg.ProfileDir,          // data/profiles/<env-id>
        "--remote-debugging-port=0",                  // 方案 A：OS 分配，读 DevToolsActivePort
        "--remote-allow-origins=http://127.0.0.1:*",  // 仅本地，不放行任意来源
        "--proxy-server=http://127.0.0.1:" + itoa(cfg.LocalProxyPort),
        "--proxy-bypass-list=<-loopback>",            // 回环不过代理，避免自连接
        "--no-first-run", "--no-default-browser-check", "--disable-sync",
        "--disable-background-networking",            // 减少与 Google 的无关连接（污染出口画像）
        "--disable-component-update", "--disable-domain-reliability",
        "--metrics-recording-only", "--disable-breakpad",
        "--window-size=1280,860",
        "--user-agent=" + cfg.UA,                     // UA 不是机密，允许出现在 argv
        // 指纹注入走 CDP（Stage 6），不通过命令行传递脚本正文
        cfg.OpenURL,                                  // 末尾位置参数
    }
}
```

**明令禁止**：
- 禁止 `--proxy-server=http://user:pass@host:port`（口令进 argv = 进进程列表 / 进日志 / 进审计）；
- 禁止把上游用户名/口令写进任何命令行参数、环境变量文件或前端响应；
- 注入脚本正文只经 CDP 传递（不落 argv、不落响应）。

**自检**：单测 `TestArgvNeverContainsUpstreamSecret` —— 构造含口令的 upstream，断言生成的 argv 中不含口令与用户名。这条断言是 M3 硬验收项「argv 不含上游代理口令」的自动化证据。

**可选（默认关闭）**：`--load-extension=<dataDir>/extensions/sidepanel`。默认关闭，因为 Chrome 加载未打包扩展会弹「请停用开发者模式扩展」提示，干扰 M3 验收；目录按 2.4 节先建好，开关 `ZINIAO_SIDEPANEL=1` 打开。侧栏扩展**不得注入业务页面、不得采集页面内容**（批复 3.3）。

---

## 7. 本地 TLSProxy 与 MITM 的两个前置

1. **CA 必须被本机 Chrome 信任**。本地代理是 MITM 架构（`network/utlsproxy.go` 的 `handleConnect` 用本地 CA 动态签发目标证书），CA 不进信任库则所有 HTTPS 站点报证书错误。复用 `network` 包已有的 CA 生成/装载能力（`cmd/ziniao-proxy -install-ca` 走的就是同一条路径），落到 `data/ca`，`system/info.data.ca_ready` 对外暴露；未就绪时 launch 直接失败并给出明确文案。
2. **`network.TLSProxy.Serve` 需新增一个接受 listener 的变体**：

```go
// Serve 保持现状（内部 Listen 后转调 ServeListener），不破坏 cmd/ziniao-proxy。
func (p *TLSProxy) Serve(ctx context.Context, listen string) error {
    ln, err := net.Listen("tcp", listen)
    if err != nil { return err }
    return p.ServeListener(ctx, ln)   // 新增
}
func (p *TLSProxy) ServeListener(ctx context.Context, ln net.Listener) error { /* 现有 handler 分流逻辑原样搬过来 */ }
```

这样本地代理端口也是「先拿到 listener 再交给 Serve」，与 CDP 的 `port=0` 一样**不存在「释放监听 → 绑定」竞态**。

---

## 8. 状态机图

```
                        ┌─────────────┐
   创建环境 ───────────▶│  created    │
                        └──────┬──────┘
                               │ Launch 成功（Stage 8 提交）
                               ▼
  ┌──────────────────────────────────────────────────────┐
  │                     running                          │◀──── 对账重挂 (a)
  │   进程态：starting → running → stopping → (removed)   │
  └───┬──────────────────────────┬───────────────────────┘
      │ Stop（优雅停止，§4）        │ 进程崩溃 / 重启对账 (b)
      │ → StopEnv（幂等，留出口）   │ → 复位 idle + env.reconciled
      ▼                          ▼
  ┌─────────────┐          ┌─────────────┐
  │    idle     │          │    idle     │
  └──────┬──────┘          └─────────────┘
         │ Deactivate（释放出口绑定）
         ▼
  ┌─────────────┐   Activate   ┌─────────────┐
  │   stopped   │─────────────▶│    idle     │
  └──────┬──────┘              └─────────────┘
         │ DeleteEnv（先收窗口 → 删行 → 清 profile）
         ▼
       （物理删除）

进程态（本机、易失、不落库）：
  starting ──CDP 连通──▶ running ──停止开始──▶ stopping ──进程退出──▶ (从注册表移除)
     │                                                                     ▲
     └──────────── 任一 stage 失败 → 回滚 → 移除 ──────────────────────────┘
```

**两条铁律**：
1. `running` 只在 Stage 8（Chrome 起来 + CDP 连通）之后提交，绝不在拉起 Chrome 前提交；
2. 进程态永远不写回数据库；业务状态的权威永远是 PostgreSQL。

---

## 9. 并发与锁边界

| 资源 | 保护方式 | 说明 |
|---|---|---|
| `entries` / `perEnv` map 结构 | `Manager.mu`（`sync.RWMutex`） | 读多写少；`Status`/`Runtime` 走 RLock |
| 单个环境的启动/停止互斥 | `perEnv[envID]`（`sync.Mutex`，在 `mu` 保护下 get-or-create） | 不同环境完全并发；同环境串行 |
| 端口分配 | **不加锁** | CDP 由 OS 分配；本地代理持 listener，OS 保证唯一 |
| 出口分配 | `network.Allocator` 自带大锁 | 现状不变，BrowserManager 只是调用方 |
| 注册表落盘 | `diskMu` + 临时文件 rename | 原子替换，避免半截 JSON |
| 对账 | 在 `New()` 内、服务对外之前完成 | 天然与 launch 互斥，不需要额外锁 |
| 事务 | `store.WithTx`（含 `store.WithTenant`） | 短事务，绝不包含任何等待浏览器的 I/O |

**多环境并发启动**：除 `perEnv` 外无全局串行点，N 个环境可同时启动（受 `Allocator` 自身锁与本机资源限制）。

---

## 10. 复用 vs 新增

| 能力 | 来源 | 说明 |
|---|---|---|
| 权限裁决 / 数据级收口 / 店铺访问校验 | **复用** `shop.EnvService.Launch`（`iam` + `checkShopAccess`） | 不动 |
| 注入脚本生成 | **复用** `fingerprint.InjectionScript` / `rebuildScript` | 不动 |
| 出口分配与绑定 | **复用** `network.Allocator.AllocateEx` + `Router` + Provider | 不动 |
| 出口轮换告警 | **复用** `security.Service.Raise` | 不动 |
| uTLS / JA3 / JA4 出站链路 | **复用** `network.NewTLSProxy`（新增 `ServeListener` 变体，Serve 逻辑原样） | 小改 |
| 本地 CA | **复用** `network` 的 CA 加载/安装（`data/ca`） | 不动 |
| 停止的业务语义 | **复用** `EnvService.StopEnv`（幂等 + 审计 + 不释放出口） | 不动 |
| 停用/启用/删除 | **复用** `Deactivate` / `Activate` / `DeleteEnv` | M3 删除前需先调 `Manager.Stop` |
| 事件与审计 | **复用** `bus.Emit` + `audit.Sink` | 只新增事件 type |
| Chrome 探测 / exec / argv | **新增** `browser/chrome.go`、`argv.go` | — |
| 动态端口 + listener 转交 | **新增** `browser/ports.go` + `network.ServeListener` | — |
| CDP 客户端 | **新增** `browser/cdp.go` | 见 §11 待决策 |
| CDP document_start 注入 | **新增** `browser/inject.go` | 替代原来的「客户端执行脚本」 |
| 注册表与落盘 | **新增** `browser/registry.go` | — |
| 残留进程对账 | **新增** `browser/reconcile.go`、`proc.go` | — |
| 状态提交点拆分 | **改造** `EnvService`：`Launch` 不写 running；新增 `MarkRunning` / `MarkLaunchFailed` | 仅调事务边界与时序 |
| 废弃 | `proxyPortFor(envID)` 的 `20000 + FNV32%20000` 静态哈希区间 | 直接删除 |

---

## 11. 待决策项 → 批复结论（2026-11-01，M3 编码前已拍板）

1. **CDP WebSocket 客户端选型**（项目当前零外部依赖，`go.mod` 只有 pgx 与 uTLS；本机模块缓存里没有 `gorilla/websocket`，也没有 `golang.org/x/net`）：
   - **方案 a（推荐）**：自研最小 RFC 6455 客户端（约 200 行：握手 + 帧编解码 + ping/pong），零新依赖，不受离线环境影响；
   - 方案 b：引入 `github.com/gorilla/websocket`（需联网下载，功能全但要新增依赖）。
   - 我倾向 **a**；请确认。
2. **孤儿进程策略默认值**：本文默认 `close`（启动器重启后把残留的我们自己的 Chrome 关掉并复位）。若运营希望「保留窗口不打扰」，可改 `keep`（只登记 + 告警）。
3. **侧栏扩展本期是否加载**：默认**不加载**（`ZINIAO_SIDEPANEL=0`），目录先建。`--load-extension` 会触发 Chrome 的开发者模式提示，影响 M3 观感验收。
4. **多开启动器实例**：本期按「不支持」设计——`registry.json` 加文件锁，第二个实例启动即报「已有启动器在运行」并只读展示，避免两个进程互相以为是孤儿。
5. **CA 安装方式**：沿用 `network` 现有能力装到**当前用户**信任库（不需要管理员权限）；若目标机器禁止改信任库，则需改为 Chrome 启动时带 `--ignore-certificate-errors`（**不推荐**，会让安全中心失去意义）——请确认是否接受「装 CA」作为 M3 前置步骤。
6. **注入补做策略**：Stage 6.4 对已存在 target 补注入时，默认**不 reload**（避免打断已打开页面）；M3 验收以新开 `fp-test.html` 为准。若要求「已开页面立即生效」，则需 `Page.reload`，请确认。

### 批复结论（本节为 2026-11-01 批复原文要点，编码以此为准）

| # | 拍板 |
|---|---|
| 1 | **CDP WebSocket 优先 `github.com/gorilla/websocket`**（先尝试 `go get`；HTTP 握手与 `/json/version`、`/json/list` 仍用标准库 `net/http`）。仅当离线模块缓存确实取不到时，才允许退化为自研最小 RFC6455 客户端，且必须满足：发送帧强制掩码、完整支持服务端分片帧重组、ping/pong 心跳、连接超时、发送并发互斥锁、正常/异常关闭，并配套覆盖握手/文本帧/分片重组/掩码/ping-pong/异常断开的单测；**未实现分片重组前不得发送大体积注入脚本** |
| 2 | 孤儿进程策略默认 **close**（启动器重启对账时关闭注册表匹配不上的残留 Chrome，结果写日志） |
| 3 | 侧边栏扩展 **M3 默认不加载**（不传 `--load-extension`）；`data/extensions/sidepanel/` 保留为可选开关（`ZINIAO_SIDEPANEL=1`）；指纹注入不依赖它，仍走 CDP `document_start` |
| 4 | **本期不支持多开**：`registry.json` 文件锁；第二实例只读打开并提示「启动器已在运行」，不允许重复拉起环境 |
| 5 | MITM CA 安装到**当前用户**信任库（不写系统级存储、不需要管理员权限），提供卸载入口；**禁止用 `--ignore-certificate-errors` 替代**；CA 未就绪阻断启动，`GET /api/v1/system/info` 返回 `ca_ready` |
| 6 | 已打开页面补注入**默认不 reload**；M3 验收以新开 `fp-test.html` 为准，新开 target 的 `document_start` 注入必须正常挂载 |

### 已确认的既有技术结论（不重写）

- `running` 状态拆出事务：`EnvService.Launch()` 只生成启动单，新增 `MarkRunning` / `MarkLaunchFailed`，**仅调整事务边界**；指纹生成、代理分配、出口绑定逻辑一律不动。现状 `shop/environment.go` 分配出口后立即写 `running` 的时序，按新方案后置到 CDP 连通成功之后。
- 本地代理端口零竞态：`network.TLSProxy` 增加 `ServeListener(ctx, ln)` 变体（已落地），`Serve` 退化为薄封装，`cmd/ziniao-proxy` 行为不变。
- CA 就绪作为 launch 前置条件；出口绑定**不因启动失败回滚**（仅在 `Deactivate` / `Delete` 时释放）。
- 现有 `/api/v1/shops/{id}/environment/launch` 响应体中的 `Upstream.Password`，M3 改造时必须剔除（现有测试未断言该字段）。

---

## 12. M3 硬验收项 → 实现点映射

| 硬验收项 | 实现点 | 验证方式 |
|---|---|---|
| Chrome argv 中不含上游代理口令 | `browser/argv.go` + 单测 `TestArgvNeverContainsUpstreamSecret` | `go test ./browser/ -run Argv`；人工查进程命令行 |
| CDP 端口由 `--remote-debugging-port=0` 分配 | `argv.go` + `manager.go` Stage 5 读 `DevToolsActivePort` | 启动后比对 `system`/runtime 返回的 `cdp_port` 与文件内容一致 |
| 本地代理端口动态空闲 | `ports.go` + `network.ServeListener`（持 listener，无竞态） | 并发启动 5 个环境，端口互不相同且均可用 |
| 多环境并发启动、注册表线程安全 | `Manager.mu` + `perEnv` | `-race` 跑并发启动测试 |
| 任一启动阶段失败均回滚，无悬空 running | §3 回滚矩阵 + `MarkLaunchFailed` | 逐 stage 注入故障（假 Chrome 路径 / 占用端口 / 超时），断言状态回到 idle、无残留进程与 listener |
| 启动器重启后残留进程对账正确 | `reconcile.go` | 启动 2 个环境 → kill 服务 → 重启，断言：活着的重挂、死了的复位、孤儿被关闭 |
| 注入生效、基线一致、两环境指纹差异符合要求 + 网络请求头与 JS 指纹不冲突 | `inject.go` + `gateway/web/fp-test.html`（**原样保留**） | 两个环境分别开 `/fp-test`，核对 11 项读数与 `baseline_hash`，并用本地代理的 `JA3/JA4` 自检接口比对 |
| 旧 Tauri 客户端在 M3 验收通过后再归档删除 | `client/` 仅归档，M3 前不移植其 webview/工具条代码 | 验收通过后执行归档 |

---

## 13. M3 实现落点（2026-11-01 编码完成）

| 设计点 | 代码 | 说明 |
|---|---|---|
| Stage 0 本机冲突 + 陈旧条目 | `browser/launch.go: Start` | 进程活着 → 409「已在本机运行」；死了 → 摘条目再启动 |
| Stage 1 业务启动单 | `shop.EnvService.Launch` | 增补 `ExitMode`/`StartURL`；手工代理（env.proxy_id）优先于代理商池，口令解密后只在内存 |
| Stage 2 前置检查 | `Manager.Precheck` | 浏览器 + ca_ready + profile 目录 |
| Stage 3 本地 TLSProxy | `browser/launch.go: startLocalProxy` | 动态端口 + `/healthz` 轮询；上游口令只进 Go 内存 |
| Stage 4 拉起进程 | `browser/process.go: StartChrome`（Windows 不继承控制台） | argv 经 `ArgvHasSecret` 扫描后才会真拉起 |
| Stage 5 CDP 端口 | `waitDevToolsPort` 轮询 `DevToolsActivePort` | 上限 `ZINIAO_CDP_TIMEOUT`（默认 20s） |
| Stage 6 CDP 连接 + 注入 | `browser/cdp.go`（gorilla/websocket）+ `inject.go` | 已有页面补注入不 reload；新标签页靠 `Target.setAutoAttach` 事件补注入 |
| Stage 7 打开首页 | `conn.Navigate` | 失败只告警，不算启动失败 |
| Stage 8 提交 running | `shop.EnvService.MarkRunning` | **唯一提交点**；用 `context.WithoutCancel`，前端断开也不影响提交 |
| Stage 9 登记运行时 | `Registry.Upsert` + 崩溃看护 goroutine | 登记失败只降级（degraded），不回滚已起窗口 |
| 停止 | `Manager.Stop` | 截图 → 逐页 `Page.close` → `Browser.close` → 宽限 → 强杀 → 收代理 → 摘注册表 → `StopEnv` |
| 崩溃自动复位 | `watchProcess` | 进程退出即复位 idle，避免前端一直显示「运行中」 |
| 对账三档 | `browser/reconcile.go` | 死条目摘除 + 库里 running 复位（`ResetStaleRunning`）、活条目重挂端口、孤儿进程关闭（快照不可用时降级） |
| 失败回滚矩阵 | `browser/launch_test.go: TestStartRollbackMatrix` | 逐阶段注入故障，断言端口/进程/状态都收干净 |
| CA 装/删 + 阻断 | `browser/ca.go` + `gateway/system.go` | `ca:manage` 权限；装完立刻刷新 `ca_ready`；卸载后启动重新阻断 |
| 前端状态联动 | `gateway/web/app.js` | 绿点/灰点、PID、CDP 端口、本地代理端口、运行时长；启动按钮在「已在本机运行」时禁用；不做乐观更新 |

**尚未完成（需人工闭环）**：CA 安装到本机信任库 + 两窗口 `fp-test.html` 对比（验收第 3 项）。
自动化部分已验证到「CA 未就绪 → 启动在阶段 2 被阻断并给出明确文案」这一档。
