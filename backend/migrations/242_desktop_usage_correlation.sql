ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS desktop_turn_id VARCHAR(36),
    ADD COLUMN IF NOT EXISTS desktop_call_id VARCHAR(36),
    ADD COLUMN IF NOT EXISTS desktop_purpose VARCHAR(32),
    ADD COLUMN IF NOT EXISTS settlement_status VARCHAR(16);

-- NULL is deliberate: historical cost estimates are not proof of settlement.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'usage_logs'::regclass AND conname = 'usage_logs_settlement_status_check') THEN
        ALTER TABLE usage_logs ADD CONSTRAINT usage_logs_settlement_status_check
            CHECK (settlement_status IN ('pending', 'settled', 'not_charged', 'unknown'));
    END IF;
END $$;
