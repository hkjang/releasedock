-- Make the OIDC redirect URI optional in configuration.
--
-- The authorization request and the token exchange must present a
-- byte-identical redirect_uri. When the value is derived from the incoming
-- request rather than stored configuration, the two legs can be served through
-- different hostnames, so the exact URI used at login is pinned to the state
-- row and replayed at the callback instead of being derived a second time.
ALTER TABLE oidc_states
    ADD COLUMN IF NOT EXISTS redirect_uri TEXT NOT NULL DEFAULT '';
