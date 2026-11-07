-- 0001_init.sql —— ISP 代理平台控制面初始 schema（M1 契约冻结）。
--
-- 规则来源：docs/01（R-03、R-04、R-08～R-12、R-15～R-18、§7）、docs/04 §5.2 与 §7、docs/06。
-- 约定（docs/06 §3、§4）：
--   * 只前进：已发布的迁移文件不得修改，改动写新文件 NNNN_描述.sql。
--   * 金额为整数分（bigint），流量为字节（bigint），时长为秒（bigint），时间为 timestamptz（UTC）。
--   * 对外 ID 为“前缀_ + 20 位小写 Crockford base32”（100 位随机），由 isp_new_id() 生成；
--     应用层可用 store.NewID 生成同格式 ID。不使用自增整数作为对外 ID。
--   * 账本、版本表、审计只追加；订阅的权益合计只能由账本触发器改变；used_bytes_total 只增不减。
--   * schema_migrations 由迁移器（internal/control/store）在执行本文件之前创建，不在此定义。
--
-- 本文件在一个事务中执行。

-- ============================================================================
-- 0. 公共函数
-- ============================================================================

-- 生成对外 ID：prefix_ + 20 个字符（Crockford base32 小写，去掉 i l o u）。
-- 随机源为 gen_random_uuid()（pg_strong_random），只取完全随机的字节的低 5 位。
CREATE FUNCTION isp_new_id(prefix text) RETURNS text
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    alphabet constant text := '0123456789abcdefghjkmnpqrstvwxyz';
    b bytea := uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid());
    s text := '';
    i int;
BEGIN
    -- uuid v4 第 6 字节高 4 位为版本、第 8 字节高 2 位为变体，跳过这两个字节。
    FOREACH i IN ARRAY ARRAY[0,1,2,3,4,5,7,9,10,11,12,13,14,15,16,17,18,19,20,21] LOOP
        s := s || substr(alphabet, (get_byte(b, i) % 32) + 1, 1);
    END LOOP;
    RETURN prefix || '_' || s;
END
$$;

-- 校验对外 ID 格式。
CREATE FUNCTION isp_id_ok(id text, prefix text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT id ~ ('^' || prefix || '_[0-9a-hjkmnp-tv-z]{20}$')
$$;

-- 付款识别码：6 位大写字母数字（去掉易混字符），用户填在转账附言里。
CREATE FUNCTION isp_new_pay_ref() RETURNS text
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    alphabet constant text := 'ABCDEFGHJKMNPQRSTUVWXYZ23456789';
    b bytea := uuid_send(gen_random_uuid());
    s text := '';
    i int;
BEGIN
    FOREACH i IN ARRAY ARRAY[0,1,2,3,4,5] LOOP
        -- 256 不是 31 的倍数，有轻微偏差；识别码只用于核对，不是安全凭据。
        s := s || substr(alphabet, (get_byte(b, i) % 31) + 1, 1);
    END LOOP;
    RETURN s;
END
$$;

-- 只追加表：禁止 UPDATE / DELETE。
CREATE FUNCTION isp_forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% 是只追加表，禁止 %', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

-- 通用 updated_at。
CREATE FUNCTION isp_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

CREATE SEQUENCE order_no_seq;
CREATE SEQUENCE exit_request_no_seq;

-- ============================================================================
-- 1. 账号、管理员、会话、验证材料、限流
-- ============================================================================

CREATE TABLE admins (
    id               text PRIMARY KEY DEFAULT isp_new_id('adm') CHECK (isp_id_ok(id, 'adm')),
    email            text NOT NULL UNIQUE CHECK (email = lower(btrim(email)) AND email LIKE '%_@_%'),
    display_name     text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 64),
    -- 首期两种角色：admin 可写；viewer 只读（越权返回 forbidden）。管理员只由受控流程（CLI）创建。
    role             text NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'viewer')),
    password_hash    text NOT NULL,                       -- argon2id PHC 串
    -- TOTP 密钥用 ISP_SECRET_KEY 做 AES-256-GCM 加密（nonce || 密文）。为空表示尚未完成首次绑定。
    totp_secret_enc  bytea,
    totp_enrolled_at timestamptz,
    -- 最近一次被接受的 TOTP 时间步，防止同一验证码重放。
    totp_last_step   bigint NOT NULL DEFAULT 0,
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    last_login_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK ((totp_enrolled_at IS NULL) = (totp_secret_enc IS NULL))
);
CREATE TRIGGER admins_touch BEFORE UPDATE ON admins FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();

-- 管理员恢复码：一次性，只存哈希；绑定 TOTP 成功时生成 10 个，重新生成时旧码全部作废。
CREATE TABLE admin_recovery_codes (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    admin_id    text NOT NULL REFERENCES admins(id),
    code_hash   bytea NOT NULL,
    used_at     timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (admin_id, code_hash)
);

CREATE TABLE users (
    id                 text PRIMARY KEY DEFAULT isp_new_id('usr') CHECK (isp_id_ok(id, 'usr')),
    -- 规范化邮箱（去空白、小写）唯一（R-15.1）。
    email              text NOT NULL UNIQUE CHECK (email = lower(btrim(email)) AND email LIKE '%_@_%'),
    email_verified_at  timestamptz,
    -- argon2id PHC 串。管理员代建且本人尚未设置密码时为空（R-15.1）。
    password_hash      text,
    password_changed_at timestamptz,
    status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'banned')),
    banned_at          timestamptz,
    banned_reason      text,
    banned_by          text REFERENCES admins(id),
    timezone           text NOT NULL DEFAULT 'Asia/Shanghai',
    created_by_admin   text REFERENCES admins(id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'banned') = (banned_at IS NOT NULL))
);
CREATE TRIGGER users_touch BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();

-- 会话：用户与管理员分开（kind），Cookie 中只放随机令牌，库里存 SHA-256；可撤销（docs/06 §4）。
CREATE TABLE sessions (
    id            text PRIMARY KEY DEFAULT isp_new_id('ses') CHECK (isp_id_ok(id, 'ses')),
    kind          text NOT NULL CHECK (kind IN ('user', 'admin')),
    user_id       text REFERENCES users(id),
    admin_id      text REFERENCES admins(id),
    token_hash    bytea NOT NULL UNIQUE,
    -- 管理员登录分两步：密码通过后为 totp_pending（已绑定）或 enroll_pending（首次绑定），
    -- 第二步通过后为 active。用户会话只有 active。
    stage         text NOT NULL DEFAULT 'active' CHECK (stage IN ('active', 'totp_pending', 'enroll_pending')),
    -- 首次绑定期间暂存的待确认 TOTP 密钥（加密）。
    pending_totp_secret_enc bytea,
    ip            inet,
    user_agent    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoke_reason text CHECK (revoke_reason IN ('logout', 'password_changed', 'password_reset', 'banned', 'admin_revoked', 'superseded')),
    CHECK (expires_at > created_at),
    CHECK ((kind = 'user' AND user_id IS NOT NULL AND admin_id IS NULL AND stage = 'active')
        OR (kind = 'admin' AND admin_id IS NOT NULL AND user_id IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
);
CREATE INDEX sessions_user_live ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_admin_live ON sessions (admin_id) WHERE revoked_at IS NULL;

-- 邮件验证材料：用途绑定、有期限、尝试上限、一次性（R-15.2、D-15）。
-- 同一用户同一用途同一时刻只有一份有效材料；重发时旧材料置 superseded_at。
CREATE TABLE email_tokens (
    id            text PRIMARY KEY DEFAULT isp_new_id('etk') CHECK (isp_id_ok(id, 'etk')),
    user_id       text NOT NULL REFERENCES users(id),
    purpose       text NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    sent_to       text NOT NULL,                 -- 发送时的邮箱
    code_hash     bytea NOT NULL,                -- HMAC-SHA256(ISP_SECRET_KEY, code)
    attempts      int NOT NULL DEFAULT 0,
    max_attempts  int NOT NULL DEFAULT 5,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    consumed_at   timestamptz,
    superseded_at timestamptz,
    CHECK (attempts >= 0 AND max_attempts > 0 AND attempts <= max_attempts),
    CHECK (expires_at > created_at),
    CHECK (consumed_at IS NULL OR superseded_at IS NULL)
);
CREATE UNIQUE INDEX email_tokens_one_live ON email_tokens (user_id, purpose)
    WHERE consumed_at IS NULL AND superseded_at IS NULL;
CREATE INDEX email_tokens_sent ON email_tokens (sent_to, created_at);

-- 限流事件（滑动窗口计数）：发信、登录、注册、找回、自助操作。
-- scope 例：'login_email'、'login_ip'、'mail_send'；key 为规范化邮箱或来源 IP。由调度定期清理。
CREATE TABLE rate_events (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope  text NOT NULL,
    key    text NOT NULL,
    at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rate_events_lookup ON rate_events (scope, key, at);

-- 邮件发件箱：业务事务内写入，发送器异步投递；开发与测试投递到目录（D-15）。
CREATE TABLE mail_outbox (
    id          text PRIMARY KEY DEFAULT isp_new_id('mail') CHECK (isp_id_ok(id, 'mail')),
    to_email    text NOT NULL,
    template    text NOT NULL,             -- verify_email / reset_password / set_password / notice_*
    payload     jsonb NOT NULL DEFAULT '{}',
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed')),
    attempts    int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    sent_at     timestamptz
);
CREATE INDEX mail_outbox_queued ON mail_outbox (created_at) WHERE status = 'queued';

-- ============================================================================
-- 2. 规则、套餐、流量包（版本只追加）
-- ============================================================================

-- 规则版本：D-14a、D-18、D-19 等首期取值；下单时快照进订单（R-09.9）。
-- 当前规则 = effective_from <= now() 中最新的一条。
CREATE TABLE rules (
    version                     text PRIMARY KEY CHECK (version ~ '^R-[0-9]{4}-[0-9]{2}-[0-9]{2}(\.[0-9]+)?$'),
    effective_from              timestamptz NOT NULL DEFAULT now(),
    period_days                 int NOT NULL CHECK (period_days > 0),               -- 包月天数（30×24 小时）
    retention_days              int NOT NULL CHECK (retention_days >= 0),           -- D-14a
    port_cooldown_days          int NOT NULL CHECK (port_cooldown_days >= 0),       -- D-18
    resource_cooldown_days      int NOT NULL CHECK (resource_cooldown_days >= 0),   -- R-11.4，同 D-18
    access_changes_per_window   int NOT NULL CHECK (access_changes_per_window >= 0),-- D-18 自助换接入次数
    access_change_window_hours  int NOT NULL CHECK (access_change_window_hours > 0),
    free_exit_changes_per_window int NOT NULL CHECK (free_exit_changes_per_window >= 0), -- D-19
    exit_change_window_days     int NOT NULL CHECK (exit_change_window_days > 0),
    unpaid_hold_hours           int NOT NULL CHECK (unpaid_hold_hours > 0),         -- R-16.4
    max_unpaid_new_orders       int NOT NULL CHECK (max_unpaid_new_orders > 0),     -- R-16.4
    month_options               int[] NOT NULL CHECK (cardinality(month_options) > 0 AND 0 < ALL (month_options)),
    -- 人工收款说明（D-01）。为空时目录接口返回空串，前台提示“收款信息未配置”，不得下单。
    payment_payee               text NOT NULL DEFAULT '',
    payment_bank                text NOT NULL DEFAULT '',
    payment_account             text NOT NULL DEFAULT '',
    payment_note                text NOT NULL DEFAULT '',
    -- 面向客户的规则说明行，下单时连同具体增量一起写入 orders.rules_snapshot。
    snapshot_lines              text[] NOT NULL DEFAULT '{}',
    created_by                  text REFERENCES admins(id),
    created_at                  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rules_effective ON rules (effective_from DESC);
CREATE TRIGGER rules_append_only BEFORE UPDATE OR DELETE ON rules
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 首期取值（docs/06 §1）。收款信息由管理员在后台发布新规则版本时填写。
INSERT INTO rules (version, effective_from, period_days, retention_days, port_cooldown_days, resource_cooldown_days,
                   access_changes_per_window, access_change_window_hours, free_exit_changes_per_window,
                   exit_change_window_days, unpaid_hold_hours, max_unpaid_new_orders, month_options, snapshot_lines)
VALUES ('R-2026-11-08', '2026-11-08T00:00:00Z', 30, 7, 7, 7, 5, 24, 1, 30, 24, 3, '{1,3,6}', ARRAY[
    '时长按 30×24 小时计，首购在开通就绪时起算',
    '流量长期保留，不按月清零；到期和释放都不清余额',
    '时长到期或流量用尽任一发生，即停止全部转发',
    '到期后保留原出口与接入 7 天，期内续时长可恢复',
    '换接入每 24 小时 5 次；换出口每 30 天免费 1 次',
    '收款由管理员人工核对，确认前不开通、不入账'
]);

CREATE TABLE plans (
    id          text PRIMARY KEY DEFAULT isp_new_id('pln') CHECK (isp_id_ok(id, 'pln')),
    -- 管理用短代码，如 us-lax；不对客户承诺含义。
    code        text NOT NULL UNIQUE CHECK (code ~ '^[a-z0-9][a-z0-9-]{1,31}$'),
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'on_sale', 'off_sale')),
    sort_order  int NOT NULL DEFAULT 0,
    created_by  text REFERENCES admins(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER plans_touch BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();

-- 套餐版本只追加（C-W1）；当前版本 = 该套餐 version 最大的一条。订单与订阅引用具体版本。
CREATE TABLE plan_versions (
    id              text PRIMARY KEY DEFAULT isp_new_id('plv') CHECK (isp_id_ok(id, 'plv')),
    plan_id         text NOT NULL REFERENCES plans(id),
    version         int NOT NULL CHECK (version > 0),
    name            text NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    country_code    text NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),   -- ISO 3166-1 alpha-2 小写
    country_name    text NOT NULL,
    city            text NOT NULL,
    price_cents     bigint NOT NULL CHECK (price_cents >= 0),            -- 每个周期（period_days）的价格
    currency        text NOT NULL DEFAULT 'CNY' CHECK (currency = 'CNY'),
    period_days     int NOT NULL CHECK (period_days > 0),
    included_bytes  bigint NOT NULL CHECK (included_bytes >= 0),         -- 首购每个周期包含的流量
    concurrency     int NOT NULL CHECK (concurrency > 0),                -- 每订阅并发连接上限
    bandwidth_mbps  int NOT NULL CHECK (bandwidth_mbps > 0),             -- 1 Mbps = 1,000,000 bit/s
    created_by      text REFERENCES admins(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plan_id, version)
);
CREATE TRIGGER plan_versions_append_only BEFORE UPDATE OR DELETE ON plan_versions
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

CREATE TABLE traffic_packs (
    id          text PRIMARY KEY DEFAULT isp_new_id('tpk') CHECK (isp_id_ok(id, 'tpk')),
    code        text NOT NULL UNIQUE CHECK (code ~ '^[a-z0-9][a-z0-9-]{1,31}$'),
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'on_sale', 'off_sale')),
    sort_order  int NOT NULL DEFAULT 0,
    created_by  text REFERENCES admins(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER traffic_packs_touch BEFORE UPDATE ON traffic_packs FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();

CREATE TABLE traffic_pack_versions (
    id               text PRIMARY KEY DEFAULT isp_new_id('tpv') CHECK (isp_id_ok(id, 'tpv')),
    traffic_pack_id  text NOT NULL REFERENCES traffic_packs(id),
    version          int NOT NULL CHECK (version > 0),
    bytes            bigint NOT NULL CHECK (bytes > 0),
    price_cents      bigint NOT NULL CHECK (price_cents >= 0),
    currency         text NOT NULL DEFAULT 'CNY' CHECK (currency = 'CNY'),
    created_by       text REFERENCES admins(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (traffic_pack_id, version)
);
CREATE TRIGGER traffic_pack_versions_append_only BEFORE UPDATE OR DELETE ON traffic_pack_versions
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- ============================================================================
-- 3. 节点、端口池、上游资源
-- ============================================================================

CREATE TABLE nodes (
    id                text PRIMARY KEY DEFAULT isp_new_id('nod') CHECK (isp_id_ok(id, 'nod')),
    name              text NOT NULL UNIQUE,
    -- 客户连接使用的公共域名（R-01.5），如 proxy.example.com。
    public_host       text NOT NULL,
    port_range_start  int NOT NULL DEFAULT 20000 CHECK (port_range_start BETWEEN 1024 AND 65535),
    port_range_end    int NOT NULL DEFAULT 29999 CHECK (port_range_end BETWEEN 1024 AND 65535),
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'draining', 'disabled')),
    -- 期望状态全局修订号：任何影响该节点期望状态的写入都在同一事务内 +1（docs/06 §5）。
    desired_revision  bigint NOT NULL DEFAULT 0 CHECK (desired_revision >= 0),
    -- 以下为最近一次上报的观测值。
    applied_revision  bigint NOT NULL DEFAULT 0 CHECK (applied_revision >= 0),
    agent_version     text,
    persist_ok        boolean,
    clock_skew_ms     int,
    capabilities      jsonb NOT NULL DEFAULT '{}',
    last_seen_at      timestamptz,
    last_report_at    timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (port_range_end >= port_range_start)
);
CREATE TRIGGER nodes_touch BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();

-- 上游资源与库存状态（R-03、R-16.3）：
--   pending_verify 待验证 → available 可售 → reserved 已预留（未付款订单或进行中的换出口任务）
--   → bound 已占用 → cooldown 冷却（换出口/释放后，到期回到 available）；
--   quarantine 异常隔离、retired 停用。
CREATE TABLE upstream_resources (
    id               text PRIMARY KEY DEFAULT isp_new_id('res') CHECK (isp_id_ok(id, 'res')),
    node_id          text NOT NULL REFERENCES nodes(id),   -- 从哪个节点验证与使用
    protocol         text NOT NULL CHECK (protocol IN ('http', 'https', 'socks5')),
    host             text NOT NULL CHECK (length(host) BETWEEN 1 AND 253),  -- 域名、IPv4 或 IPv6（不含方括号）
    port             int NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username         text NOT NULL DEFAULT '',
    password_enc     bytea,                                 -- AES-256-GCM；无认证上游为空
    -- 供应商声明的地域（R-01.4），与检测结果分开展示。
    country_code     text NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
    country_name     text NOT NULL,
    city             text NOT NULL,
    provider         text NOT NULL DEFAULT '',              -- 供应商标签，只给管理端
    note             text NOT NULL DEFAULT '',
    status           text NOT NULL DEFAULT 'pending_verify'
                     CHECK (status IN ('pending_verify', 'available', 'reserved', 'bound', 'cooldown', 'quarantine', 'retired')),
    status_reason    text,
    reserved_order_id text,                                 -- 外键见 orders 建表之后
    reserved_task_id  text,                                 -- 外键见 tasks 建表之后
    reserved_until   timestamptz,
    bound_subscription_id text,                             -- 外键见 subscriptions 建表之后
    cooldown_until   timestamptz,
    -- 最近一次验证（R-03.2）。
    last_verified_at timestamptz,
    last_verify_ok   boolean,
    last_exit_ip     inet,
    last_exit_country_code text CHECK (last_exit_country_code ~ '^[a-z]{2}$'),
    last_error       text,
    row_version      bigint NOT NULL DEFAULT 0,
    created_by       text REFERENCES admins(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (protocol, host, port, username),
    CHECK ((status = 'reserved') = (reserved_until IS NOT NULL)),
    CHECK (status <> 'reserved' OR num_nonnulls(reserved_order_id, reserved_task_id) = 1),
    CHECK (status = 'reserved' OR (reserved_order_id IS NULL AND reserved_task_id IS NULL)),
    CHECK ((status = 'bound') = (bound_subscription_id IS NOT NULL)),
    CHECK ((status = 'cooldown') = (cooldown_until IS NOT NULL))
);
CREATE TRIGGER upstream_resources_touch BEFORE UPDATE ON upstream_resources FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();
CREATE INDEX upstream_resources_stock ON upstream_resources (country_code, city) WHERE status = 'available';
CREATE INDEX upstream_resources_status ON upstream_resources (status);
-- 平台内独享（D-13）：一个订阅同一时刻至多占用一个资源（另由 exit_bindings 约束保证一对一）。
CREATE UNIQUE INDEX upstream_resources_one_per_sub ON upstream_resources (bound_subscription_id)
    WHERE bound_subscription_id IS NOT NULL;
CREATE UNIQUE INDEX upstream_resources_one_per_order ON upstream_resources (reserved_order_id)
    WHERE reserved_order_id IS NOT NULL;

-- ============================================================================
-- 4. 订阅、接入版本、出口绑定
-- ============================================================================

-- 订阅：权益载体（R-04.1、R-18）。
-- 时长推导：entitled_seconds_total = 时长账本求和；seconds_before_period = 之前各段已耗尽的秒数；
--   当前段起点 period_started_at 不为空时，expires_at = period_started_at + (entitled_seconds_total - seconds_before_period)。
--   有效期内续时长：账本触发器直接顺延 expires_at；保留期内续时长/重新履约：应用把
--   seconds_before_period 设为续费前的 entitled_seconds_total，period_started_at 设为重新就绪时间。
--   该等式由提交时检查的约束触发器 subscriptions_time_consistent 保证。
-- 流量：entitled_bytes_total = 流量账本求和；used_bytes_total 只增不减（GREATEST 合并上报）。
--   可用余额 = entitled - used，允许短暂为负（透支 ≤ 1,000,000 字节，D-17），对外显示为 0。
CREATE TABLE subscriptions (
    id                     text PRIMARY KEY DEFAULT isp_new_id('sub') CHECK (isp_id_ok(id, 'sub')),
    user_id                text NOT NULL REFERENCES users(id),
    node_id                text REFERENCES nodes(id),
    plan_id                text NOT NULL REFERENCES plans(id),
    plan_version_id        text NOT NULL REFERENCES plan_versions(id),   -- 首购时的版本，决定并发/带宽/地域
    rules_version          text NOT NULL REFERENCES rules(version),      -- 最近一次购买时的规则版本
    created_from_order_id  text UNIQUE,                                  -- 新购订单；外键见 orders 之后。重复履约不会建第二个订阅
    name                   text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 24),
    lifecycle              text NOT NULL DEFAULT 'pending'
                           CHECK (lifecycle IN ('pending', 'fulfill_failed', 'active', 'retained', 'released', 'deleted')),
    lifecycle_reason       text,
    admin_paused           boolean NOT NULL DEFAULT false,
    paused_reason          text,
    paused_at              timestamptz,
    paused_by              text REFERENCES admins(id),
    entitled_bytes_total   bigint NOT NULL DEFAULT 0 CHECK (entitled_bytes_total >= 0),
    used_bytes_total       bigint NOT NULL DEFAULT 0 CHECK (used_bytes_total >= 0),
    entitled_seconds_total bigint NOT NULL DEFAULT 0 CHECK (entitled_seconds_total >= 0),
    seconds_before_period  bigint NOT NULL DEFAULT 0 CHECK (seconds_before_period >= 0),
    first_ready_at         timestamptz,                                  -- 首次就绪（起算）时间
    period_started_at      timestamptz,
    expires_at             timestamptz,
    retention_ends_at      timestamptz,
    released_at            timestamptz,
    deleted_at             timestamptz,
    current_access_id      text,                                         -- 外键见 access_versions 之后
    current_exit_id        text,                                         -- 外键见 exit_bindings 之后
    -- 下发给 agent 的订阅条目版本（docs/04 §7.2），接入、出口、额度、期限、启用任一变化即 +1。
    desired_version        bigint NOT NULL DEFAULT 0 CHECK (desired_version >= 0),
    -- agent 最近回报的已应用版本（GREATEST 合并）。
    applied_version        bigint NOT NULL DEFAULT 0 CHECK (applied_version >= 0),
    active_conns           int NOT NULL DEFAULT 0 CHECK (active_conns >= 0),
    usage_reported_at      timestamptz,
    -- 乐观锁：更新时 WHERE row_version = $old，并 SET row_version = row_version + 1。
    row_version            bigint NOT NULL DEFAULT 0,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, user_id),                                                -- 供划转的同用户复合外键
    CHECK (seconds_before_period <= entitled_seconds_total),
    CHECK ((period_started_at IS NULL) = (expires_at IS NULL)),
    CHECK (lifecycle NOT IN ('active', 'retained') OR expires_at IS NOT NULL),
    CHECK (lifecycle <> 'retained' OR retention_ends_at IS NOT NULL),
    CHECK (lifecycle <> 'released' OR released_at IS NOT NULL),
    CHECK ((lifecycle = 'deleted') = (deleted_at IS NOT NULL)),
    CHECK (admin_paused = (paused_at IS NOT NULL)),
    -- 履约异常、已释放、已删除的订阅没有生效的接入与出口（R-10.4、R-10.7）。
    CHECK (lifecycle IN ('pending', 'active', 'retained') OR (current_access_id IS NULL AND current_exit_id IS NULL))
);
CREATE INDEX subscriptions_user ON subscriptions (user_id, created_at DESC);
CREATE INDEX subscriptions_lifecycle ON subscriptions (lifecycle, expires_at);
CREATE INDEX subscriptions_node ON subscriptions (node_id) WHERE node_id IS NOT NULL;
CREATE INDEX subscriptions_retention ON subscriptions (retention_ends_at) WHERE lifecycle = 'retained';

-- 订阅行保护：
--   * 权益合计只能由账本触发器（pg_trigger_depth() >= 2）改变，不能直接写（R-08.1、R-08.8）；
--   * used_bytes_total、desired_version、applied_version 只增不减；
--   * 新订阅的合计必须为 0。
CREATE FUNCTION isp_subscriptions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.entitled_bytes_total <> 0 OR NEW.entitled_seconds_total <> 0 OR NEW.used_bytes_total <> 0 THEN
            RAISE EXCEPTION '新订阅的权益与用量必须从 0 开始，由账本条目入账' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.entitled_bytes_total, NEW.entitled_seconds_total) IS DISTINCT FROM (OLD.entitled_bytes_total, OLD.entitled_seconds_total)
       AND pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION '订阅 % 的权益合计只能通过追加账本条目改变', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.used_bytes_total < OLD.used_bytes_total THEN
        RAISE EXCEPTION '订阅 % 的 used_bytes_total 不能回退（% → %）', OLD.id, OLD.used_bytes_total, NEW.used_bytes_total
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.desired_version < OLD.desired_version OR NEW.applied_version < OLD.applied_version THEN
        RAISE EXCEPTION '订阅 % 的版本号不能回退', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$$;
CREATE TRIGGER subscriptions_guard BEFORE INSERT OR UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION isp_subscriptions_guard();

-- 时长等式在事务提交时检查，允许同一事务内先入账、再调整当前段。
CREATE FUNCTION isp_subscriptions_time_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s subscriptions%ROWTYPE;
BEGIN
    SELECT * INTO s FROM subscriptions WHERE id = NEW.id;
    IF NOT FOUND OR s.period_started_at IS NULL THEN
        RETURN NULL;
    END IF;
    IF s.expires_at <> s.period_started_at + make_interval(secs => s.entitled_seconds_total - s.seconds_before_period) THEN
        RAISE EXCEPTION '订阅 % 的到期时间与时长账本不一致', s.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER subscriptions_time_consistent
    AFTER INSERT OR UPDATE ON subscriptions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION isp_subscriptions_time_check();

-- 接入版本（R-04.2）：三个协议端口 + 连接用户名 + 连接密码。重置密码也产生新版本（端口不变）。
CREATE TABLE access_versions (
    id               text PRIMARY KEY DEFAULT isp_new_id('acc') CHECK (isp_id_ok(id, 'acc')),
    subscription_id  text NOT NULL REFERENCES subscriptions(id),
    version          int NOT NULL CHECK (version > 0),
    node_id          text NOT NULL REFERENCES nodes(id),
    change_kind      text NOT NULL CHECK (change_kind IN ('initial', 'password_reset', 'ports_and_password', 'refulfill')),
    username         text NOT NULL CHECK (username ~ '^[a-z0-9]{6,32}$'),
    password_enc     bytea NOT NULL,                                       -- AES-256-GCM（docs/06 §3）
    http_port        int NOT NULL CHECK (http_port BETWEEN 1 AND 65535),
    https_port       int NOT NULL CHECK (https_port BETWEEN 1 AND 65535),
    socks5_port      int NOT NULL CHECK (socks5_port BETWEEN 1 AND 65535),
    -- pending 已签发待节点应用；active 生效；revoked 已撤销；failed 下发失败（从未生效）。
    state            text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'active', 'revoked', 'failed')),
    -- 最近一次换接入改动、用户尚未确认的字段（前台高亮），由用户确认后清空。
    unseen_changes   text[] NOT NULL DEFAULT '{}' CHECK (unseen_changes <@ ARRAY['ports', 'password']::text[]),
    task_id          text,                                                 -- 外键见 tasks 之后
    issued_at        timestamptz NOT NULL DEFAULT now(),
    activated_at     timestamptz,
    revoked_at       timestamptz,
    revoke_reason    text,
    UNIQUE (subscription_id, version),
    CHECK (http_port <> https_port AND http_port <> socks5_port AND https_port <> socks5_port),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    CHECK (state <> 'active' OR activated_at IS NOT NULL)
);
-- 每个订阅同一时刻只有一个生效接入、至多一个待生效接入。
CREATE UNIQUE INDEX access_versions_one_active ON access_versions (subscription_id) WHERE state = 'active';
CREATE UNIQUE INDEX access_versions_one_pending ON access_versions (subscription_id) WHERE state = 'pending';
-- 连接用户名在节点上全局唯一（认证器按用户名找订阅）。
CREATE UNIQUE INDEX access_versions_username_live ON access_versions (username) WHERE state IN ('pending', 'active');

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_current_access_fk
    FOREIGN KEY (current_access_id) REFERENCES access_versions(id);

-- 端口池（R-04.6、D-18）：节点创建时由应用按端口范围预填 free 行。
-- allocated 归属某个接入版本（重置密码时迁移到新版本）；释放后进入 cooldown，到期回到 free。
CREATE TABLE ports (
    node_id           text NOT NULL REFERENCES nodes(id),
    port              int NOT NULL CHECK (port BETWEEN 1 AND 65535),
    state             text NOT NULL DEFAULT 'free' CHECK (state IN ('free', 'allocated', 'cooldown', 'blocked')),
    access_version_id text REFERENCES access_versions(id),
    cooldown_until    timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, port),
    CHECK ((state = 'allocated') = (access_version_id IS NOT NULL)),
    CHECK ((state = 'cooldown') = (cooldown_until IS NOT NULL))
);
CREATE TRIGGER ports_touch BEFORE UPDATE ON ports FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();
CREATE INDEX ports_free ON ports (node_id, port) WHERE state = 'free';
CREATE INDEX ports_cooldown ON ports (cooldown_until) WHERE state = 'cooldown';
CREATE INDEX ports_access ON ports (access_version_id) WHERE access_version_id IS NOT NULL;

-- 出口绑定版本（R-11）：订阅当前占用的上游资源及验证结果。
CREATE TABLE exit_bindings (
    id                 text PRIMARY KEY DEFAULT isp_new_id('exb') CHECK (isp_id_ok(id, 'exb')),
    subscription_id    text NOT NULL REFERENCES subscriptions(id),
    version            int NOT NULL CHECK (version > 0),
    resource_id        text NOT NULL REFERENCES upstream_resources(id),
    -- pending 已提交待节点应用；active 生效；unbound 已解绑；failed 切换失败（从未生效）。
    state              text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'active', 'unbound', 'failed')),
    -- 切换前经候选链验证得到的实际出口（R-11.2、R-11.3），作为平台内独占的判据。
    verified_ip        inet,
    verified_country_code text CHECK (verified_country_code ~ '^[a-z]{2}$'),
    -- 最近一次检测（R-05）。stale（过期）由读取时按 checked_at 判断，不落库。
    check_result       text CHECK (check_result IN ('ok', 'slow', 'failed')),
    check_ip           inet,
    check_country_code text CHECK (check_country_code ~ '^[a-z]{2}$'),
    check_city         text,
    check_asn          text,
    latency_ms         int CHECK (latency_ms >= 0),
    check_error        text,
    checked_at         timestamptz,
    task_id            text,                                         -- 外键见 tasks 之后
    bound_at           timestamptz,
    unbound_at         timestamptz,
    unbound_reason     text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subscription_id, version),
    CHECK ((state = 'unbound') = (unbound_at IS NOT NULL)),
    CHECK ((check_result IS NULL) = (checked_at IS NULL))
);
CREATE UNIQUE INDEX exit_bindings_one_active ON exit_bindings (subscription_id) WHERE state = 'active';
CREATE UNIQUE INDEX exit_bindings_one_pending ON exit_bindings (subscription_id) WHERE state = 'pending';
-- 一个上游资源同一时刻只服务一个订阅（D-13）。
CREATE UNIQUE INDEX exit_bindings_resource_live ON exit_bindings (resource_id) WHERE state IN ('pending', 'active');
-- 实际出口 IP 在平台内独占（R-11.3）：两个生效绑定不能有同一个验证出口。
CREATE UNIQUE INDEX exit_bindings_ip_live ON exit_bindings (verified_ip)
    WHERE state IN ('pending', 'active') AND verified_ip IS NOT NULL;

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_current_exit_fk
    FOREIGN KEY (current_exit_id) REFERENCES exit_bindings(id);
ALTER TABLE upstream_resources ADD CONSTRAINT upstream_resources_bound_sub_fk
    FOREIGN KEY (bound_subscription_id) REFERENCES subscriptions(id);

-- 订阅事件：面向客户的“时长与流量记录 / 接入与出口变更历史”与管理端时间线（R-17 IP 详情）。
-- 客户只看 customer_visible = true 的条目；detail 是给客户的中文说明，不含供应商信息。
CREATE TABLE subscription_events (
    id              text PRIMARY KEY DEFAULT isp_new_id('evt') CHECK (isp_id_ok(id, 'evt')),
    subscription_id text NOT NULL REFERENCES subscriptions(id),
    at              timestamptz NOT NULL DEFAULT now(),
    kind            text NOT NULL CHECK (kind IN (
                        'ready', 'time_added', 'traffic_added', 'password_reset', 'access_changed', 'exit_changed',
                        'expired', 'exhausted', 'renamed',
                        -- 以下默认只给管理端
                        'paused', 'resumed', 'released', 'deleted', 'adjusted', 'transferred', 'refunded',
                        'fulfill_failed', 'task_failed')),
    detail          text NOT NULL,
    customer_visible boolean NOT NULL DEFAULT true,
    order_id        text,                                            -- 外键见 orders 之后
    ledger_entry_id text,
    task_id         text,
    actor_kind      text NOT NULL DEFAULT 'system' CHECK (actor_kind IN ('user', 'admin', 'system', 'node')),
    actor_id        text
);
CREATE INDEX subscription_events_sub ON subscription_events (subscription_id, at DESC);
CREATE TRIGGER subscription_events_append_only BEFORE UPDATE OR DELETE ON subscription_events
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- ============================================================================
-- 5. 订单、收款、退款、账本、划转
-- ============================================================================

-- 订单（R-09、R-16）：收款与履约两个维度分开（§7.1）。
-- “含流量续费”= kind 'renew_time' 且同时有 traffic_pack 行；两项各自入账（R-18）。
CREATE TABLE orders (
    id                  text PRIMARY KEY DEFAULT isp_new_id('ord') CHECK (isp_id_ok(id, 'ord')),
    no                  text NOT NULL UNIQUE DEFAULT (
                            'ORD-' || to_char(now() AT TIME ZONE 'Asia/Shanghai', 'YYYYMMDD') || '-' ||
                            lpad(nextval('order_no_seq')::text, 4, '0')),
    user_id             text NOT NULL REFERENCES users(id),
    kind                text NOT NULL CHECK (kind IN ('new', 'renew_time', 'add_traffic')),
    -- 续购类订单绑定已有订阅；新购在确认收款时创建订阅（pending）并回填。
    subscription_id     text REFERENCES subscriptions(id),
    plan_id             text REFERENCES plans(id),
    plan_version_id     text REFERENCES plan_versions(id),
    months              int CHECK (months > 0),
    amount_cents        bigint NOT NULL CHECK (amount_cents >= 0),
    currency            text NOT NULL DEFAULT 'CNY' CHECK (currency = 'CNY'),
    payment_method      text NOT NULL DEFAULT 'manual_transfer' CHECK (payment_method IN ('manual_transfer', 'admin_gift')),
    payment_status      text NOT NULL DEFAULT 'awaiting'
                        CHECK (payment_status IN ('awaiting', 'confirmed', 'cancelled', 'refund_review', 'refunded')),
    fulfillment_status  text NOT NULL DEFAULT 'not_started'
                        CHECK (fulfillment_status IN ('not_started', 'processing', 'delivered', 'failed')),
    fulfillment_error   text,                                        -- 面向客户的交付异常说明
    -- 下单时的规则版本与快照（R-09.9），套餐改价或下架不追溯。
    rules_version       text NOT NULL REFERENCES rules(version),
    rules_snapshot      jsonb NOT NULL,                              -- {"lines": [...], "rules": {...}}
    pay_ref             text NOT NULL DEFAULT isp_new_pay_ref() CHECK (pay_ref ~ '^[A-Z0-9]{4,12}$'),
    -- 未付款保留截止（R-16.4，新购同时预留库存）；到点由调度取消并回收。
    hold_until          timestamptz,
    user_claimed_paid_at timestamptz,                                -- 用户“已转账”反馈，只是核对线索（R-16.5）
    confirmed_at        timestamptz,
    delivered_at        timestamptz,
    cancelled_at        timestamptz,
    cancel_reason       text CHECK (cancel_reason IN ('user', 'timeout', 'admin')),
    created_by_admin    text REFERENCES admins(id),                  -- 管理员代建（P0a）
    row_version         bigint NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (kind <> 'new' OR (plan_id IS NOT NULL AND plan_version_id IS NOT NULL AND months IS NOT NULL)),
    CHECK (kind = 'new' OR subscription_id IS NOT NULL),
    CHECK (kind <> 'renew_time' OR months IS NOT NULL),
    CHECK (payment_status <> 'awaiting' OR hold_until IS NOT NULL),
    -- 晚到款（R-16.6）确认后状态离开 cancelled，cancelled_at 保留作为历史。
    CHECK (payment_status <> 'cancelled' OR cancelled_at IS NOT NULL),
    CHECK (payment_status IN ('awaiting', 'cancelled') OR confirmed_at IS NOT NULL),
    -- 履约只能在收款确认之后开始（R-09.2）。
    CHECK (fulfillment_status = 'not_started' OR payment_status IN ('confirmed', 'refund_review', 'refunded')),
    CHECK ((fulfillment_status = 'delivered') = (delivered_at IS NOT NULL)),
    CHECK (fulfillment_status <> 'failed' OR fulfillment_error IS NOT NULL)
);
CREATE TRIGGER orders_touch BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();
CREATE INDEX orders_user ON orders (user_id, created_at DESC);
CREATE INDEX orders_subscription ON orders (subscription_id, created_at DESC) WHERE subscription_id IS NOT NULL;
CREATE INDEX orders_status ON orders (payment_status, fulfillment_status, created_at);
CREATE INDEX orders_hold ON orders (hold_until) WHERE payment_status = 'awaiting';
-- 付款识别码在待核款订单中唯一，便于管理员按附言找订单。
CREATE UNIQUE INDEX orders_pay_ref_awaiting ON orders (pay_ref) WHERE payment_status = 'awaiting';

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_created_from_order_fk
    FOREIGN KEY (created_from_order_id) REFERENCES orders(id);
ALTER TABLE upstream_resources ADD CONSTRAINT upstream_resources_reserved_order_fk
    FOREIGN KEY (reserved_order_id) REFERENCES orders(id);
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_order_fk
    FOREIGN KEY (order_id) REFERENCES orders(id);

-- 订单行：每行一个商品项与它带来的权益增量。
--   plan        新购：时长 + 流量（included_bytes × 月数）
--   renew_time  续时长：只有时长
--   traffic_pack 买流量：只有流量
CREATE TABLE order_lines (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id                text NOT NULL REFERENCES orders(id),
    line_no                 int NOT NULL CHECK (line_no > 0),
    item_kind               text NOT NULL CHECK (item_kind IN ('plan', 'renew_time', 'traffic_pack')),
    plan_version_id         text REFERENCES plan_versions(id),
    traffic_pack_version_id text REFERENCES traffic_pack_versions(id),
    label                   text NOT NULL,
    detail                  text NOT NULL,
    amount_cents            bigint NOT NULL CHECK (amount_cents >= 0),
    seconds_delta           bigint NOT NULL DEFAULT 0 CHECK (seconds_delta >= 0),
    bytes_delta             bigint NOT NULL DEFAULT 0 CHECK (bytes_delta >= 0),
    UNIQUE (order_id, line_no),
    CHECK (item_kind <> 'plan' OR (plan_version_id IS NOT NULL AND seconds_delta > 0)),
    CHECK (item_kind <> 'renew_time' OR (plan_version_id IS NOT NULL AND seconds_delta > 0 AND bytes_delta = 0)),
    CHECK (item_kind <> 'traffic_pack' OR (traffic_pack_version_id IS NOT NULL AND bytes_delta > 0 AND seconds_delta = 0))
);
CREATE TRIGGER order_lines_append_only BEFORE UPDATE OR DELETE ON order_lines
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 人工收款确认记录（R-09.3）：以订单为业务键，每个订单至多一条，重复确认返回已有记录。
CREATE TABLE payments (
    id            text PRIMARY KEY DEFAULT isp_new_id('pay') CHECK (isp_id_ok(id, 'pay')),
    order_id      text NOT NULL UNIQUE REFERENCES orders(id),
    amount_cents  bigint NOT NULL CHECK (amount_cents >= 0),
    currency      text NOT NULL DEFAULT 'CNY' CHECK (currency = 'CNY'),
    method        text NOT NULL CHECK (method IN ('manual_transfer', 'admin_gift')),
    external_ref  text NOT NULL,              -- 外部收款参考（流水号等）；赠送时写原因
    late_arrival  boolean NOT NULL DEFAULT false,   -- 订单已取消/超时后到账（R-16.6）
    note          text NOT NULL DEFAULT '',
    confirmed_by  text NOT NULL REFERENCES admins(id),
    confirmed_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER payments_append_only BEFORE UPDATE OR DELETE ON payments
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 退款处置（R-09.6）：首期不自动退款，管理员复核后记录；批准时追加负向账本条目回收剩余权益。
CREATE TABLE refunds (
    id               text PRIMARY KEY DEFAULT isp_new_id('rfd') CHECK (isp_id_ok(id, 'rfd')),
    order_id         text NOT NULL REFERENCES orders(id),
    status           text NOT NULL DEFAULT 'review' CHECK (status IN ('review', 'approved', 'rejected')),
    amount_cents     bigint NOT NULL CHECK (amount_cents >= 0),
    reclaim_bytes    bigint NOT NULL DEFAULT 0 CHECK (reclaim_bytes >= 0),
    reclaim_seconds  bigint NOT NULL DEFAULT 0 CHECK (reclaim_seconds >= 0),
    reason           text NOT NULL,
    external_ref     text,                    -- 退款流水
    requested_by     text NOT NULL REFERENCES admins(id),
    requested_at     timestamptz NOT NULL DEFAULT now(),
    decided_by       text REFERENCES admins(id),
    decided_at       timestamptz,
    decision_note    text,
    CHECK ((status = 'review') = (decided_at IS NULL))
);
CREATE UNIQUE INDEX refunds_one_open ON refunds (order_id) WHERE status = 'review';

-- 同用户订阅间划转（R-10.6，首期仅管理员）：以成对账本条目记录。复合外键保证两端属于同一用户。
CREATE TABLE transfers (
    id                   text PRIMARY KEY DEFAULT isp_new_id('trf') CHECK (isp_id_ok(id, 'trf')),
    user_id              text NOT NULL REFERENCES users(id),
    from_subscription_id text NOT NULL,
    to_subscription_id   text NOT NULL,
    bytes                bigint NOT NULL CHECK (bytes > 0),
    reason               text NOT NULL,
    admin_id             text NOT NULL REFERENCES admins(id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (from_subscription_id <> to_subscription_id),
    FOREIGN KEY (from_subscription_id, user_id) REFERENCES subscriptions(id, user_id),
    FOREIGN KEY (to_subscription_id, user_id) REFERENCES subscriptions(id, user_id)
);
CREATE TRIGGER transfers_append_only BEFORE UPDATE OR DELETE ON transfers
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 权益账本（R-08、R-09、R-18；docs/04 §7.3）：只追加，带符号。
--   entitlement_kind = time：amount 为秒；traffic：amount 为字节。
--   业务来源键 (source_type, source_id, entitlement_kind) 唯一：重复确认、重试、并发只能入账一次。
--     order        source_id = 订单 ID（时长、流量各一条）
--     adjust / gift / compensation  source_id = 管理员操作 ID（adj_…，同一幂等键复用同一 ID）
--     refund       source_id = 退款 ID
--     transfer_out / transfer_in    source_id = 划转 ID，两条成对
CREATE TABLE ledger_entries (
    id               text PRIMARY KEY DEFAULT isp_new_id('led') CHECK (isp_id_ok(id, 'led')),
    subscription_id  text NOT NULL REFERENCES subscriptions(id),
    entitlement_kind text NOT NULL CHECK (entitlement_kind IN ('time', 'traffic')),
    amount           bigint NOT NULL CHECK (amount <> 0),
    source_type      text NOT NULL CHECK (source_type IN
                         ('order', 'adjust', 'gift', 'compensation', 'refund', 'transfer_out', 'transfer_in')),
    source_id        text NOT NULL,
    order_id         text REFERENCES orders(id),
    refund_id        text REFERENCES refunds(id),
    transfer_id      text REFERENCES transfers(id),
    reason           text NOT NULL DEFAULT '',
    admin_id         text REFERENCES admins(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_source_key UNIQUE (source_type, source_id, entitlement_kind),
    CHECK (source_type <> 'order' OR (order_id IS NOT NULL AND source_id = order_id AND amount > 0)),
    CHECK (source_type <> 'refund' OR (refund_id IS NOT NULL AND source_id = refund_id AND amount < 0)),
    CHECK (source_type <> 'transfer_out' OR (transfer_id IS NOT NULL AND source_id = transfer_id
                                              AND entitlement_kind = 'traffic' AND amount < 0)),
    CHECK (source_type <> 'transfer_in' OR (transfer_id IS NOT NULL AND source_id = transfer_id
                                             AND entitlement_kind = 'traffic' AND amount > 0)),
    CHECK (source_type IN ('order', 'refund', 'transfer_out', 'transfer_in') OR admin_id IS NOT NULL),
    CHECK (source_type NOT IN ('gift', 'compensation') OR amount > 0)
);
CREATE INDEX ledger_entries_sub ON ledger_entries (subscription_id, created_at);
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 入账即在同一事务内更新订阅合计；当前段已起算时同步顺延/提前到期时间。
-- 合计受 CHECK (>= 0) 约束，扣回超过余额会整体失败，不会产生负权益。
-- 必须是 AFTER 触发器：INSERT … ON CONFLICT DO NOTHING 跳过的重复条目不会触发，重复确认不会重复加权益。
CREATE FUNCTION isp_ledger_apply() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.entitlement_kind = 'traffic' THEN
        UPDATE subscriptions SET entitled_bytes_total = entitled_bytes_total + NEW.amount
         WHERE id = NEW.subscription_id;
    ELSE
        UPDATE subscriptions
           SET entitled_seconds_total = entitled_seconds_total + NEW.amount,
               expires_at = CASE WHEN period_started_at IS NULL THEN NULL
                                 ELSE expires_at + make_interval(secs => NEW.amount) END
         WHERE id = NEW.subscription_id;
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER ledger_entries_apply AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION isp_ledger_apply();

-- 划转必须成对：提交时检查每个划转恰有一出一入、金额相反。
CREATE FUNCTION isp_transfer_pair_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    n_out int; n_in int; s bigint;
BEGIN
    SELECT count(*) FILTER (WHERE source_type = 'transfer_out'),
           count(*) FILTER (WHERE source_type = 'transfer_in'),
           coalesce(sum(amount), 0)
      INTO n_out, n_in, s
      FROM ledger_entries WHERE transfer_id = NEW.id;
    IF n_out <> 1 OR n_in <> 1 OR s <> 0 THEN
        RAISE EXCEPTION '划转 % 必须恰有一出一入两条账本条目且金额相反', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER transfers_paired
    AFTER INSERT ON transfers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION isp_transfer_pair_check();

ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_ledger_fk
    FOREIGN KEY (ledger_entry_id) REFERENCES ledger_entries(id);

-- ============================================================================
-- 6. 变更任务、换出口申请、检测
-- ============================================================================

-- 变更任务（C-W5）：换接入、重置密码、换出口、组合更换、履约。
-- 声明式对账：任务写入期望状态并记录 target_version；节点回报 applied_version >= target_version 即完成。
CREATE TABLE tasks (
    id                   text PRIMARY KEY DEFAULT isp_new_id('tsk') CHECK (isp_id_ok(id, 'tsk')),
    subscription_id      text NOT NULL REFERENCES subscriptions(id),
    kind                 text NOT NULL CHECK (kind IN ('access_change', 'password_reset', 'exit_change', 'combined', 'fulfill')),
    state                text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    requested_by_kind    text NOT NULL CHECK (requested_by_kind IN ('user', 'admin', 'system')),
    requested_by_user    text REFERENCES users(id),
    requested_by_admin   text REFERENCES admins(id),
    order_id             text REFERENCES orders(id),
    exit_request_id      text,                                       -- 外键见 exit_requests 之后
    new_access_id        text REFERENCES access_versions(id),
    new_exit_id          text REFERENCES exit_bindings(id),
    params               jsonb NOT NULL DEFAULT '{}',
    target_version       bigint CHECK (target_version > 0),
    attempts             int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    failure_code         text,
    failure_reason       text,                                       -- 面向管理员的失败原因
    created_at           timestamptz NOT NULL DEFAULT now(),
    started_at           timestamptz,
    finished_at          timestamptz,
    CHECK ((requested_by_kind = 'user') = (requested_by_user IS NOT NULL)),
    CHECK ((requested_by_kind = 'admin') = (requested_by_admin IS NOT NULL)),
    CHECK ((state IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CHECK (state <> 'failed' OR failure_reason IS NOT NULL),
    CHECK (kind <> 'fulfill' OR order_id IS NOT NULL)
);
-- 每订阅同一时刻至多一个进行中的任务（部分唯一索引，docs/04 §7.3）。
CREATE UNIQUE INDEX tasks_one_in_progress ON tasks (subscription_id) WHERE state IN ('queued', 'running');
CREATE INDEX tasks_sub ON tasks (subscription_id, created_at DESC);
-- 自助换接入频率按滚动窗口计数（D-18）。
CREATE INDEX tasks_user_access_changes ON tasks (subscription_id, created_at)
    WHERE requested_by_kind = 'user' AND kind IN ('access_change', 'password_reset');
CREATE INDEX tasks_open ON tasks (created_at) WHERE state IN ('queued', 'running');

ALTER TABLE access_versions ADD CONSTRAINT access_versions_task_fk FOREIGN KEY (task_id) REFERENCES tasks(id);
ALTER TABLE exit_bindings ADD CONSTRAINT exit_bindings_task_fk FOREIGN KEY (task_id) REFERENCES tasks(id);
ALTER TABLE upstream_resources ADD CONSTRAINT upstream_resources_reserved_task_fk
    FOREIGN KEY (reserved_task_id) REFERENCES tasks(id);
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_task_fk FOREIGN KEY (task_id) REFERENCES tasks(id);

-- 换出口申请（R-11、R-17 我的申请、D-19、C-W11）。
-- charge：free 免费次数内；manual 超出免费次数，需管理员确认收费（fee_status）后才能切换。
-- 客户视图不含 internal_note、候选资源与供应商信息。
CREATE TABLE exit_requests (
    id                 text PRIMARY KEY DEFAULT isp_new_id('req') CHECK (isp_id_ok(id, 'req')),
    no                 text NOT NULL UNIQUE DEFAULT (
                           'REQ-' || to_char(now() AT TIME ZONE 'Asia/Shanghai', 'MMDD') || '-' ||
                           lpad(nextval('exit_request_no_seq')::text, 4, '0')),
    subscription_id    text NOT NULL,
    user_id            text NOT NULL REFERENCES users(id),
    reason             text NOT NULL CHECK (reason IN ('target_blocked', 'quality', 'region', 'other')),
    note               text NOT NULL DEFAULT '' CHECK (length(note) <= 200),
    also_change_access boolean NOT NULL DEFAULT false,
    charge             text NOT NULL CHECK (charge IN ('free', 'manual')),
    fee_status         text NOT NULL CHECK (fee_status IN ('not_applicable', 'awaiting_decision', 'charged', 'waived')),
    fee_amount_cents   bigint CHECK (fee_amount_cents >= 0),
    fee_external_ref   text,
    status             text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'succeeded', 'failed', 'cancelled')),
    old_ip             inet,
    new_ip             inet,
    failure_reason     text,                      -- 面向客户
    internal_note      text NOT NULL DEFAULT '',  -- 只给管理端
    task_id            text REFERENCES tasks(id),
    handled_by         text REFERENCES admins(id),
    accepted_at        timestamptz,
    finished_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (subscription_id, user_id) REFERENCES subscriptions(id, user_id),
    CHECK ((charge = 'free') = (fee_status = 'not_applicable')),
    CHECK (fee_status <> 'charged' OR (fee_amount_cents IS NOT NULL AND fee_external_ref IS NOT NULL)),
    -- D-19：收费未确认前不得进入处理。
    CHECK (status NOT IN ('processing', 'succeeded') OR fee_status <> 'awaiting_decision'),
    CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CHECK (status <> 'failed' OR failure_reason IS NOT NULL),
    CHECK (status <> 'succeeded' OR new_ip IS NOT NULL)
);
CREATE TRIGGER exit_requests_touch BEFORE UPDATE ON exit_requests FOR EACH ROW EXECUTE FUNCTION isp_touch_updated_at();
-- 同一订阅未完成的申请只保留一条（R-17.3、C-W11）。
CREATE UNIQUE INDEX exit_requests_one_open ON exit_requests (subscription_id) WHERE status IN ('pending', 'processing');
CREATE INDEX exit_requests_user ON exit_requests (user_id, created_at DESC);
CREATE INDEX exit_requests_status ON exit_requests (status, created_at);
-- 免费次数按滚动窗口计数（D-19）：失败、取消的申请不计入。
CREATE INDEX exit_requests_free_window ON exit_requests (subscription_id, created_at)
    WHERE charge = 'free' AND status IN ('pending', 'processing', 'succeeded');

ALTER TABLE tasks ADD CONSTRAINT tasks_exit_request_fk FOREIGN KEY (exit_request_id) REFERENCES exit_requests(id);

-- 节点探测（R-05、R-11.2；docs/06 §5）：作为期望状态的 probes 下发，结果经 probe-results 回传，按 ID 幂等。
--   exit_check         经订阅当前出口检测
--   candidate_compare  经候选资源检测，并与当前出口比较（不同、平台内独占、国家符合）
--   upstream_verify    录入后验证上游认证、连通性与实际出口
CREATE TABLE probes (
    id                  text PRIMARY KEY DEFAULT isp_new_id('prb') CHECK (isp_id_ok(id, 'prb')),
    node_id             text NOT NULL REFERENCES nodes(id),
    kind                text NOT NULL CHECK (kind IN ('exit_check', 'candidate_compare', 'upstream_verify')),
    subscription_id     text REFERENCES subscriptions(id),
    resource_id         text REFERENCES upstream_resources(id),
    exit_binding_id     text REFERENCES exit_bindings(id),
    requested_by_kind   text NOT NULL DEFAULT 'system' CHECK (requested_by_kind IN ('admin', 'system')),
    requested_by_admin  text REFERENCES admins(id),
    status              text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'dispatched', 'done', 'expired')),
    result              text CHECK (result IN ('ok', 'slow', 'failed')),
    observed_ip         inet,
    observed_country_code text CHECK (observed_country_code ~ '^[a-z]{2}$'),
    observed_city       text,
    observed_asn        text,
    latency_ms          int CHECK (latency_ms >= 0),
    error               text,
    -- candidate_compare 的比较结论（控制面计算）。
    differs_from_current boolean,
    unique_on_platform  boolean,
    country_matches     boolean,
    deadline_at         timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    dispatched_at       timestamptz,
    completed_at        timestamptz,
    node_time           timestamptz,
    CHECK ((status = 'done') = (completed_at IS NOT NULL)),
    CHECK ((status = 'done') = (result IS NOT NULL)),
    CHECK (kind <> 'exit_check' OR exit_binding_id IS NOT NULL),
    CHECK (kind <> 'candidate_compare' OR (subscription_id IS NOT NULL AND resource_id IS NOT NULL)),
    CHECK (kind <> 'upstream_verify' OR resource_id IS NOT NULL)
);
CREATE INDEX probes_pending ON probes (node_id, created_at) WHERE status IN ('queued', 'dispatched');
CREATE INDEX probes_sub ON probes (subscription_id, created_at DESC) WHERE subscription_id IS NOT NULL;
CREATE INDEX probes_resource ON probes (resource_id, created_at DESC) WHERE resource_id IS NOT NULL;

-- ============================================================================
-- 7. 用量上报与趋势
-- ============================================================================

-- 节点上报（每 ≤ 5 秒一次）。只保存头信息；各订阅数值见 usage_samples。
CREATE TABLE usage_reports (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id          text NOT NULL REFERENCES nodes(id),
    received_at      timestamptz NOT NULL DEFAULT now(),
    node_time        timestamptz NOT NULL,
    applied_revision bigint NOT NULL CHECK (applied_revision >= 0),
    persist_ok       boolean NOT NULL,
    subscriptions    int NOT NULL CHECK (subscriptions >= 0)
);
CREATE INDEX usage_reports_node ON usage_reports (node_id, received_at DESC);

-- 每次上报中每个订阅一行：累计值（不是增量）。趋势 = 相邻样本差值；监控不作为扣量依据（R-19.2）。
-- 保留 90 天（D-07 建议），由调度按 sampled_at 删除；禁止修改。
CREATE TABLE usage_samples (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    report_id        bigint NOT NULL REFERENCES usage_reports(id) ON DELETE CASCADE,
    subscription_id  text NOT NULL REFERENCES subscriptions(id),
    sampled_at       timestamptz NOT NULL,          -- 节点时钟
    used_total       bigint NOT NULL CHECK (used_total >= 0),
    used_by_protocol jsonb,                         -- {"http": n, "https": n, "socks5": n}，累计值
    active_conns     int NOT NULL CHECK (active_conns >= 0),
    applied_version  bigint NOT NULL CHECK (applied_version >= 0)
);
CREATE INDEX usage_samples_sub ON usage_samples (subscription_id, sampled_at);
CREATE INDEX usage_samples_time ON usage_samples (sampled_at);
CREATE TRIGGER usage_samples_no_update BEFORE UPDATE ON usage_samples
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- ============================================================================
-- 8. 告警、审计、幂等
-- ============================================================================

-- 告警事件（R-19.3、C-W9）。同一 dedupe_key 未解决时只保留一条，重复发生累加 occurrences。
CREATE TABLE alerts (
    id              text PRIMARY KEY DEFAULT isp_new_id('alt') CHECK (isp_id_ok(id, 'alt')),
    kind            text NOT NULL CHECK (kind IN (
                        'node_offline', 'upstream_error', 'config_failed', 'cert_risk', 'persist_failed', 'clock_skew',
                        'fulfill_failed', 'exit_change_failed', 'port_pool_low', 'stock_low', 'late_payment')),
    severity        text NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'acked', 'resolved')),
    dedupe_key      text NOT NULL,
    message         text NOT NULL,
    details         jsonb NOT NULL DEFAULT '{}',
    node_id         text REFERENCES nodes(id),
    subscription_id text REFERENCES subscriptions(id),
    resource_id     text REFERENCES upstream_resources(id),
    order_id        text REFERENCES orders(id),
    task_id         text REFERENCES tasks(id),
    occurrences     int NOT NULL DEFAULT 1 CHECK (occurrences > 0),
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    acked_by        text REFERENCES admins(id),
    acked_at        timestamptz,
    ack_note        text,
    resolved_at     timestamptz,
    CHECK (status <> 'acked' OR acked_at IS NOT NULL),
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX alerts_one_unresolved ON alerts (dedupe_key) WHERE status <> 'resolved';
CREATE INDEX alerts_status ON alerts (status, last_seen_at DESC);

-- 操作审计（R-12 审计、R-15.6、C-W8）：只追加，不记录明文凭据。
CREATE TABLE audit_logs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor_kind  text NOT NULL CHECK (actor_kind IN ('admin', 'user', 'system', 'node')),
    admin_id    text REFERENCES admins(id),
    user_id     text REFERENCES users(id),
    node_id     text REFERENCES nodes(id),
    -- 动作名用“对象.动作”，如 order.confirm_payment、subscription.pause、access.reveal、admin.login_failed。
    action      text NOT NULL CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
    target_type text,
    target_id   text,
    outcome     text NOT NULL DEFAULT 'ok' CHECK (outcome IN ('ok', 'denied', 'failed')),
    ip          inet,
    user_agent  text,
    request_id  text,
    details     jsonb NOT NULL DEFAULT '{}',
    CHECK (actor_kind <> 'admin' OR admin_id IS NOT NULL OR outcome <> 'ok')
);
CREATE INDEX audit_logs_at ON audit_logs (at DESC);
CREATE INDEX audit_logs_target ON audit_logs (target_type, target_id, at DESC);
CREATE INDEX audit_logs_admin ON audit_logs (admin_id, at DESC) WHERE admin_id IS NOT NULL;
CREATE INDEX audit_logs_action ON audit_logs (action, at DESC);
CREATE TRIGGER audit_logs_append_only BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION isp_forbid_mutation();

-- 写接口幂等（docs/06 §3）：scope 为“主体 + 操作”，如 'user:usr_…:create_order'。
-- 首次请求先插入 in_progress 行占位；完成后写入响应。同键不同请求体返回 conflict。
CREATE TABLE idempotency_keys (
    scope         text NOT NULL,
    key           text NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
    request_hash  bytea NOT NULL,
    state         text NOT NULL DEFAULT 'in_progress' CHECK (state IN ('in_progress', 'done')),
    status_code   int,
    response      jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (scope, key),
    CHECK ((state = 'done') = (status_code IS NOT NULL))
);
CREATE INDEX idempotency_keys_expiry ON idempotency_keys (expires_at);
