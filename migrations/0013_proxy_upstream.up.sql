-- =============================================================================
-- schema version: p3-proxy-upstream v1
-- P3 上游住宅/ISP 代理接入：proxies 台账扩展（类型、城市、拨测健康与出口实测）
--
-- 本期只做加法：给 0012 建的 proxies 表加列，不动主键、不动既有列语义、
-- 不删任何历史数据，因此可重复执行（ADD COLUMN IF NOT EXISTS）且可回滚
-- （见 0013_proxy_upstream.down.sql）。
--
-- 建模口径（P3 拍板）：
--   - status       保留 active|disabled，**只表启停**（管理员手动停用）；
--   - check_status 新增 unknown|ok|fail，**只表拨测健康**；
--     两者刻意分离：把「拨测失败」写成 disabled 会让运营在「启用」时
--     把 fail 结论抹掉，也会让「停用」与「不通」在列表里混成一列。
--   - 出口实测值（exit_ip / exit_country / latency_ms / last_checked_at /
--     last_error）只在拨测真正跑过之后才有值，未拨测一律空 —— 猜出来的
--     出口 IP 比没有更危险。
--
-- 沿用 0001–0012 的规范：
--   - 时间列 timestamptz；updated_at 由应用层维护（全库 0 trigger）；
--   - 索引命名 idx_{表}_{语义}，统一 IF NOT EXISTS，复合索引首列 tenant_id；
--   - 枚举用 CHECK 收口（Go 侧再判一次：mem 驱动没有 CHECK）；
--   - 整个文件在单事务内执行 → 禁止 CREATE INDEX CONCURRENTLY / VACUUM。
-- =============================================================================

-- ---------- 代理类型与城市 ----------
-- type 是「这条线路是什么」：isp（运营商）/ static_residential（静态住宅）/
-- residential（住宅）/ datacenter（机房）。它只用于台账筛选与备注展示，
-- 不参与任何启动链路判据——本期不做按类型的准入限制。
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS type text NOT NULL DEFAULT 'isp';
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS city text;

-- ---------- 拨测健康（与启停分离） ----------
-- check_status：unknown（默认/从未拨测）/ ok / fail。
-- last_error 只存**拨测结论文本**，且经 shop.trimErr 裁剪过：
-- 上游凭据绝不进这一列（详见 shop/proxy.go 的 trimErr）。
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS check_status text NOT NULL DEFAULT 'unknown';
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS exit_ip text;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS exit_country text;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS latency_ms integer;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS last_checked_at timestamptz;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS last_error text;

-- 绑定下拉按「拨测 ok」过滤，是这个索引存在的主要理由。
CREATE INDEX IF NOT EXISTS idx_proxies_tenant_check
    ON proxies (tenant_id, check_status);

-- ---------- CHECK（DB 侧收口；Go 侧必须再判一次，mem 驱动没有 CHECK） ----------
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_type_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_type_chk
    CHECK (type IN ('isp', 'static_residential', 'residential', 'datacenter'));

ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_check_status_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_check_status_chk
    CHECK (check_status IN ('unknown', 'ok', 'fail'));

-- 延迟与最近拨测时刻要么都没有、要么都有：半个结果（有延迟没时刻）会让
-- 「最近一次是什么时候通的」这个问题没有答案，列表页只能显示「-」。
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_check_pair_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_check_pair_chk
    CHECK ((latency_ms IS NULL) = (last_checked_at IS NULL));

-- ---------- 历史数据 ----------
-- 0013 是纯加列，全部新列都有默认值，历史行不需要回填：
-- 老代理的 check_status 保持 unknown —— 没拨测过就是没拨测过，
-- 回填成 ok 等于替运营做了一个他们没做过的验证。
