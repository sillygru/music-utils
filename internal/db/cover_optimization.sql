-- Additive index for cover refresh sweep.
CREATE INDEX IF NOT EXISTS idx_cover_urls_checked_at ON cover_urls(checked_at) WHERE cover_url IS NOT NULL;
