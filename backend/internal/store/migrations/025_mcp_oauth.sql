-- MCP over SSO: /mcp accepts a Keycloak access token in addition to a personal
-- key. The MCP authorization specification (2025-06-18 and later) is OAuth 2.1
-- with this server as the *resource server*: it publishes where the
-- authorization server is (RFC 9728) and checks the tokens that server issued
-- for it. Keycloak keeps doing the login; nothing here issues tokens.
--
-- Off by default. The issuer and web client are the existing OIDC settings, so
-- only what is specific to the resource server is added: the identifier the
-- server claims, the audiences an administrator accepts without an Audience
-- mapper, and the permission ceiling an SSO principal gets.
ALTER TABLE oidc_settings
    ADD COLUMN IF NOT EXISTS mcp_oauth_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS mcp_oauth_resource TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS mcp_oauth_audience TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS mcp_oauth_scopes TEXT[] NOT NULL DEFAULT ARRAY['mcp.use','applications.read','profiles.read','releases.read'];
