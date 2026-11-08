-- 0009：尚未生效的规则版本可以撤回（2026-11-08；docs/06 §11）。
--
-- rules 仍然只追加：任何 UPDATE、以及删除已生效（effective_from <= now()）的版本都拒绝。
-- 唯一的例外是删除尚未生效的版本：它没有被任何订单或订阅引用过（引用时取的是当时生效的版本；外键另行兜底）。
CREATE FUNCTION isp_rules_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.effective_from > now() THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'rules 是只追加表，禁止 %（只能撤回尚未生效的版本）', TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

DROP TRIGGER rules_append_only ON rules;
CREATE TRIGGER rules_append_only BEFORE UPDATE OR DELETE ON rules
    FOR EACH ROW EXECUTE FUNCTION isp_rules_guard();
