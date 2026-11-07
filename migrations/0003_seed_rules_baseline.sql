-- 内置规则 R-2026-11-08 改为“自始生效”的兜底版本（docs/06 §8.1）。
--
-- 0001 把它的 effective_from 固定为 2026-11-08T00:00Z。当前规则按 effective_from 取最新，于是早于该时刻发布的
-- 管理员规则会在该时刻被内置规则覆盖：收款说明变回空串，下单一律 412“暂未开放购买”。
-- 管理员发布的规则 effective_from 不早于发布时刻（catalog.CreateRules），把内置规则移到纪元起点后，
-- 任何已发布的规则都排在它之后、一直优先；没有发布过规则时内置规则在任何时刻都生效。
--
-- rules 只追加（rules_append_only 触发器）；这是迁移内对内置行的一次性更正，只改这一行的 effective_from，
-- 事务内临时停用触发器后立即恢复。已有订阅与订单只引用版本号，不受影响。

ALTER TABLE rules DISABLE TRIGGER rules_append_only;
UPDATE rules SET effective_from = '1970-01-01T00:00:00Z' WHERE version = 'R-2026-11-08';
ALTER TABLE rules ENABLE TRIGGER rules_append_only;
