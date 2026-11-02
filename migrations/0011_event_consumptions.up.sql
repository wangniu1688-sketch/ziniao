-- =============================================================================
-- schema version: phase2d-event-consumptions v1
-- 接收侧投递幂等表（event_consumptions）
--
-- 发件箱（event_outbox）只能保证 at-least-once：Handle 成功到 Ack 之间
-- 崩溃/发布切换，整条事件会重投一遍。没有这张表时，重投的后果是——
--   notify.Sink 再生成一整份站内通知（同一事件两封信）；
--   audit.Sink 撞审计主键（entry.ID = event.ID），事件卡在 pending 直到重试耗尽。
--
-- 用法：每个 Handler 在自己的业务事务内 MarkConsumed(event_id, handler)，
-- 派发器重投时按 (event_id, handler) 跳过已消费的处理器——
-- 部分成功的进度得以保留，而不是整条事件从头再来。
--
-- 外发通道（邮件/Webhook）自身的 at-least-once 窗口（Send 成功但状态回写前崩溃）
-- 不由这张表解决：那半步在对方系统里，接收方按通知 id 去重是契约的一部分
-- （见 docs/TODO-phase2.md 第 14 条）。这张表收窄的是我们自己能收窄的部分。
--
-- 可回滚：见 0011_event_consumptions.down.sql
-- =============================================================================

CREATE TABLE IF NOT EXISTS event_consumptions (
    event_id    text NOT NULL,
    handler     text NOT NULL,
    tenant_id   text NOT NULL DEFAULT '',
    consumed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, handler)
);

-- 观测：哪个 handler 在什么时候消费了哪条事件（排障第一问「这条事件处理过没有」）。
CREATE INDEX IF NOT EXISTS idx_event_consumptions_tenant_time
    ON event_consumptions (tenant_id, consumed_at DESC);

-- 与 egress_binding_history 同口径：只 ENABLE，不 FORCE。
-- FORCE 会让不带租户上下文的维护 SQL「执行成功但零行」（0009 踩过）。
ALTER TABLE event_consumptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON event_consumptions
    USING (tenant_id = current_tenant() OR current_tenant() IS NULL)
    WITH CHECK (tenant_id = current_tenant());
