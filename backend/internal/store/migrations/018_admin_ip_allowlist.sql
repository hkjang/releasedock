-- Source-IP allowlist for administration endpoints.
--
-- Behind a reverse proxy every request appears to come from the proxy, so an
-- allowlist is only meaningful together with an explicit list of trusted
-- proxies; the API consults X-Forwarded-For only for peers in that list.

CREATE TABLE IF NOT EXISTS network_settings (
    id TEXT PRIMARY KEY DEFAULT 'default',
    admin_ip_allowlist_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    -- CIDR strings; a bare address is accepted and stored as a /32 or /128.
    admin_ip_allowlist TEXT[] NOT NULL DEFAULT '{}',
    trusted_proxy_cidrs TEXT[] NOT NULL DEFAULT '{}',
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Enabling an empty allowlist would lock every administrator out.
    CHECK (NOT admin_ip_allowlist_enabled OR cardinality(admin_ip_allowlist) > 0)
);
INSERT INTO network_settings(id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
