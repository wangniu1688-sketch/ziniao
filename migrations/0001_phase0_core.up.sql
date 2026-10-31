-- =============================================================================
-- schema version: phase0-core v1
-- 说明：Phase 0 地基（租户 / 身份 / 权限 / 事件总线 / 审计 / 通知 / 计量 / 店铺最小实体）
-- 铁律：租户域表必须有 tenant_id；写路径额外受 RLS WITH CHECK 保护（fail-closed）
-- =============================================================================

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);

-- ---------- 租户 ----------
CREATE TABLE tenants (
    id          text PRIMARY KEY,
    name        text NOT NULL,
    slug        text NOT NULL,
    status      text NOT NULL DEFAULT 'trial',
    plan_code   text NOT NULL DEFAULT 'free',
    schema_name text,                       -- 企业版独立 schema 逃生舱（空=共享库+行级隔离）
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tenants_slug_uniq ON tenants (slug);

CREATE TABLE memberships (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    user_id    text NOT NULL,
    status     text NOT NULL DEFAULT 'invited',
    invited_by text,
    joined_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX memberships_tenant_user_uniq ON memberships (tenant_id, user_id);
CREATE INDEX memberships_tenant_idx ON memberships (tenant_id);

-- ---------- 身份 ----------
CREATE TABLE users (
    id            text PRIMARY KEY,
    username      text NOT NULL,
    email         text,
    phone         text,
    display_name  text,
    status        text NOT NULL DEFAULT 'active',
    mfa_enabled   boolean NOT NULL DEFAULT false,
    totp_secret   text,                     -- L4：生产环境应为信封加密密文（KMS）
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);
CREATE UNIQUE INDEX users_username_uniq ON users (username);

CREATE TABLE credentials (
    user_id      text PRIMARY KEY,
    password_enc text NOT NULL,             -- 仅存密文（pbkdf2-sha256$iter$salt$hash）
    algo         text NOT NULL,
    key_version  integer NOT NULL DEFAULT 1, -- 密钥轮换版本（Q10 信封加密）
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id            text PRIMARY KEY,
    user_id       text NOT NULL,
    tenant_id     text NOT NULL,
    ip            text,
    user_agent    text,
    terminal_fp   text,
    client_ver    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    login_result  text NOT NULL DEFAULT 'success',
    blocked_cause text
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_tenant_idx ON sessions (tenant_id);

-- ---------- 权限（RBAC + ABAC 五层） ----------
CREATE TABLE roles (
    id            text PRIMARY KEY,
    tenant_id     text,                     -- NULL = 系统内置角色
    code          text NOT NULL,
    name          text NOT NULL,
    builtin       boolean NOT NULL DEFAULT false,
    mask_fields   text[] NOT NULL DEFAULT '{}',   -- 字段级遮蔽（M13）
    deny_elements text[] NOT NULL DEFAULT '{}',   -- 元素级屏蔽（M14 事前拦截）
    data_scope    text NOT NULL DEFAULT 'none',   -- none|own|team|all
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX roles_code_uniq ON roles (COALESCE(tenant_id, '~'), code);

CREATE TABLE role_permissions (
    role_id        text NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_key text NOT NULL,
    PRIMARY KEY (role_id, permission_key)
);

CREATE TABLE user_roles (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    user_id    text NOT NULL,
    role_id    text NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    granted_by text,
    granted_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_roles_uniq ON user_roles (tenant_id, user_id, role_id);
CREATE INDEX user_roles_tenant_idx ON user_roles (tenant_id);

-- 资源级授权（第五层）：临时授权到期自动失效
CREATE TABLE data_grants (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL,
    user_id       text NOT NULL,
    resource_type text NOT NULL,
    resource_id   text NOT NULL DEFAULT '*',
    scope         text,
    granted_by    text,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX data_grants_user_idx ON data_grants (tenant_id, user_id, resource_type);

-- ---------- 计费与计量 ----------
CREATE TABLE subscriptions (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    plan_code  text NOT NULL,
    status     text NOT NULL DEFAULT 'active',
    started_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX subscriptions_tenant_idx ON subscriptions (tenant_id, status);

CREATE TABLE usage_counters (
    tenant_id  text NOT NULL,
    metric     text NOT NULL,
    value      bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, metric)
);

-- ---------- 事件总线（事务发件箱） ----------
CREATE TABLE event_outbox (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    status       text NOT NULL DEFAULT 'pending',
    attempts     integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_outbox_claim_idx ON event_outbox (status, available_at);
CREATE INDEX event_outbox_tenant_idx ON event_outbox (tenant_id);

-- ---------- 审计（六大日志 + 哈希链） ----------
CREATE TABLE audit_log (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL,
    category      text NOT NULL,            -- login|operation|admin|risk|upload|grant
    actor_user_id text,
    actor_name    text,
    shop_id       text,
    device_id     text,
    terminal_ip   text,
    terminal_fp   text,
    action        text NOT NULL,
    result        text NOT NULL,
    url           text,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    data          jsonb,
    prev_hash     text NOT NULL DEFAULT '',
    hash          text NOT NULL
);
CREATE INDEX audit_tenant_time_idx ON audit_log (tenant_id, occurred_at DESC);
CREATE INDEX audit_tenant_category_idx ON audit_log (tenant_id, category);
CREATE INDEX audit_tenant_actor_idx ON audit_log (tenant_id, actor_user_id);

-- ---------- 通知 ----------
CREATE TABLE notifications (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    user_id    text NOT NULL,
    category   text NOT NULL,
    title      text NOT NULL,
    body       text NOT NULL,
    event_type text,
    channels   text[] NOT NULL DEFAULT '{inapp}',
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at    timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (tenant_id, user_id, created_at DESC);

-- ---------- 店铺（Phase 0 最小实体，M2 正式实现） ----------
CREATE TABLE shops (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    name       text NOT NULL,
    platform   text,
    site       text,
    status     text NOT NULL DEFAULT '注册中',
    owner_id   text NOT NULL,
    profit     numeric(18, 2) NOT NULL DEFAULT 0,  -- 受字段级遮蔽（M13）
    cost       numeric(18, 2) NOT NULL DEFAULT 0,
    balance    numeric(18, 2) NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE INDEX shops_tenant_idx ON shops (tenant_id, created_at DESC);
CREATE INDEX shops_owner_idx ON shops (tenant_id, owner_id);

-- =============================================================================
-- 行级安全（RLS）：多租户的第二道防线（L12）
-- current_tenant() 取不到值时返回 NULL，比较结果为 NULL → 零行（fail-closed）
-- =============================================================================
CREATE OR REPLACE FUNCTION current_tenant() RETURNS text AS $$
    SELECT NULLIF(current_setting('app.tenant_id', true), '');
$$ LANGUAGE sql STABLE;

ALTER TABLE memberships    ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles          ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_roles     ENABLE ROW LEVEL SECURITY;
ALTER TABLE data_grants    ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions       ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions  ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox   ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log      ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications  ENABLE ROW LEVEL SECURITY;
ALTER TABLE shops          ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
-- 登录场景下租户未知：允许按 user_id 查本人所属团队（必须带 user_id 谓词，见驱动实现）
CREATE POLICY login_lookup ON memberships FOR SELECT
    USING (current_tenant() IS NULL);
-- 系统内置角色（tenant_id IS NULL）对所有租户可见
CREATE POLICY tenant_isolation ON roles
    USING (tenant_id IS NULL OR tenant_id = current_tenant())
    WITH CHECK (tenant_id IS NULL OR tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON user_roles
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON data_grants
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON subscriptions
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON usage_counters
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON event_outbox
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON audit_log
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON notifications
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY tenant_isolation ON shops
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
