-- Cover every identity/status update path, including administrator changes.
-- Ordinary API keys keep their established password/logout semantics.
CREATE FUNCTION revoke_desktop_credentials_on_identity_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.password_hash IS DISTINCT FROM NEW.password_hash
       OR OLD.email IS DISTINCT FROM NEW.email
       OR (NEW.status <> 'active' AND OLD.status IS DISTINCT FROM NEW.status)
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at THEN
        UPDATE desktop_model_credentials
        SET revoked_at = NOW(), revoke_reason = 'identity_changed', updated_at = NOW()
        WHERE user_id = NEW.id AND revoked_at IS NULL;
        UPDATE api_keys SET status = 'disabled', updated_at = NOW()
        WHERE user_id = NEW.id AND desktop_managed AND status <> 'disabled';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_users_desktop_credential_revocation
AFTER UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION revoke_desktop_credentials_on_identity_change();
