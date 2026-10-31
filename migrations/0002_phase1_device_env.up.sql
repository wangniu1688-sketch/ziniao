-- =============================================================================
-- schema version: phase1-device-env v1
-- Phase 1：设备与网络（M3）、店铺环境（M0/M1）、安全策略（M14）、凭证托管（M2.2）
-- 铁律：租户域表必须有 tenant_id；写路径额外受 RLS WITH CHECK 保护（fail-closed）
-- =============================================================================

-- ---------- 店铺扩展字段（分组/标签/绑定设备） ----------
ALTER TABLE shops ADD COLUMN IF NOT EXISTS group_name text;
ALTER TABLE shops ADD COLUMN IF NOT EXISTS tags text[] NOT NULL DEFAULT '{}';
ALTER TABLE shops ADD COLUMN IF NOT EXISTS device_id text;
CREATE INDEX IF NOT EXISTS shops_group_idx ON shops (tenant_id, group_name);
CREATE INDEX IF NOT EXISTS shops_device_idx ON shops (tenant_id, device_id);

-- ---------- 设备 ----------
CREATE TABLE IF NOT EXISTS devices (
    id              text PRIMARY KEY,
    tenant_id       text NOT NULL,
    name            text NOT NULL,
    type            text NOT NULL,          -- cloud_network|self_proxy|vps|broadband|box|cloud_phone|cloud_number
    sn              text,                   -- 紫鸟盒子硬件序列号
    ip              text,
    country         text,
    region          text,
    city            text,
    asn             text,
    asn_type        text,                   -- residential|isp|mobile|datacenter
    status          text NOT NULL DEFAULT 'purchasing',
    owner_id        text,
    protocol        text,                   -- http|https|socks5
    proxy_host      text,
    proxy_port      integer,
    proxy_user      text,
    proxy_secret_enc text,                  -- L4：代理口令密文（信封加密）
    health_score    integer NOT NULL DEFAULT 0,
    purity_score    integer NOT NULL DEFAULT 0,
    bound_accounts  integer NOT NULL DEFAULT 0,
    concurrent_users integer NOT NULL DEFAULT 0,
    max_concurrent  integer NOT NULL DEFAULT 6,  -- L9：配额模型，不是互斥锁
    expires_at      timestamptz NOT NULL,
    auto_renew      boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);
CREATE INDEX IF NOT EXISTS devices_tenant_idx ON devices (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS devices_type_idx ON devices (tenant_id, type, status);

-- 绑定关系：一台设备可绑定多个店铺账号（L9），并发人数按 user_id 去重
CREATE TABLE IF NOT EXISTS device_bindings (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    device_id    text NOT NULL,
    shop_id      text NOT NULL,
    user_id      text,                      -- 当前占用者
    bound_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    unbound_at   timestamptz
);
CREATE INDEX IF NOT EXISTS bindings_device_idx ON device_bindings (tenant_id, device_id);
CREATE INDEX IF NOT EXISTS bindings_shop_idx ON device_bindings (tenant_id, shop_id);

-- ---------- 店铺浏览器环境（画像 + 内核 + TLS 基线绑定） ----------
CREATE TABLE IF NOT EXISTS environments (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL,
    shop_id       text,
    device_id     text,
    profile_id    text NOT NULL,
    baseline_hash text NOT NULL,            -- L8：离线启动校验一致才允许启动
    kernel_major  integer NOT NULL DEFAULT 0,
    tls_hello_id  text,
    status        text NOT NULL DEFAULT 'created',
    snapshot      jsonb NOT NULL DEFAULT '{}',  -- 基线快照（漂移检测）
    consistency   jsonb NOT NULL DEFAULT '{}',  -- 创建时的自洽性报告
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS envs_tenant_shop_idx ON environments (tenant_id, shop_id);

-- ---------- 访问策略（M14 事前拦截） ----------
CREATE TABLE IF NOT EXISTS access_policies (
    id        text PRIMARY KEY,
    tenant_id text NOT NULL,
    name      text NOT NULL,
    scope     text NOT NULL,                -- shop|member|team
    scope_id  text,
    template  text,
    enabled   boolean NOT NULL DEFAULT true,
    rules     jsonb NOT NULL DEFAULT '[]',
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS policies_tenant_idx ON access_policies (tenant_id, scope, scope_id);

-- ---------- 安全事件（事中告警 / 事后追溯） ----------
CREATE TABLE IF NOT EXISTS security_events (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL,
    category      text NOT NULL,            -- intercept|alert|risk|drift
    level         text NOT NULL,            -- high|medium|low
    shop_id       text,
    device_id     text,
    actor_user_id text,
    action        text NOT NULL,
    detail        jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sec_events_tenant_idx ON security_events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sec_events_cat_idx ON security_events (tenant_id, category, level);

-- ---------- 店铺凭证托管（M2.2 / Q10 信封加密） ----------
CREATE TABLE IF NOT EXISTS shop_credentials (
    id          text PRIMARY KEY,
    tenant_id   text NOT NULL,
    shop_id     text NOT NULL,
    kind        text NOT NULL,              -- password|totp|passkey|pay_account|email
    username    text,
    secret_enc  text NOT NULL,              -- L4：仅密文
    algo        text NOT NULL,
    key_version integer NOT NULL DEFAULT 1, -- 密钥轮换版本
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text
);
CREATE UNIQUE INDEX IF NOT EXISTS shop_creds_uniq ON shop_credentials (tenant_id, shop_id, kind);

-- =============================================================================
-- 行级安全（与 0001 同策略：current_tenant() 为空 → 零行，fail-closed）
-- =============================================================================
ALTER TABLE devices          ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_bindings  ENABLE ROW LEVEL SECURITY;
ALTER TABLE environments     ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_policies  ENABLE ROW LEVEL SECURITY;
ALTER TABLE security_events  ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_credentials ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON devices
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON device_bindings
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON environments
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON access_policies
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON security_events
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON shop_credentials
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
