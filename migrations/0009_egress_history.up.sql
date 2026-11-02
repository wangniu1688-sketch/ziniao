-- =============================================================================
-- schema version: phase2c-egress-history v1
-- Phase 2C：出口绑定历史留档（egress_binding_history）
--
-- 为什么必须事件化：egress_bindings 只有「当前绑定」，一次释放 / 一次换绑
-- 就把过去抹掉了。而风控里真正要回答的问题是——
--   「这两个店铺有没有在**历史上**共用过同一 ASN / 同一出口？」
-- 时间错开的共用，当前绑定表永远看不见（那时另一个环境早已解绑）。
-- 所以每次生命周期变化追加一行，**只追加不改写**：历史是事实，不能因为现在
-- 看起来正常而被覆盖。
--
-- 记录哪些动作（action）：
--   bound    首次绑定
--   rebound  换绑（同一个环境换了资源/出口，prev_exit 记录从哪变到哪）
--   released 主动释放（Allocator.Release，例如环境停用）
--   expired  租约到期回收（network.egress_sweep）
--   dropped  既存绑定作废（代理商下架 / 不支持取回凭据 / 取回失败 / 绑定已过期）
--
-- L4：与 egress_bindings 同口径，**不含任何凭据**（username/password 不落库）。
-- 保留：当前不自动清理（历史是资产）；如需按保留期裁剪，
-- 走 idx_egress_history_tenant_time 的区间删除，不要加触发器。
-- 可回滚：见 0009_egress_history.down.sql
-- =============================================================================

CREATE TABLE IF NOT EXISTS egress_binding_history (
    id          text PRIMARY KEY,
    tenant_id   text NOT NULL,
    env_id      text NOT NULL,
    action      text NOT NULL,
    provider    text NOT NULL DEFAULT '',
    resource_id text NOT NULL DEFAULT '',
    protocol    text,
    host        text,
    port        integer,
    country     text,
    region      text,
    city        text,
    asn         text,
    asn_type    text,
    exit_addr   text,                        -- 当时的出口 host:port（平台真正看到的身份）
    prev_exit   text,                        -- 上一跳出口（rebound 时用来说明「从哪变到哪」）
    reason      text,                        -- 人可读的成因（排障第一问）
    occurred_at timestamptz NOT NULL DEFAULT now()
);

-- action 用 CHECK 而不是自由文本：拼错一个字母的事件等于不存在，
-- 而「这个动作到底有没有被记录」是这条链路唯一的验收标准。
ALTER TABLE egress_binding_history
    ADD CONSTRAINT egress_history_action_chk
    CHECK (action IN ('bound', 'rebound', 'released', 'expired', 'dropped'));

-- 时间线：控制台按租户倒序翻页。
CREATE INDEX IF NOT EXISTS idx_egress_history_tenant_time
    ON egress_binding_history (tenant_id, occurred_at DESC);

-- 单环境时间线（排障：这个店铺的出口换过几次）。
CREATE INDEX IF NOT EXISTS idx_egress_history_env
    ON egress_binding_history (tenant_id, env_id, occurred_at DESC);

-- 关联检测：同一 ASN 下出现过哪些环境（group by asn）。
CREATE INDEX IF NOT EXISTS idx_egress_history_asn
    ON egress_binding_history (tenant_id, asn) WHERE asn IS NOT NULL AND asn <> '';

-- 出口地址维度：同一 host:port 是否被不同环境用过（ASN 拿不到时的兜底判据）。
CREATE INDEX IF NOT EXISTS idx_egress_history_exit
    ON egress_binding_history (tenant_id, exit_addr) WHERE exit_addr IS NOT NULL AND exit_addr <> '';

-- 与 egress_bindings 同口径：**只 ENABLE，不 FORCE**。
-- 刻意不加 FORCE 的原因不是省事：加了之后连表主（应用自己的库账号）也被策略约束，
-- 于是任何不带租户上下文的维护 SQL（保留期裁剪、排障手工 DELETE）都会
-- 「执行成功但零行」——静默残留比报错难查得多。本轮就踩过：残留行让主键冲突，
-- 新事件被 ON CONFLICT DO NOTHING 全部吞掉，表现为「这条链路一条都没写进去」。
-- 应用侧的隔离依旧成立：所有读写都经仓储注入 tenant_id，且显式带 tenant_id 谓词。
ALTER TABLE egress_binding_history ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON egress_binding_history
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
