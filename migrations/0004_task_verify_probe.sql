-- 换出口 / 组合更换的切换后检测（R-11.6、A-42）。
--
-- 节点应用新出口后，控制面经新绑定下发一次出口检测（kind = exit_check，exit_binding_id = 任务的新出口），
-- 任务记下这条探测并保持 running；检测通过才报告成功，失败则整体回到原接入与原出口、候选资源隔离并告警。
-- 此前任务只校验提交前的比较探测，候选在比较之后故障时任务仍报告成功、订阅全部请求 503。

ALTER TABLE tasks ADD COLUMN verify_probe_id text REFERENCES probes(id);
CREATE UNIQUE INDEX tasks_verify_probe ON tasks (verify_probe_id) WHERE verify_probe_id IS NOT NULL;
