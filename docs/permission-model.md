# 权限模型（五层 + 双层口径）

配套文件：`docs/permission-matrix.csv`（码表 × 角色矩阵）、`iam/permissions.go`（目录与页面登记）、
`iam/policy.go`（裁决）、`iam/roles.go`（内置角色）。

## 1. 五层裁决

`iam.Engine.Check` 按 功能 → 资源 → 页面 → 元素 → 数据 顺序裁决，先拒先返回，`Decision.Layer` 标明被拒层级。

| 层 | 码形 | 决定什么 |
|---|---|---|
| function | `shop:read`、`proxy:manage`、`env:launch` | 能不能做这个动作（**资源接口的唯一闸门**） |
| resource | `resource:device.purchase`、`DataGrant` | 有没有这个资源的授权（含临时授权到期） |
| page | `page:proxies`、`page:logs` | 纯页面导航能不能进（**不拦截资源接口**） |
| element | `element:shop.delete_button` | 按钮/字段是否屏蔽（DOM 注入层第三层） |
| data | `data:shop` + `DataScope`（none/own/team/all） | 能看到哪些行 |

## 2. 双层口径（M2 终审定稿，T15）

> 一句话：**资源接口看资源/功能码，`page:*` 只管导航可见性与纯页面导航。**

1. **资源接口层（强制）**
   `GET /api/v1/proxies`、`GET /api/v1/environments`、`GET /api/v1/logs` 等，网关 handler 显式传入
   `action`（`proxy:read` / `env:read` / `audit:read` …）。功能码或资源授权缺失即 403，
   **缺失 `page:*` 不改变这一判定**——`iam.Engine.Check` 在 `Action != ""` 时不再走页面层拦截。

   反例（曾发生）：若页面层也拦，cs / agency_ops 持有 `env:read` 却因缺 `page:environments` 被拒，
   出现"给了只读码又被页面层收回"的自相矛盾。

2. **页面层（可见性 + 纯导航）**
   `page:*` 用于：
   - `Action == ""` 的纯页面导航裁决（例如门户侧渲染前问一次 `/api/v1/iam/check`）；
   - 作为页面级能力的登记口径，便于权限目录与审计对齐。
   它**不单独承担网关拦截责任**：一个只有 `page:proxies` 而没有 `proxy:read` 的主体，调用代理接口仍然 403。

3. **前端导航可见性**
   导航清单由后端 `iam.NavSections` 定义，经 `GET /api/v1/me` 的 `nav[]` 下发（含 `visible`）。
   `visible` = **资源/功能码 AND 页面码**（`iam.NavFor`）：

   | 导航项 | 资源码 | 页面码 |
   |---|---|---|
   | 店铺管理 | `shop:read` | `page:shops` |
   | 代理管理 | `proxy:read` | `page:proxies` |
   | 分组管理 | `group:read` | `page:groups` |
   | 设备与网络 | `device:read` | `page:devices` |
   | 日志中心 | `audit:read` | `page:logs` |
   | 工作台 / 系统设置 | — | —（恒可见） |

   **资源码被拒 ⇒ 对应导航项从侧边栏消失**（不可见即不可达，直接改 hash 也会被服务端 403 挡回）；
   只有页面码而没有资源码同样隐藏——页面码不单独放行。
   前端（`gateway/web/app.js` 的 `visibleSections()`）只按 `visible` 渲染，不自行判断权限。

   已知口径差（本期不改角色权限，仅登记）：operator 持有 `page:logs` 但没有 `audit:read`，
   故日志中心对运营不可见。要么补 `audit:read`（扩权），要么收回 `page:logs`，
   留待下一次权限迭代决定；当前以「不留点了就 403 的死入口」为准。

## 3. 已登记页面路径（`iam.RequiredPage`）

| 路径 | 页面码 |
|---|---|
| `/shops` | `page:shops` |
| `/finance` | `page:finance` |
| `/audit` | `page:audit` |
| `/team` | `page:team` |
| `/devices` | `page:devices` |
| `/security` | `page:security` |
| `/groups` | `page:groups` |
| `/proxies` | `page:proxies` |
| `/logs` | `page:logs` |
| `/environments` | `page:environments` |

`page:approvals` 已在权限目录中，但门户暂无独立审批页，暂不登记路径（登记即生效，避免误拦）。

## 4. 角色口径（M2 终审确认，不再收紧）

| 角色 | 代理 | 环境 | 分组 | 页面 |
|---|---|---|---|---|
| owner | read + manage | 全部 | read + manage | 全部 |
| ops_manager | read + manage | 全部（含停用/删除） | read + manage | 全部 |
| operator | **仅 `proxy:read`** | `env:read` + `env:update`（含启停） | `group:read` | shops/proxies/groups/logs/devices/approvals |
| cs | — | `env:read` | `group:read` | **仅 groups**（+shops） |
| agency_ops | — | `env:check`/`launch`/`stop`/`read` | `group:read` | 仅 shops |
| finance | 全部拒绝 | 全部拒绝 | 全部拒绝 | finance/shops（字段遮蔽） |
| 无角色成员 | 全部 403 | 全部 403 | 全部 403 | 无可访问模块 |

管理类权限（`proxy:manage`、`group:manage`）只有 owner 与 ops_manager 持有，边界由
`gateway/m2_test.go:TestM2PermissionDenied` 锁定。

## 5. MITM 根证书（M3/T7）

本地代理是 MITM 架构，CA 不在信任库里，浏览器每个 HTTPS 站点都会报证书错误，
因此 `ca_ready=false` 时**启动直接阻断**（`browser.Manager.Precheck` 返回 `ErrCANotReady`），
不用 `--ignore-certificate-errors` 绕过——那会把「证书错误」这个真实信号一起屏蔽掉。

| 接口 | 权限 | 说明 |
|---|---|---|
| `GET /api/v1/system/ca` | 登录即可 | CA 状态（`present` / `installed` / `ready` / `detail`） |
| `POST /api/v1/system/ca/install` | `ca:manage` | 生成并安装到**当前用户**信任库（不需要管理员权限） |
| `POST /api/v1/system/ca/uninstall` | `ca:manage` | 从信任库移除；卸载后启动再次被阻断 |

`ca:manage` 只有 owner 与 ops_manager 持有：改信任库会影响本机所有浏览器对 HTTPS 的判定。
装/删是**有副作用**的动作，必须由人显式触发——启动器自己只如实报告 `ca_ready`，绝不自动安装。

## 6. 待办

- T14：历史自由文本分组名自动匹配/创建分组（本期不隐式创建，见 M2 终审 Q2）。
