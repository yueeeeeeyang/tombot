-- 仅由值班人员在修复节点且核对原容器后执行。重新排空原计划，沿用目标规格和递增的控制版本。
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '15s';

-- 与领取升级及签发会话使用同一把用户电脑锁。每个 gset 都要求精确匹配一行，失败回滚整笔事务。
SELECT h.user_id AS repair_user
FROM tombot.computer_execution_heads h
JOIN tombot.computers c ON (c.user_id,c.id)=(h.user_id,h.computer_id)
WHERE c.id=:'computer_id' AND c.container_id=:'source_container_id' AND c.status='stopping'
FOR UPDATE OF h
\gset

UPDATE tombot.computer_image_upgrades p
SET state='draining', attempt_count=0, next_attempt_at=clock_timestamp(), updated_at=clock_timestamp()
FROM tombot.computers c, tombot.node_connections n
WHERE (c.user_id,c.id,c.node_id,c.image_id,c.generation,c.container_id)=
      (p.user_id,p.computer_id,p.node_id,p.source_image_id,p.source_generation,p.source_container_id)
  AND c.status='stopping' AND p.user_id=:'repair_user' AND p.computer_id=:'computer_id'
  AND p.source_container_id=:'source_container_id' AND p.target_specification->>'fencing_token'=:'target_fence'
  AND n.node_id=p.node_id AND n.image_id=p.target_image_id AND n.supports_computer_upgrades
  AND p.state='quarantined' AND p.attempt_count>0 AND p.lease_expires_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM tombot.computer_sessions s WHERE s.user_id=p.user_id AND s.status<>'released')
RETURNING p.target_image_id AS repair_image
\gset

INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes)
SELECT audit_id,'重新排队已隔离的电脑镜像升级','computer',:'computer_id',audit_id,'succeeded',
       jsonb_build_object('target_image_id',:'repair_image'::text,'target_fence',:'target_fence'::bigint)
FROM (SELECT gen_random_uuid()::text AS audit_id) audit;
COMMIT;
