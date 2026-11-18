-- 为旧桌面默认猫咪补齐电脑工具，保留自定义提示词及其他工具集合。
-- 迁移身份受强制行级策略约束，临时权限仅对结构所有者生效并在同一事务内回收。
CREATE POLICY default_computer_tools_migration ON tombot.bots TO CURRENT_USER USING (true) WITH CHECK (true);
UPDATE tombot.bots
   SET tool_permissions='["computer_browser","computer_input","computer_screenshot","computer_shell"]'::jsonb,
       revision=revision+1, updated_at=clock_timestamp()
 WHERE tool_permissions='["computer_shell"]'::jsonb
   AND system_prompt='You are a helpful assistant. Collaborate with the user and use the available tools when needed.';
DROP POLICY default_computer_tools_migration ON tombot.bots;
