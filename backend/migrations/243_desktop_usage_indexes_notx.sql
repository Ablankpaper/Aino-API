CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_user_desktop_turn
    ON usage_logs (user_id, desktop_turn_id, created_at DESC) WHERE desktop_turn_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_user_desktop_call
    ON usage_logs (user_id, desktop_call_id) WHERE desktop_call_id IS NOT NULL;
