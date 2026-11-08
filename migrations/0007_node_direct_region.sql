-- 0007：节点的直出出口地域（D-25 补充，2026-11-08）。
-- 直出代理经中转服务器自身地址出站，前台不显示出口 IP，只显示这台服务器的出口地域（如 新加坡、香港）。
-- 由管理员在节点上填写：国家/地区代码（ISO 3166-1 alpha-2 小写，用于国旗）与显示名称；两者同时有或同时为空。
ALTER TABLE nodes ADD COLUMN direct_country_code text CHECK (direct_country_code ~ '^[a-z]{2}$');
ALTER TABLE nodes ADD COLUMN direct_region text CHECK (char_length(direct_region) BETWEEN 1 AND 32);
ALTER TABLE nodes ADD CONSTRAINT nodes_direct_region_pair CHECK ((direct_country_code IS NULL) = (direct_region IS NULL));
