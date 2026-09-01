-- Silent SSO: when the Keycloak session is still alive, a visitor is signed in
-- without seeing a login screen.
--
-- The attempt is made with prompt=none, so the identity provider either
-- redirects straight back with a code or reports login_required without
-- showing any UI. Whether a given login attempt was silent has to be
-- remembered across the redirect, because a silent failure must land on the
-- login page quietly rather than surfacing an error.
ALTER TABLE oidc_settings
    ADD COLUMN IF NOT EXISTS auto_login BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE oidc_states
    ADD COLUMN IF NOT EXISTS silent BOOLEAN NOT NULL DEFAULT FALSE;
