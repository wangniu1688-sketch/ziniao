-- =============================================================================
-- schema version: phase2c-notify-delivery v1
-- Phase 2C：通知外发（邮件/Webhook）的投递留痕
--
-- 为什么要在 notifications 上加状态列而不是新开一张投递表：
--  1. 通知本体与投递状态是 1:1 的，「这条 high 告警到底发没发到运营邮箱」
--     是运营唯一的排查依据，放在两处就得 JOIN，且容易不同步；
--  2. 重投任务只需要「状态 != sent 的行」，按 tenant_id + 状态过滤即可，
--     不需要独立的生命周期管理。
--
-- 语义约定（mem 与 pg 必须逐条一致，见容灾笔记）——
--  delivery_status:
--    none    本次通知没有任何外发通道（纯站内，或通道全被跳过）
--    pending 已入库待投递（首次投递或重投仍尝试中）
--    sent    所有需要的通道都已送达
--    failed  至少一个通道失败且已耗尽 MaxAttempts
--  delivered_channels: **已终结**的通道集合（重投只对差集尝试）。
--    终结构含两种情况：① 成功送达 ② 明确跳过（如该用户未填邮箱，
--    ErrNoRecipient）。第二种如果不记进来，重投任务会把「没邮箱的人」当成
--    「投递失败的人」，每轮重试一次直到耗尽次数，最后还是 failed ——
--    这类假失败会把真正需要人看的告警淹没掉。
--  attempts: 该行累计投递尝试次数（跨重投累加，非单通道）
-- =============================================================================
ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS delivery_status    text NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS delivery_note      text,
    ADD COLUMN IF NOT EXISTS attempts           integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS delivered_channels text[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS last_delivery_at   timestamptz;

-- 无效取值一旦写进来，重投任务会把整张表当成待办反复扫描。
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_chk;
ALTER TABLE notifications ADD CONSTRAINT notifications_status_chk
    CHECK (delivery_status IN ('none', 'pending', 'sent', 'failed'));

-- 重投任务的热点查询：这些行的量级通常只有个位数，避免全表扫描。
CREATE INDEX IF NOT EXISTS notifications_retry_idx
    ON notifications (tenant_id, created_at)
    WHERE delivery_status IN ('pending', 'failed');
