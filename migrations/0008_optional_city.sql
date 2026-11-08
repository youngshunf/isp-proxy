-- 0008：套餐与上游资源的城市可选（D-27，2026-11-08；docs/06 §10）。
--
-- 匹配规则：一次购买的有效城市 = 套餐固定的城市；套餐不限城市时取用户选择的城市；都没有为 NULL。
--   * 有效城市为 NULL：匹配该国家的全部资源（有城市与没有城市的都算）。
--   * 有效城市为 X：只匹配 city = X 的资源（不会分到没有城市的资源）。
-- 挑选、预留、履约与库存计数都按这条规则；履约取 coalesce(subscriptions.city, plan_versions.city)，存量订阅行为不变。
-- 城市有值时不能是空白；应用把 "" 与纯空白归一为 NULL。

-- 1. 上游资源：城市可空（供应商不保证城市时不填）。存量的空白城市归一为 NULL（应用此前不允许空白，正常不会有）。
ALTER TABLE upstream_resources ALTER COLUMN city DROP NOT NULL;
UPDATE upstream_resources SET city = NULL WHERE btrim(city) = '';
ALTER TABLE upstream_resources ADD CONSTRAINT upstream_resources_city_nonblank CHECK (city IS NULL OR btrim(city) <> '');

-- 2. 套餐版本：城市可空，NULL 表示不限城市。版本只追加（不能 UPDATE），约束对存量行不回溯校验。
ALTER TABLE plan_versions ALTER COLUMN city DROP NOT NULL;
ALTER TABLE plan_versions ADD CONSTRAINT plan_versions_city_nonblank CHECK (city IS NULL OR btrim(city) <> '') NOT VALID;

-- 3. 新购订单的有效城市（下单时确定）；续购订单与存量订单为空。
ALTER TABLE orders ADD COLUMN city text CONSTRAINT orders_city_nonblank CHECK (city IS NULL OR btrim(city) <> '');

-- 4. 订阅的有效城市：确认收款创建订阅时从新购订单复制；重新履约（释放后续费）在此城市内挑选。
--    存量订阅为空，履约回落到套餐版本的城市。
ALTER TABLE subscriptions ADD COLUMN city text CONSTRAINT subscriptions_city_nonblank CHECK (city IS NULL OR btrim(city) <> '');
