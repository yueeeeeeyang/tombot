-- 查询与领取恢复窗口共用故障范围。容器停止回执还须由节点核对原计划和 Docker 退出证据。
CREATE FUNCTION tombot.can_retry_computer_upgrade_failure(code text)
RETURNS boolean LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path = pg_catalog AS $$
    SELECT code IS NULL OR code IN ('computer_image_upgrade_source_mismatch','container_stop_unconfirmed');
$$;
REVOKE ALL ON FUNCTION tombot.can_retry_computer_upgrade_failure(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tombot.can_retry_computer_upgrade_failure(text) TO tombot_control;

-- 调用方已持有节点维护锁。只为仍绑定原电脑、原镜像、原控制版本且已排空的计划重新排队。
CREATE OR REPLACE FUNCTION tombot.retry_node_upgrade_computer(node text, image text, computer text, container text, fence bigint, release_digest text)
RETURNS boolean LANGUAGE sql SECURITY INVOKER SET search_path = pg_catalog AS $$
    WITH repaired AS (
        UPDATE tombot.computer_image_upgrades p SET state='draining',attempt_count=0,
               recovery_release=release_digest,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()
        FROM tombot.computers c, tombot.node_connections nc
        WHERE p.node_id=node AND p.target_image_id=image AND p.computer_id=computer AND p.source_container_id=container
          AND (c.user_id,c.id,c.node_id,c.image_id,c.generation,c.container_id)=
              (p.user_id,p.computer_id,p.node_id,p.source_image_id,p.source_generation,p.source_container_id)
          AND nc.node_id=node AND nc.image_id=image AND nc.supports_computer_upgrades AND c.status='stopping'
          AND p.state='quarantined' AND p.attempt_count>0 AND p.lease_expires_at IS NULL
          AND tombot.can_retry_computer_upgrade_failure(p.last_failure_code)
          AND p.target_specification->>'fencing_token'=fence::text AND p.recovery_release IS DISTINCT FROM release_digest
          AND NOT EXISTS (SELECT 1 FROM tombot.computer_sessions s WHERE s.node_id=node AND s.status<>'released')
          AND NOT EXISTS (SELECT 1 FROM tombot.computer_image_upgrades a WHERE a.node_id=node AND a.state IN ('draining','switching'))
        RETURNING p.computer_id
    ) SELECT EXISTS(SELECT 1 FROM repaired);
$$;
