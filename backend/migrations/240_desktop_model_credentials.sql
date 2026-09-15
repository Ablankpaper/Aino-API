-- Migration 240: Add desktop model credentials table
-- Time-limited, revocable API keys issued to desktop clients for managed model access

ALTER TABLE api_keys ADD COLUMN desktop_managed BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS desktop_model_credentials (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    device_id VARCHAR(128) NOT NULL,
    connection_grant_id VARCHAR(128) NOT NULL,
    session_family_id VARCHAR(128) NOT NULL,
    token_version BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    model_id VARCHAR(128) NOT NULL,
    api_key_id BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    revoke_reason VARCHAR(100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Foreign keys
    CONSTRAINT fk_desktop_credential_user
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT fk_desktop_credential_api_key
        FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE,
    CONSTRAINT fk_desktop_credential_group
        FOREIGN KEY (group_id) REFERENCES groups(id),
    CONSTRAINT desktop_credential_expiry CHECK (expires_at > created_at),
    CONSTRAINT desktop_credential_key_unique UNIQUE (api_key_id)
);

-- Active credential lookup by user and device
-- Expired leases are retired in the same transaction before replacement.
-- Time-dependent predicates cannot be used in PostgreSQL indexes.
CREATE UNIQUE INDEX idx_desktop_credentials_active_lookup
    ON desktop_model_credentials (user_id, device_id, connection_grant_id, session_family_id, group_id)
    WHERE revoked_at IS NULL;

-- Efficient revocation by session family
CREATE INDEX idx_desktop_credentials_session_family
    ON desktop_model_credentials (session_family_id)
    WHERE revoked_at IS NULL;

-- Efficient revocation by device
CREATE INDEX idx_desktop_credentials_device
    ON desktop_model_credentials (user_id, device_id)
    WHERE revoked_at IS NULL;

-- Efficient revocation by API key
CREATE INDEX idx_desktop_credentials_api_key
    ON desktop_model_credentials (api_key_id);

-- Efficient lookup by user
CREATE INDEX idx_desktop_credentials_user
    ON desktop_model_credentials (user_id, created_at DESC);
