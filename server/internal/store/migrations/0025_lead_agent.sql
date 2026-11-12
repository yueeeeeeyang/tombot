-- 主猫固定在侧栏顶部。原先标识为 assistant 的默认猫继续担任；
-- 没有这只猫时，用最早创建的那只，避免已有工作空间没有置顶。
ALTER TABLE workspace_meta ADD COLUMN lead_agent_id TEXT NOT NULL DEFAULT '';

UPDATE workspace_meta
SET lead_agent_id = COALESCE(
  (SELECT desktop_id FROM bots WHERE is_archived = 0 AND desktop_id = 'assistant' LIMIT 1),
  (SELECT desktop_id FROM bots WHERE is_archived = 0 AND desktop_id IS NOT NULL ORDER BY created_at, id LIMIT 1),
  ''
)
WHERE id = 1 AND lead_agent_id = '';
