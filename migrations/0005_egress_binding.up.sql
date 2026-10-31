-- =============================================================================
-- schema version: phase2b-egress-binding v1
-- Phase 2B：出口绑定落库（重启/发布不换 IP）
--
-- 为什么必须落库：绑定关系原先只活在 Allocator 的 map 里，进程一重启就丢，
-- 于是每个环境都被重新分配一个新出口。对平台来说「同一账号忽然换登录 IP」
-- 是最强的关联信号之一——等于每次 deploy 都主动给全部店铺做一次 IP 跳变。
--
-- L4：出口凭据（username/password）**不在这里**。凭据只随启动单下发，
-- 重启后按 resource_id 向代理商取回（network.Attacher），库中只留标识与元数据。
-- =============================================================================
CREATE TABLE IF NOT EXISTS egress_bindings (
    env_id      text PRIMARY KEY,          -- 一个环境同一时刻只能绑一条出口
    tenant_id   text NOT NULL,
    provider    text NOT NULL,             -- 代理商名（多代理商路由用）
    resource_id text NOT NULL,             -- 代理商侧资源 ID（向厂商归还/取回凭据的钥匙）
    protocol    text NOT NULL,             -- socks5|http|https
    host        text NOT NULL,
    port        integer NOT NULL,
    country     text,
    region      text,
    city        text,
    asn         text,
    asn_type    text,                      -- residential|isp|mobile|datacenter
    region_code text,                      -- 申请时用的区域码（复现同一区域用）
    last_exit   text,                      -- 上次实际使用的出口 host:port（跨重启的轮换判定）
    bound_at    timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,      -- 到期即归还厂商，避免持续计费
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 互斥锁一：同一资源只能属于一个环境。内存里的 owner map 只在一个进程内有效；
-- 滚动发布时新旧进程各自向代理商申请到同一个资源，只有数据库能挡住（先查后写必然双成功）。
CREATE UNIQUE INDEX IF NOT EXISTS egress_bindings_resource_uniq ON egress_bindings (provider, resource_id);

-- 互斥锁二：同一个出口地址（host:port）只能属于一个环境。
-- 为什么资源 ID 不够：代理商对同一个 IP:PORT 可能重复售卖/重复下发不同的订单号，
-- 而平台看到的出口是地址，不是我们的订单号。少了这条，两个店铺会在同一 IP 上登录——
-- 这正是多账号防关联最不能出的事故。
CREATE UNIQUE INDEX IF NOT EXISTS egress_bindings_exit_uniq ON egress_bindings (provider, host, port);
CREATE INDEX IF NOT EXISTS egress_bindings_tenant_idx ON egress_bindings (tenant_id, expires_at);

ALTER TABLE egress_bindings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON egress_bindings
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
