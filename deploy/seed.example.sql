-- seed.example.sql —— 开发种子数据（docs/06 §1 D-04：示例价格仅开发使用，生产由管理员在后台录入）。
--
-- 由 `isp-control seed-dev` 在一个事务中执行；可重复执行（固定 ID + ON CONFLICT DO NOTHING）。
-- 全部为占位值：节点域名、上游地址、收款信息都不是真实的，不得用于生产。
-- 上游指向本机回环端口（平台 E2E 的假 ISP 上游），无认证。

-- 带示例收款说明的规则版本（内置 R-2026-11-08 的收款说明为空，空时服务端拒绝下单）。
INSERT INTO rules (version, effective_from, period_days, retention_days, port_cooldown_days, resource_cooldown_days,
                   access_changes_per_window, access_change_window_hours, free_exit_changes_per_window,
                   exit_change_window_days, unpaid_hold_hours, max_unpaid_new_orders, month_options,
                   payment_payee, payment_bank, payment_account, payment_note, snapshot_lines)
SELECT 'R-2026-11-08.1', '2026-11-08T00:00:01Z', period_days, retention_days, port_cooldown_days, resource_cooldown_days,
       access_changes_per_window, access_change_window_hours, free_exit_changes_per_window,
       exit_change_window_days, unpaid_hold_hours, max_unpaid_new_orders, month_options,
       '示例收款方（开发占位）', '示例银行（开发占位）', '0000 0000 0000 0000', '转账附言请填写付款识别码', snapshot_lines
  FROM rules WHERE version = 'R-2026-11-08'
ON CONFLICT (version) DO NOTHING;

INSERT INTO nodes (id, name, public_host, port_range_start, port_range_end)
VALUES ('nod_dev00000000000000001', 'dev-node-1', 'proxy.example.test', 20000, 20999)
ON CONFLICT (id) DO NOTHING;

-- 端口池按节点范围预填（库存分配只从 ports 表取空闲端口）。
INSERT INTO ports (node_id, port)
SELECT 'nod_dev00000000000000001', p FROM generate_series(20000, 20999) AS p
ON CONFLICT (node_id, port) DO NOTHING;

-- 出口类型（D-24）：us-lax、jp-tyo 为独享；us-lax-shared 为共享（一个出口最多由资源的绑定上限个订阅共用）。
INSERT INTO plans (id, code, status, sort_order, exit_mode) VALUES
    ('pln_dev00000000000000001', 'us-lax', 'on_sale', 10, 'exclusive'),
    ('pln_dev00000000000000002', 'jp-tyo', 'on_sale', 20, 'exclusive'),
    ('pln_dev00000000000000003', 'us-lax-shared', 'on_sale', 15, 'shared')
ON CONFLICT (id) DO NOTHING;

INSERT INTO plan_versions (id, plan_id, version, name, country_code, country_name, city, price_cents, period_days,
                           included_bytes, concurrency, bandwidth_mbps) VALUES
    ('plv_dev00000000000000001', 'pln_dev00000000000000001', 1, '美国 洛杉矶（示例）', 'us', '美国', '洛杉矶',
     9900, 30, 50000000000, 50, 20),
    ('plv_dev00000000000000002', 'pln_dev00000000000000002', 1, '日本 东京（示例）', 'jp', '日本', '东京',
     12900, 30, 30000000000, 30, 20),
    ('plv_dev00000000000000003', 'pln_dev00000000000000003', 1, '美国 洛杉矶 共享（示例）', 'us', '美国', '洛杉矶',
     3900, 30, 20000000000, 20, 10)
ON CONFLICT (id) DO NOTHING;

INSERT INTO traffic_packs (id, code, status, sort_order) VALUES
    ('tpk_dev00000000000000001', 'gb-10', 'on_sale', 10),
    ('tpk_dev00000000000000002', 'gb-50', 'on_sale', 20)
ON CONFLICT (id) DO NOTHING;

INSERT INTO traffic_pack_versions (id, traffic_pack_id, version, bytes, price_cents) VALUES
    ('tpv_dev00000000000000001', 'tpk_dev00000000000000001', 1, 10000000000, 1500),
    ('tpv_dev00000000000000002', 'tpk_dev00000000000000002', 1, 50000000000, 6000)
ON CONFLICT (id) DO NOTHING;

-- 绑定上限 max_tenants：1 只给独享订阅；≥2 时空闲时可整个分给一个独享订阅，也可由多个共享订阅共用。
INSERT INTO upstream_resources (id, node_id, protocol, host, port, country_code, country_name, city, provider, note, status, max_tenants) VALUES
    ('res_dev00000000000000001', 'nod_dev00000000000000001', 'socks5', '127.0.0.1', 31081, 'us', '美国', '洛杉矶', 'dev-fake', '开发占位上游', 'available', 1),
    ('res_dev00000000000000002', 'nod_dev00000000000000001', 'http',   '127.0.0.1', 31082, 'us', '美国', '洛杉矶', 'dev-fake', '开发占位上游（可共享）', 'available', 3),
    ('res_dev00000000000000003', 'nod_dev00000000000000001', 'socks5', '127.0.0.1', 31083, 'jp', '日本', '东京',   'dev-fake', '开发占位上游', 'available', 1),
    ('res_dev00000000000000004', 'nod_dev00000000000000001', 'http',   '127.0.0.1', 31084, 'jp', '日本', '东京',   'dev-fake', '开发占位上游', 'pending_verify', 1)
ON CONFLICT (id) DO NOTHING;
