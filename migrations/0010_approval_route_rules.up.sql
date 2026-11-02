-- Phase 2 收口：审批路由规则可配置（approval_route_rules）
--
-- 现状：审批路由（谁有资格批这张单、要不要会签）硬编码在 approval/router.go 的 rulesOf 里，
-- 运营改一个阈值或把某个单据从「运营主管」提到「老板双人复核」，必须改代码发版。
-- 审批级别属于运营策略而不是代码不变量，它和「买设备多少钱要老板批」一样是会变的数字。
--
-- 表名的由来：approval_flows（0007）已被**代理机构委托审批**占用（跨租户两方流程），
-- 这张是单据路由规则表，另起 approval_route_rules。
--
-- 语义（重要，决定升级行为）：
--   · 租户 0 条规则：全部走内置规则（与改造前完全一致，老租户升级行为不变）；
--   · 配了该类型的规则：按规则命中（priority 升序取第一条命中的）；
--   · 该类型配了规则但**没命中**（如只配了「≥5 万双人」的单子来了 1 千）：
--     回落内置规则——半套配置不能让流程变成「无人可批」的死锁；
--   · ticket_type 为空 = 兜底规则，适用于所有类型。
--
-- 双保险：ticket_type / min_amount / min_approvers 在 DB 有 CHECK，Go 侧再判一次
-- （mem 驱动没有 CHECK；两份驱动给出不同结论就是「单测全绿、生产脏数据」——PG 那轮踩过）。
--
-- 租户隔离：只 ENABLE 不 FORCE。与 egress_bindings / egress_binding_history 同口径：
-- FORCE 之后连表主也受策略约束，维护用的裸 DELETE 会「执行成功但零行」，
-- 静默残留比报错难查（0009 踩过这条）。应用侧连接角色非超级用户，ENABLE 已经生效。
-- 可回滚：见 0010_approval_route_rules.down.sql

CREATE TABLE IF NOT EXISTS approval_route_rules (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- 空串 = 兜底规则（适用于所有单据类型）
    ticket_type   text NOT NULL DEFAULT '',
    -- 金额下限（CNY）；0 = 不卡金额。目前只有 device.purchase 用得上金额
    min_amount    numeric(14, 2) NOT NULL DEFAULT 0,
    -- 合格审批人须具备其中之一；空集 = 任意有 approval:approve 的人
    roles         text[] NOT NULL DEFAULT '{}',
    -- 会签法定人数；1 = 单人即可
    min_approvers int NOT NULL DEFAULT 1,
    -- 为什么需要这个级别：展示给申请人，避免"为什么我不能批"的黑箱
    reason        text NOT NULL DEFAULT '',
    -- 升序取第一条命中的规则
    priority      int NOT NULL DEFAULT 100,
    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 单据类型必须是已知类型（禁止配出一条永远不会被命中的规则）
ALTER TABLE approval_route_rules
    ADD CONSTRAINT approval_route_rules_type_chk
    CHECK (ticket_type IN ('', 'audit.export.plain', 'shop.credential.reveal',
                           'device.purchase', 'shop.bulk_delete'));

ALTER TABLE approval_route_rules
    ADD CONSTRAINT approval_route_rules_amount_chk CHECK (min_amount >= 0);

-- 上限不是洁癖：法定人数配成 99 等于这张单永远批不出来
ALTER TABLE approval_route_rules
    ADD CONSTRAINT approval_route_rules_quorum_chk CHECK (min_approvers BETWEEN 1 AND 10);

-- 同一类型同一金额门槛同一优先级只允许一条：重复规则会让「取第一条命中」变成掷骰子
CREATE UNIQUE INDEX IF NOT EXISTS uq_approval_route_rule
    ON approval_route_rules (tenant_id, ticket_type, min_amount, priority);

CREATE INDEX IF NOT EXISTS idx_approval_route_rules_tenant
    ON approval_route_rules (tenant_id, priority);

ALTER TABLE approval_route_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approval_route_rules
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
