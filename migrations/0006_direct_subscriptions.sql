-- 服务器直出代理（D-25，docs/06 §9.2、docs/04 §6.1）。
--
-- 订阅分两类：isp（默认，已有订阅不变）经 ISP 上游出站；direct 由管理员赠送，经中转服务器自身地址出站，
-- 只限带宽不限流量，有效期跟随该用户的付费订阅（不存储，生成期望状态时计算）。
--
-- 直出订阅有接入（三个端口与凭据，换接入规则同 ISP 订阅），没有套餐、订单、出口绑定、上游资源、
-- 时长与流量账本条目。带宽上限与并发上限存在订阅行上（赠送时确定，管理员可改带宽）。
-- 生命周期只用 pending（赠送后待节点应用）、fulfill_failed（下发失败）、active、deleted（收回）。

ALTER TABLE subscriptions ADD COLUMN kind text NOT NULL DEFAULT 'isp' CHECK (kind IN ('isp', 'direct'));
ALTER TABLE subscriptions ADD COLUMN direct_bandwidth_mbps int CHECK (direct_bandwidth_mbps > 0);
ALTER TABLE subscriptions ADD COLUMN direct_max_conns int CHECK (direct_max_conns > 0);

-- 直出订阅没有套餐。
ALTER TABLE subscriptions ALTER COLUMN plan_id DROP NOT NULL;
ALTER TABLE subscriptions ALTER COLUMN plan_version_id DROP NOT NULL;

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_kind_shape CHECK (
    (kind = 'isp' AND plan_id IS NOT NULL AND plan_version_id IS NOT NULL
        AND direct_bandwidth_mbps IS NULL AND direct_max_conns IS NULL)
    OR (kind = 'direct' AND plan_id IS NULL AND plan_version_id IS NULL AND created_from_order_id IS NULL
        AND direct_bandwidth_mbps IS NOT NULL AND direct_max_conns IS NOT NULL
        AND lifecycle IN ('pending', 'fulfill_failed', 'active', 'deleted')
        AND current_exit_id IS NULL
        AND period_started_at IS NULL AND expires_at IS NULL AND retention_ends_at IS NULL AND released_at IS NULL
        AND entitled_bytes_total = 0 AND entitled_seconds_total = 0 AND seconds_before_period = 0)
);

-- 0001 的“有效/保留期必须有到期时间”只约束 ISP 订阅：直出订阅的可用截止不存储（跟随付费订阅计算）。
-- 0001 中该约束未命名（自动名 subscriptions_check2），按定义查找后替换。
DO $$
DECLARE
    c text;
    n int;
BEGIN
    SELECT min(conname), count(*) INTO c, n FROM pg_constraint
     WHERE conrelid = 'subscriptions'::regclass AND contype = 'c'
       AND pg_get_constraintdef(oid) LIKE '%lifecycle%active%retained%expires_at IS NOT NULL%'
       AND pg_get_constraintdef(oid) NOT LIKE '%kind%';
    IF n <> 1 THEN
        RAISE EXCEPTION '0006: 找不到唯一的“有效订阅必须有到期时间”约束（找到 % 个）', n;
    END IF;
    EXECUTE format('ALTER TABLE subscriptions DROP CONSTRAINT %I', c);
END $$;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_active_has_expiry
    CHECK (kind = 'direct' OR lifecycle NOT IN ('active', 'retained') OR expires_at IS NOT NULL);

-- 每个用户最多一个未删除的直出订阅（D-25）。并发赠送只有一个成功。
CREATE UNIQUE INDEX subscriptions_one_direct ON subscriptions (user_id) WHERE kind = 'direct' AND lifecycle <> 'deleted';
-- 生成期望状态与视图时按用户求付费订阅的最晚到期时间。
CREATE INDEX subscriptions_user_paid ON subscriptions (user_id, expires_at) WHERE kind = 'isp' AND lifecycle <> 'deleted';

-- 直出订阅没有出口绑定、流量与时长账本条目、订单（服务层先行给出友好错误，这里兜底）。
CREATE FUNCTION isp_reject_direct_subscription() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.subscription_id IS NOT NULL
       AND EXISTS (SELECT 1 FROM subscriptions s WHERE s.id = NEW.subscription_id AND s.kind = 'direct') THEN
        RAISE EXCEPTION '%: 直出订阅 % 没有出口绑定、账本条目与订单（D-25）', TG_TABLE_NAME, NEW.subscription_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER exit_bindings_isp_only BEFORE INSERT ON exit_bindings
    FOR EACH ROW EXECUTE FUNCTION isp_reject_direct_subscription();
CREATE TRIGGER ledger_entries_isp_only BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION isp_reject_direct_subscription();
CREATE TRIGGER orders_isp_only BEFORE INSERT OR UPDATE OF subscription_id ON orders
    FOR EACH ROW EXECUTE FUNCTION isp_reject_direct_subscription();

-- 赠送任务：为直出订阅签发首个接入并等节点应用（没有订单与出口，tasks_check4 只约束 fulfill）。
ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('access_change', 'password_reset', 'exit_change', 'combined', 'fulfill', 'direct_grant'));
