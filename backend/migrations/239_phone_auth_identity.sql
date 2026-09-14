-- Migration 239: Add phone auth identity support
-- Adds 'phone' as a valid provider_type for auth_identities
-- Ensures one phone identity per user with partial unique index

-- 1. Update CHECK constraint to include 'phone'
-- Drop old constraint first
ALTER TABLE auth_identities DROP CONSTRAINT IF EXISTS auth_identities_provider_type_check;

-- Add new constraint with phone included
ALTER TABLE auth_identities ADD CONSTRAINT auth_identities_provider_type_check
  CHECK (provider_type IN ('email', 'github', 'google', 'linuxdo', 'oidc', 'wechat', 'dingtalk', 'phone'));

-- 2. Create partial unique index: one phone identity per user
-- This ensures each user can have at most one phone identity
CREATE UNIQUE INDEX IF NOT EXISTS auth_identities_phone_per_user
  ON auth_identities (user_id)
  WHERE provider_type = 'phone';

-- 3. Ensure signup_source CHECK constraint also includes phone
-- Check if constraint exists and update it
DO $$
BEGIN
  -- Drop existing signup_source constraint on users table if it exists
  IF EXISTS (
    SELECT 1 FROM information_schema.table_constraints 
    WHERE constraint_name = 'users_signup_source_check' 
    AND table_name = 'users'
  ) THEN
    ALTER TABLE users DROP CONSTRAINT users_signup_source_check;
  END IF;
  
  -- Add updated constraint
  ALTER TABLE users ADD CONSTRAINT users_signup_source_check
    CHECK (signup_source IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'phone'));
END $$;

-- 4. Update grant provider_type constraint if exists
-- user_provider_default_grants table tracks first-time provider grants
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables 
    WHERE table_name = 'user_provider_default_grants'
  ) THEN
    -- Drop only the known CHECK constraint. PostgreSQL 18 also reports the
    -- generated NOT NULL constraint in information_schema; a broad name match
    -- can remove that constraint instead and leave this CHECK in place.
    ALTER TABLE user_provider_default_grants
      DROP CONSTRAINT IF EXISTS user_provider_default_grants_provider_type_check;
    
    -- Add new constraint
    ALTER TABLE user_provider_default_grants ADD CONSTRAINT user_provider_default_grants_provider_type_check
      CHECK (provider_type IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'phone'));
  END IF;
END $$;
