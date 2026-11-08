-- 共享出口套餐（D-24；docs/06 §9.1，docs/04 §7.6）。
--
-- 一个上游资源可以同时给至多 max_tenants 个共享订阅使用；独享订阅仍独占整份资源。
--
-- 名额模型：
--   * 独享占用沿用资源状态：reserved（未付款订单 / 换出口任务预留）、bound（已占用）、cooldown（冷却），
--     由 reserved_order_id / reserved_task_id / bound_subscription_id / cooldown_until 表达，行为与 0001 相同。
--   * 共享占用 = 待生效与生效的共享出口绑定（含换出口任务的待生效绑定，即任务预留）
--               + resource_slots 中的名额行（未付款订单预留、冷却中的名额）。
--     共享占用期间资源状态保持 available（或被隔离 / 停用），名额用满在视图里显示为“已占用”（R-16.3）。
--   * 同一资源不同时存在独享占用与共享占用；共享占用合计 ≤ max_tenants；max_tenants = 1 的资源只给独享。
--   这三条不变量由下面的触发器在锁住资源行之后检查，并发抢最后一个名额只有一个事务成功（A-47）。
--   名额行不按业务时钟判断过期：到期的订单预留与冷却名额由调度删除（与独享 reserved/cooldown 状态由调度回收一致）。
--
-- 同 IP 规则：独享订阅的出口 IP 在平台内唯一（exit_bindings_ip_live 改为只约束独享绑定）；
-- 共享订阅的出口 IP 不能是任何独享订阅的出口 IP（触发器按 IP 取咨询锁后检查）。同一共享资源上的订阅共用出口 IP。

-- 1. 套餐出口类型：exclusive（默认，已有套餐不变）或 shared。
ALTER TABLE plans ADD COLUMN exit_mode text NOT NULL DEFAULT 'exclusive' CHECK (exit_mode IN ('exclusive', 'shared'));

-- 2. 订单快照：新购订单下单时记录套餐的出口类型，套餐改动不追溯；续购订单沿用订阅，为空。
ALTER TABLE orders ADD COLUMN exit_mode text CHECK (exit_mode IN ('exclusive', 'shared'));
UPDATE orders SET exit_mode = 'exclusive' WHERE kind = 'new';
ALTER TABLE orders ADD CONSTRAINT orders_exit_mode_new CHECK ((kind = 'new') = (exit_mode IS NOT NULL));
-- 新购订单未显式给出时按套餐当前的出口类型快照（应用总是显式写入；直接写库的种子与测试由此补齐）。
CREATE FUNCTION isp_orders_exit_mode() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind = 'new' AND NEW.exit_mode IS NULL THEN
        NEW.exit_mode := (SELECT exit_mode FROM plans WHERE id = NEW.plan_id);
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER orders_exit_mode BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION isp_orders_exit_mode();

-- 3. 订阅出口类型：确认收款创建订阅时取新购订单的快照；重新履约沿用。
ALTER TABLE subscriptions ADD COLUMN exit_mode text NOT NULL DEFAULT 'exclusive' CHECK (exit_mode IN ('exclusive', 'shared'));

-- 4. 资源绑定上限（≥ 1）。应用录入时不填取配置 core.shared_exit_default_capacity（默认 3）；
--    列默认值 3 只用于直接写库（种子 SQL、测试），已有资源按“未设置用系统默认值”（01 §3）取 3。
ALTER TABLE upstream_resources ADD COLUMN max_tenants int NOT NULL DEFAULT 3 CHECK (max_tenants >= 1);

-- 5. 共享名额行：未付款订单预留（order_hold）与冷却中的名额（cooldown）。
CREATE TABLE resource_slots (
    id              text PRIMARY KEY DEFAULT isp_new_id('slt') CHECK (isp_id_ok(id, 'slt')),
    resource_id     text NOT NULL REFERENCES upstream_resources(id),
    kind            text NOT NULL CHECK (kind IN ('order_hold', 'cooldown')),
    order_id        text REFERENCES orders(id),
    -- 冷却名额：腾出名额的订阅（只给管理端追溯）。
    subscription_id text REFERENCES subscriptions(id),
    until           timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'order_hold') = (order_id IS NOT NULL))
);
CREATE UNIQUE INDEX resource_slots_one_per_order ON resource_slots (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX resource_slots_resource ON resource_slots (resource_id);
CREATE INDEX resource_slots_until ON resource_slots (kind, until);

-- 6. 出口绑定的出口类型：插入时由触发器从订阅取，应用不必（也不能）自行填写。
ALTER TABLE exit_bindings ADD COLUMN exit_mode text NOT NULL DEFAULT 'exclusive' CHECK (exit_mode IN ('exclusive', 'shared'));

CREATE FUNCTION isp_exit_bindings_mode() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.exit_mode := COALESCE((SELECT exit_mode FROM subscriptions WHERE id = NEW.subscription_id), 'exclusive');
    RETURN NEW;
END $$;
CREATE TRIGGER exit_bindings_mode BEFORE INSERT ON exit_bindings
    FOR EACH ROW EXECUTE FUNCTION isp_exit_bindings_mode();

-- 一个资源同一时刻只服务一个独享订阅；共享订阅另由名额检查约束。同名重建，应用的冲突映射不变。
DROP INDEX exit_bindings_resource_live;
CREATE UNIQUE INDEX exit_bindings_resource_live ON exit_bindings (resource_id)
    WHERE state IN ('pending', 'active') AND exit_mode = 'exclusive';
-- 独享出口 IP 平台内唯一（R-11.3）；共享与独享之间由触发器检查。
DROP INDEX exit_bindings_ip_live;
CREATE UNIQUE INDEX exit_bindings_ip_live ON exit_bindings (verified_ip)
    WHERE state IN ('pending', 'active') AND verified_ip IS NOT NULL AND exit_mode = 'exclusive';
-- 一个订阅在同一资源上至多占一个名额（待生效与生效绑定不能落在同一资源）。
CREATE UNIQUE INDEX exit_bindings_resource_sub_live ON exit_bindings (resource_id, subscription_id)
    WHERE state IN ('pending', 'active');
CREATE INDEX exit_bindings_shared_live ON exit_bindings (resource_id)
    WHERE state IN ('pending', 'active') AND exit_mode = 'shared';

-- 共享占用：待生效与生效的共享绑定 + 名额行。调用方须已锁住资源行（读已提交下每条语句取新快照）。
CREATE FUNCTION isp_shared_occupancy(res text) RETURNS int
LANGUAGE sql STABLE AS $$
    SELECT ((SELECT count(*) FROM exit_bindings b
              WHERE b.resource_id = res AND b.state IN ('pending', 'active') AND b.exit_mode = 'shared')
          + (SELECT count(*) FROM resource_slots s WHERE s.resource_id = res))::int
$$;

-- 资源上是否有独享占用（独享状态或待生效 / 生效的独享绑定）。
CREATE FUNCTION isp_exclusive_occupied(res text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM upstream_resources r WHERE r.id = res AND r.status IN ('reserved', 'bound', 'cooldown'))
        OR EXISTS (SELECT 1 FROM exit_bindings b
                    WHERE b.resource_id = res AND b.state IN ('pending', 'active') AND b.exit_mode = 'exclusive')
$$;

-- 出口绑定进入待生效 / 生效（或改资源、改验证 IP）时检查名额、混用与同 IP 规则。
CREATE FUNCTION isp_exit_bindings_tenancy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap int;
BEGIN
    IF NEW.state NOT IN ('pending', 'active') THEN
        RETURN NULL;
    END IF;
    -- 待生效 → 生效不改变占用。
    IF TG_OP = 'UPDATE' AND OLD.state IN ('pending', 'active') AND OLD.resource_id = NEW.resource_id
       AND OLD.verified_ip IS NOT DISTINCT FROM NEW.verified_ip THEN
        RETURN NULL;
    END IF;
    SELECT max_tenants INTO cap FROM upstream_resources WHERE id = NEW.resource_id FOR UPDATE;
    IF NEW.exit_mode = 'shared' THEN
        IF cap < 2 THEN
            RAISE EXCEPTION 'upstream_resources: 绑定上限为 % 的资源只给独享套餐', cap
                USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_shared_capacity';
        END IF;
        IF isp_exclusive_occupied(NEW.resource_id) THEN
            RAISE EXCEPTION 'upstream_resources: 资源已被独享订阅占用，不能再分给共享订阅'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_exit_mode_mixed';
        END IF;
        IF isp_shared_occupancy(NEW.resource_id) > cap THEN
            RAISE EXCEPTION 'upstream_resources: 共享名额已满（上限 %）', cap
                USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_shared_capacity';
        END IF;
    ELSIF isp_shared_occupancy(NEW.resource_id) > 0 THEN
        RAISE EXCEPTION 'upstream_resources: 资源上有共享订阅或共享名额，不能分给独享订阅'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_exit_mode_mixed';
    END IF;
    IF NEW.verified_ip IS NOT NULL THEN
        PERFORM pg_advisory_xact_lock(hashtext('exit_ip:' || host(NEW.verified_ip)));
        IF EXISTS (SELECT 1 FROM exit_bindings o
                    WHERE o.id <> NEW.id AND o.state IN ('pending', 'active') AND o.verified_ip = NEW.verified_ip
                      AND o.exit_mode <> NEW.exit_mode) THEN
            RAISE EXCEPTION 'exit_bindings: 出口 IP % 已被平台内其他订阅占用（独享与共享不能共用出口 IP）', host(NEW.verified_ip)
                USING ERRCODE = 'unique_violation', CONSTRAINT = 'exit_bindings_ip_live';
        END IF;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER exit_bindings_tenancy AFTER INSERT OR UPDATE OF state, resource_id, verified_ip ON exit_bindings
    FOR EACH ROW EXECUTE FUNCTION isp_exit_bindings_tenancy();

-- 订单预留名额：资源可共享、无独享占用、占用不超过上限。冷却名额替换刚解除的绑定，不增加占用，不检查。
CREATE FUNCTION isp_resource_slots_tenancy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap int;
BEGIN
    IF NEW.kind = 'cooldown' THEN
        RETURN NULL;
    END IF;
    SELECT max_tenants INTO cap FROM upstream_resources WHERE id = NEW.resource_id FOR UPDATE;
    IF cap < 2 THEN
        RAISE EXCEPTION 'upstream_resources: 绑定上限为 % 的资源只给独享套餐', cap
            USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_shared_capacity';
    END IF;
    IF isp_exclusive_occupied(NEW.resource_id) THEN
        RAISE EXCEPTION 'upstream_resources: 资源已被独享订阅占用，不能再分给共享订阅'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_exit_mode_mixed';
    END IF;
    IF isp_shared_occupancy(NEW.resource_id) > cap THEN
        RAISE EXCEPTION 'upstream_resources: 共享名额已满（上限 %）', cap
            USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_shared_capacity';
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER resource_slots_tenancy AFTER INSERT ON resource_slots
    FOR EACH ROW EXECUTE FUNCTION isp_resource_slots_tenancy();

-- 资源行：进入独享状态时不能有共享占用；修改绑定上限不能低于当前共享占用（有共享占用时也不能改为 1）。
CREATE FUNCTION isp_upstream_resources_tenancy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    occ int;
BEGIN
    IF NEW.status IN ('reserved', 'bound', 'cooldown') AND NEW.status IS DISTINCT FROM OLD.status
       AND isp_shared_occupancy(NEW.id) > 0 THEN
        RAISE EXCEPTION 'upstream_resources: 资源上有共享订阅或共享名额，不能分给独享订阅'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_exit_mode_mixed';
    END IF;
    IF NEW.max_tenants <> OLD.max_tenants THEN
        occ := isp_shared_occupancy(NEW.id);
        IF occ > 0 AND (NEW.max_tenants < 2 OR occ > NEW.max_tenants) THEN
            RAISE EXCEPTION 'upstream_resources: 当前共享占用 % 个名额，绑定上限不能改为 %', occ, NEW.max_tenants
                USING ERRCODE = 'check_violation', CONSTRAINT = 'upstream_resources_shared_capacity';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER upstream_resources_tenancy BEFORE UPDATE OF status, max_tenants ON upstream_resources
    FOR EACH ROW EXECUTE FUNCTION isp_upstream_resources_tenancy();
