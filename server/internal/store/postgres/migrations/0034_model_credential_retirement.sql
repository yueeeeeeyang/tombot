-- 撤销与销毁分开：保留引用身份和审计线索，销毁后不再保留可还原材料。
ALTER TABLE tombot.model_credentials
    ADD COLUMN destroyed_at timestamptz,
    ALTER COLUMN nonce DROP NOT NULL,
    ALTER COLUMN ciphertext DROP NOT NULL,
    ADD CONSTRAINT model_credentials_retirement CHECK (
        (destroyed_at IS NULL AND nonce IS NOT NULL AND ciphertext IS NOT NULL)
        OR (destroyed_at IS NOT NULL AND revoked_at IS NOT NULL AND nonce IS NULL AND ciphertext IS NULL)
    );
CREATE INDEX model_policy_versions_credential_active ON tombot.model_policy_versions(credential_id)
    WHERE status <> 'revoked';

CREATE OR REPLACE FUNCTION tombot.guard_model_credential() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, tombot, pg_temp AS $guard$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '凭据标识必须保留用于追溯' USING ERRCODE = '23514';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['revoked_at','destroyed_at','nonce','ciphertext']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['revoked_at','destroyed_at','nonce','ciphertext']) THEN
        RAISE EXCEPTION '凭据版本身份不可修改' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL AND NEW.destroyed_at IS NULL
       AND NEW.nonce IS NOT DISTINCT FROM OLD.nonce AND NEW.ciphertext IS NOT DISTINCT FROM OLD.ciphertext THEN
        RETURN NEW;
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NOT DISTINCT FROM OLD.revoked_at
       AND OLD.destroyed_at IS NULL AND NEW.destroyed_at IS NOT NULL AND NEW.nonce IS NULL AND NEW.ciphertext IS NULL THEN
        IF EXISTS (SELECT 1 FROM tombot.model_policy_versions WHERE credential_id=OLD.id AND status<>'revoked') THEN
            RAISE EXCEPTION '仍被策略引用的凭据不能销毁' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION '凭据只允许首次撤销及符合条件的销毁' USING ERRCODE = '23514';
END
$guard$;
REVOKE ALL ON FUNCTION tombot.guard_model_credential() FROM PUBLIC;

-- 当前配置表规模较小，结构锁和约束扫描受统一迁移时限限制；失败整体回滚。
-- 密文销毁是独立管理动作，不在迁移时执行；已销毁材料不能通过应用回滚恢复。
