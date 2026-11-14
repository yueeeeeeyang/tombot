-- 仅替换目录保护函数，无数据回填或表扫描；函数定义锁由迁移事务短暂持有。
-- 停用供应商允许原地编辑，目录身份及历史策略不可变约束保持。
-- 失败事务回滚；降级须恢复升级前数据库和程序，或新增前向脚本恢复限制。
CREATE OR REPLACE FUNCTION tombot.guard_model_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- 只有停用后的目录才允许删除；被任何策略版本引用时整体拒绝，保证历史配置可追溯。
        IF OLD.is_enabled THEN
            RAISE EXCEPTION '模型目录必须停用后才能删除' USING ERRCODE = '23514';
        END IF;
        IF TG_TABLE_NAME = 'model_providers' THEN
            IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE provider_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的供应商不能删除' USING ERRCODE = '23514';
            END IF;
        ELSE
            IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE model_id = OLD.id) THEN
                RAISE EXCEPTION '仍被策略版本引用的模型不能删除' USING ERRCODE = '23514';
            END IF;
        END IF;
        RETURN OLD;
    END IF;
    IF TG_TABLE_NAME = 'model_providers' AND NOT OLD.is_enabled AND NOT NEW.is_enabled THEN
        IF (to_jsonb(NEW) - ARRAY['name','protocol','base_url','revision']) IS DISTINCT FROM
           (to_jsonb(OLD) - ARRAY['name','protocol','base_url','revision']) OR NEW.revision <> OLD.revision + 1 THEN
            RAISE EXCEPTION '停用供应商仅允许编辑名称、协议和地址并递增修订号' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['is_enabled', 'revision']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['is_enabled', 'revision']) OR NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION '模型目录内容不可原地修改' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;

REVOKE ALL ON FUNCTION tombot.guard_model_catalog() FROM PUBLIC;
