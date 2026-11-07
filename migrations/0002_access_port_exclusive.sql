-- 节点上同一端口不能同时属于两个订阅的生效/待生效接入（M3 要求：agent 遇到端口冲突会拒绝整份期望状态）。
--
-- 端口池 ports(node_id, port) 主键已保证每个端口只分配给一个接入版本；这里再从接入版本一侧兜底：
-- 插入或变更为 pending/active 的接入版本时，若同节点其他订阅的 pending/active 接入用到其三个端口中的任何一个
-- （含跨协议，例如本版本的 http 端口等于对方的 socks5 端口），整条语句失败。
-- 同一订阅内的旧版本不计：重置密码沿用端口，旧版本在签发新版本时即被撤销；换端口时新旧端口组不相交。
-- 按节点取事务级咨询锁，避免两个事务同时通过检查。

CREATE FUNCTION isp_access_ports_exclusive() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state NOT IN ('pending', 'active') THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('access_ports:' || NEW.node_id));
    IF EXISTS (
        SELECT 1 FROM access_versions o
         WHERE o.node_id = NEW.node_id
           AND o.subscription_id <> NEW.subscription_id
           AND o.id <> NEW.id
           AND o.state IN ('pending', 'active')
           AND ARRAY[o.http_port, o.https_port, o.socks5_port] && ARRAY[NEW.http_port, NEW.https_port, NEW.socks5_port]
    ) THEN
        RAISE EXCEPTION 'access_versions: 端口已被同节点其他订阅的接入占用'
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'access_versions_ports_exclusive';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER access_versions_ports_exclusive
    BEFORE INSERT OR UPDATE OF state, http_port, https_port, socks5_port, node_id ON access_versions
    FOR EACH ROW EXECUTE FUNCTION isp_access_ports_exclusive();
