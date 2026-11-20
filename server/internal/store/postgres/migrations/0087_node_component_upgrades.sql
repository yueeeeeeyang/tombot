-- 新字段均可空，无需回填。原有维护窗口、验收围栏与待领取的电脑计划继续沿用。
ALTER TABLE tombot.node_upgrades
    ADD COLUMN host_release text CHECK (host_release ~ '^[0-9a-f]{64}$'),
    ADD COLUMN stage text CHECK (stage IN ('maintenance','stop','activate','start','finish','rollback','rolled_back'));
GRANT UPDATE (host_release,stage) ON tombot.node_upgrades TO tombot_control;
ALTER TABLE tombot.node_update_jobs ADD COLUMN stage text;
GRANT UPDATE (stage) ON tombot.node_update_jobs TO tombot_control;

-- 一次验收资源可经历多次发布和回退，按原围栏去重，迟到回执不能反复补充重试次数。
CREATE TABLE tombot.node_verification_repairs (
    node_id text NOT NULL REFERENCES tombot.computer_nodes(id),
    fencing_token bigint NOT NULL,
    release text NOT NULL CHECK (release ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (node_id,fencing_token,release)
);
GRANT SELECT, INSERT ON tombot.node_verification_repairs TO tombot_control;

-- 运维视图返回当前真正阻止电脑领取的原因，历史失败码留在原计划。
CREATE FUNCTION tombot.node_upgrade_diagnosis(node text) RETURNS text
LANGUAGE sql STABLE SECURITY INVOKER SET search_path=pg_catalog AS $$
    SELECT CASE
        WHEN NOT m.is_finished THEN coalesce(m.stage,'maintenance')
        WHEN v.state='quarantined' THEN 'verification_quarantined/'||coalesce(v.node_error_code,v.error_code,'unconfirmed')
        WHEN n.control_expires_at IS NULL OR n.control_expires_at<=statement_timestamp() THEN 'heartbeat'
        WHEN n.status='draining' THEN 'node_draining'
        WHEN n.status<>'ready' OR n.capability_expires_at IS NULL OR n.capability_expires_at<=statement_timestamp()
             OR n.verified_image_id IS DISTINCT FROM nc.image_id
            THEN 'verification/'||coalesce(CASE WHEN v.state='failed' THEN coalesce(v.node_error_code,v.error_code) END,v.state,'pending')
        WHEN p.state='quarantined' THEN 'computer_quarantined/'||coalesce(p.last_failure_code,'unconfirmed')
        WHEN p.state IS NOT NULL THEN p.state
        WHEN EXISTS(SELECT 1 FROM tombot.computers c WHERE c.node_id=n.id AND c.status='ready' AND c.image_id<>nc.image_id) THEN 'awaiting_worker'
        ELSE 'ready' END
    FROM tombot.computer_nodes n JOIN tombot.node_connections nc ON nc.node_id=n.id
    LEFT JOIN tombot.node_upgrades m ON m.node_id=n.id
    LEFT JOIN tombot.node_verifications v ON v.node_id=n.id
    LEFT JOIN LATERAL (SELECT state,last_failure_code FROM tombot.computer_image_upgrades
        WHERE node_id=n.id AND state IN ('draining','switching','quarantined')
        ORDER BY (state='quarantined'),computer_id LIMIT 1) p ON true
    WHERE n.id=node
$$;
REVOKE ALL ON FUNCTION tombot.node_upgrade_diagnosis(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.node_upgrade_diagnosis(text) TO tombot_control;
