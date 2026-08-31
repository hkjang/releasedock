-- Air-gapped Keycloak deployments are frequently served without TLS, and
-- Keycloak can also emit backchannel URLs (token_endpoint, jwks_uri) on an
-- internal plaintext address while the frontend authorization_endpoint stays on
-- the public HTTPS hostname. Both cases previously failed discovery outright.
--
-- The relaxation is opt-in and the code still refuses plaintext to a publicly
-- routable host, so a client secret can never be sent unencrypted to the
-- internet by a configuration mistake.
ALTER TABLE oidc_settings
    ADD COLUMN IF NOT EXISTS allow_insecure_endpoints BOOLEAN NOT NULL DEFAULT FALSE;
