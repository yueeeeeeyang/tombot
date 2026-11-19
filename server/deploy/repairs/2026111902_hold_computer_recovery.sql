-- 同机管理员只为原镜像取得程序修复窗口；在锁内复核隔离现场，新镜像升级仍走服务端维护协议。
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '15s';

SELECT n.id AS held_node
FROM tombot.node_connections nc JOIN tombot.computer_nodes n ON n.id=nc.node_id
WHERE nc.node_id=:'node_id' AND nc.image_id=:'image_id' AND nc.supports_computer_upgrades
  AND n.reserved=1
FOR UPDATE OF nc,n
\gset

SELECT p.user_id AS held_user
FROM tombot.computer_image_upgrades p
JOIN tombot.computers c ON (c.user_id,c.id,c.node_id,c.image_id,c.container_id,c.generation)=
    (p.user_id,p.computer_id,p.node_id,p.source_image_id,p.source_container_id,p.source_generation)
JOIN tombot.computer_execution_heads h ON (h.user_id,h.computer_id)=(p.user_id,p.computer_id)
WHERE p.node_id=:'node_id' AND p.user_id=:'user_id' AND p.computer_id=:'computer_id' AND p.source_container_id=:'source_container_id'
  AND p.target_image_id=:'image_id' AND p.target_specification->>'fencing_token'=:'target_fence'
  AND p.state='quarantined' AND c.status='stopping' AND p.attempt_count>0 AND p.lease_expires_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM tombot.computer_sessions s WHERE s.node_id=p.node_id AND s.status<>'released')
FOR UPDATE OF h,p
\gset

-- 验收与维护串行，已经领取的验收保留其租约。同一次程序修复可在进程中断后继续。
SELECT EXISTS(SELECT 1 FROM tombot.node_verifications WHERE node_id=:'held_node'
              AND lease_expires_at>clock_timestamp() FOR UPDATE) AS verification_busy
\gset
\if :verification_busy
ROLLBACK;
SELECT 'verification_busy';
\quit
\endif
INSERT INTO tombot.node_upgrades AS m(node_id,operation_id,target_image_id,is_finished)
VALUES (:'node_id',:'operation_id',:'image_id',false)
ON CONFLICT (node_id) DO UPDATE SET operation_id=EXCLUDED.operation_id,target_image_id=EXCLUDED.target_image_id,is_finished=false
WHERE (m.is_finished AND m.operation_id<EXCLUDED.operation_id)
   OR (NOT m.is_finished AND m.operation_id=EXCLUDED.operation_id AND m.target_image_id=EXCLUDED.target_image_id)
RETURNING operation_id AS maintenance_operation
\gset
UPDATE tombot.computer_nodes SET status=CASE WHEN status='draining' THEN status ELSE 'bootstrapping' END,
       verified_image_id=NULL,capability_expires_at=NULL WHERE id=:'held_node';
UPDATE tombot.node_verifications SET is_valid=false WHERE node_id=:'held_node';
INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes)
VALUES(:'maintenance_operation','为隔离电脑保留原镜像维修窗口','computer',:'computer_id',:'maintenance_operation',
       'succeeded',jsonb_build_object('node_id',:'held_node'::text))
ON CONFLICT (id) DO NOTHING;
COMMIT;
