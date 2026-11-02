-- Phase 2 补齐：每人通知偏好（上一阶段 TODO：通知「触发即全通道发」）
--
-- 现状：notify Sink 的外发清单按「当前装了什么通道」决定（email/sms/webhook 全发），
-- 只要用户填了邮箱手机号就一定会收到。短告警如此合理，但日常审批待办这样推，
-- 人会把告警静音——**噪音是最有效的「关掉所有通知」按钮**。
-- 所以把选择权交回给每个人：站内信始终保留（它是可回看的事实记录），
-- 外发通道由用户自己勾选；webhook 是服务端集成通道（全局单 URL），不进个人偏好。
--
-- 语义：**行是否存在**区分「未设置」与「设置过」——
--   · 无行：走默认（所有已装配的外发通道，行为与改造前一致，不影响既有租户）
--   · 有行且 channels=[]：只要站内信（用户主动关闭全部外发）
--   · 有行且 channels=["email"]：只外发邮件
-- 用行存在而不是「空数组代表默认」，是因为「我真的什么都不想要」必须能被表达出来；
-- 否则用户关掉全部开关后一轮又自动回到全开。
--
-- 租户隔离：普通 tenant_isolation 策略（prefs 归属 (tenant_id, user_id)，
-- 同一人在不同团队的偏好互不影响——这是刻意的，一个人在 A 团队要短信、在 B 团队不想被打扰很常见）。
-- 可回滚：见 0008_notify_prefs.down.sql

CREATE TABLE IF NOT EXISTS notification_prefs (
    tenant_id  text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id    text NOT NULL,
    channels   jsonb NOT NULL DEFAULT '[]', -- 用户启用的**外发**通道子集，如 ["email","sms"]
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

-- 只允许数组：写成标量会让读取侧的解析分支越来越多
ALTER TABLE notification_prefs
    ADD CONSTRAINT notification_prefs_channels_is_array CHECK (jsonb_typeof(channels) = 'array');

CREATE INDEX IF NOT EXISTS idx_notification_prefs_user ON notification_prefs(user_id);

ALTER TABLE notification_prefs ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_prefs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_prefs
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
