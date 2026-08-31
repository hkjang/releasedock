package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) localLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Username = store.NormalizeUsername(input.Username)
	clientIP := remoteIP(r)
	limiterKey := "login-user:" + clientIP + ":" + input.Username
	if !s.limiter.allowed(limiterKey) {
		writeError(w, http.StatusTooManyRequests, "login_rate_limited", "too many failed attempts; try again later")
		return
	}
	var userID, hash string
	err := s.store.Pool.QueryRow(r.Context(), `SELECT id,password_hash FROM users WHERE lower(username)=lower($1) AND active=TRUE AND auth_source='local'`, input.Username).Scan(&userID, &hash)
	if err != nil || hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Password)) != nil {
		s.limiter.failure(limiterKey)
		s.store.Audit(r.Context(), "", "auth.login", "user", "", "failure", remoteIP(r), r.UserAgent(), []byte(`{"method":"local"}`))
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	s.limiter.success(limiterKey)
	s.createSession(w, r, userID, "local")
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request, userID, method string) {
	token, err := secure.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "entropy_error", "could not create session")
		return
	}
	csrf, err := secure.RandomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "entropy_error", "could not create session")
		return
	}
	expires := time.Now().Add(12 * time.Hour)
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,ip,user_agent) VALUES($1,$2,$3,$4,$5,$6)`, secure.TokenHash(token), userID, secure.TokenHash(csrf), expires, remoteIP(r), r.UserAgent())
	if err != nil {
		s.log.Error("create session", "error", err)
		writeError(w, http.StatusInternalServerError, "session_error", "could not create session")
		return
	}
	p, err := s.storePrincipal(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", "could not load user")
		return
	}
	secureCookies := s.useSecureCookies(r.Context(), r)
	http.SetCookie(w, &http.Cookie{
		Name: "releasedock_session", Value: token, Path: "/", HttpOnly: true,
		Secure: secureCookies, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name: "releasedock_csrf", Value: csrf, Path: "/", HttpOnly: false,
		Secure: secureCookies, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
	})
	s.store.Audit(r.Context(), userID, "auth.login", "user", userID, "success", remoteIP(r), r.UserAgent(), []byte(`{"method":"`+method+`"}`))
	if method == "oidc" {
		returnTo := r.Header.Get("X-ReleaseDock-OIDC-Return-To")
		if !safeReturnTo(returnTo) {
			returnTo = "/"
		}
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": p, "csrfToken": csrf, "expiresAt": expires})
}

func (s *Server) storePrincipal(ctx context.Context, userID string) (store.Principal, error) {
	// A short-lived synthetic session is deliberately not used; load through a
	// read query so no credential material is created as a side effect.
	var p store.Principal
	err := s.store.Pool.QueryRow(ctx, `SELECT id,username,display_name,email,auth_source FROM users WHERE id=$1 AND active=TRUE`, userID).
		Scan(&p.UserID, &p.Username, &p.DisplayName, &p.Email, &p.AuthSource)
	if err != nil {
		return store.Principal{}, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT rp.permission_code FROM user_roles ur JOIN role_permissions rp ON rp.role_id=ur.role_id WHERE ur.user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return store.Principal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return store.Principal{}, err
		}
		p.Permissions = append(p.Permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return store.Principal{}, err
	}
	roleRows, err := s.store.Pool.Query(ctx, `SELECT CASE r.id WHEN 'role-admin' THEN 'admin' WHEN 'role-operator' THEN 'operator' WHEN 'role-viewer' THEN 'viewer' ELSE r.name END FROM roles r JOIN user_roles ur ON ur.role_id=r.id WHERE ur.user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return store.Principal{}, err
	}
	defer roleRows.Close()
	for roleRows.Next() {
		var role string
		if err := roleRows.Scan(&role); err != nil {
			return store.Principal{}, err
		}
		p.Roles = append(p.Roles, role)
	}
	return p, roleRows.Err()
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	if cookie, err := r.Cookie("releasedock_session"); err == nil {
		_, _ = s.store.Pool.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, secure.TokenHash(cookie.Value))
	}
	secureCookies := s.useSecureCookies(r.Context(), r)
	http.SetCookie(w, &http.Cookie{Name: "releasedock_session", Value: "", Path: "/", HttpOnly: true, Secure: secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "releasedock_csrf", Value: "", Path: "/", HttpOnly: false, Secure: secureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	s.store.Audit(r.Context(), p.UserID, "auth.logout", "user", p.UserID, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var preferences json.RawMessage
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT preferences FROM users WHERE id=$1`, p.UserID).Scan(&preferences); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load profile")
		return
	}
	_ = preferences
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) updateMyProfile(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var input struct {
		DisplayName *string         `json:"displayName"`
		Email       *string         `json:"email"`
		Preferences json.RawMessage `json:"preferences"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if (input.DisplayName != nil && len(*input.DisplayName) > 200) || (input.Email != nil && len(*input.Email) > 320) || len(input.Preferences) > 64<<10 {
		writeError(w, http.StatusBadRequest, "invalid_profile", "profile fields are too long")
		return
	}
	var preferences any
	if len(input.Preferences) > 0 {
		var object map[string]any
		if json.Unmarshal(input.Preferences, &object) != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", "preferences must be a JSON object")
			return
		}
		preferences = input.Preferences
	}
	_, err := s.store.Pool.Exec(r.Context(), `UPDATE users SET display_name=COALESCE($2,display_name),email=COALESCE($3,email),preferences=COALESCE($4,preferences),updated_at=now() WHERE id=$1`, p.UserID, input.DisplayName, input.Email, preferences)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update profile")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "profile.update", "user", p.UserID, "success", remoteIP(r), r.UserAgent(), nil)
	updated, err := s.storePrincipal(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load profile")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if len(input.NewPassword) < 12 || len(input.NewPassword) > 1024 {
		writeError(w, http.StatusBadRequest, "invalid_password", "newPassword must contain between 12 and 1024 characters")
		return
	}
	if input.CurrentPassword == input.NewPassword {
		writeError(w, http.StatusBadRequest, "invalid_password", "newPassword must differ from the current password")
		return
	}
	var hash, source string
	if err := s.store.Pool.QueryRow(r.Context(), `SELECT COALESCE(password_hash,''),auth_source FROM users WHERE id=$1 AND active=TRUE`, p.UserID).Scan(&hash, &source); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if source != "local" || hash == "" {
		writeError(w, http.StatusBadRequest, "password_not_available", "OIDC accounts must change their password in the identity provider")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid_current_password", "current password is incorrect")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(input.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password_error", "could not change password")
		return
	}
	tx, err := s.store.Pool.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not change password")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if _, err = tx.Exec(r.Context(), `UPDATE users SET password_hash=$2,updated_at=now() WHERE id=$1`, p.UserID, string(newHash)); err == nil {
		// Keep the current browser session and revoke sessions on other devices.
		if cookie, cookieErr := r.Cookie("releasedock_session"); cookieErr == nil {
			_, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2`, p.UserID, secure.TokenHash(cookie.Value))
		} else {
			_, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, p.UserID)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not change password")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "password.change", "user", p.UserID, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updatePreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var input map[string]any
	if !decodeJSON(w, r, &input) {
		return
	}
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) > 64<<10 {
		writeError(w, http.StatusBadRequest, "invalid_preferences", "preferences must be a JSON object no larger than 64 KiB")
		return
	}
	// PATCH merges: a caller that sets one key must not silently drop the rest.
	var merged json.RawMessage
	err = s.store.Pool.QueryRow(r.Context(), `UPDATE users SET preferences=preferences||$2,updated_at=now() WHERE id=$1 RETURNING preferences`, p.UserID, encoded).Scan(&merged)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update preferences")
		return
	}
	_ = json.Unmarshal(merged, &input)
	s.store.Audit(r.Context(), p.UserID, "profile.preferences.update", "user", p.UserID, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"preferences": input})
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	rows, err := s.store.Pool.Query(r.Context(), `SELECT id,name,prefix,scopes,expires_at,last_used_at,revoked_at,created_at,updated_at FROM api_keys WHERE user_id=$1 ORDER BY created_at DESC`, p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list API keys")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, prefix string
		var scopes []string
		var expires, lastUsed, revoked *time.Time
		var created, updated time.Time
		if err := rows.Scan(&id, &name, &prefix, &scopes, &expires, &lastUsed, &revoked, &created, &updated); err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list API keys")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "prefix": prefix, "permissions": scopes, "expiresAt": expires, "lastUsedAt": lastUsed, "revokedAt": revoked, "createdAt": created, "updatedAt": updated, "active": revoked == nil && (expires == nil || expires.After(time.Now()))})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "page": 1, "pageSize": len(items)})
}

type apiKeyInput struct {
	Name           string   `json:"name"`
	Scopes         []string `json:"permissions"`
	ExpiresAt      *string  `json:"expiresAt"`
	ClearExpiresAt bool     `json:"clearExpiresAt"`
}

func parseAPIKeyExpiry(value *string) (*time.Time, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil
	}
	raw := strings.TrimSpace(*value)
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return &parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, errors.New("expiresAt must be RFC3339 or YYYY-MM-DD")
	}
	parsed = parsed.Add(24*time.Hour - time.Nanosecond)
	return &parsed, nil
}

func validateKeyInput(p store.Principal, input apiKeyInput) error {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		return errors.New("name is required and must not exceed 100 characters")
	}
	if len(input.Scopes) == 0 {
		return errors.New("at least one scope is required")
	}
	expiresAt, err := parseAPIKeyExpiry(input.ExpiresAt)
	if err != nil {
		return err
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return errors.New("expires_at must be in the future")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if seen[scope] {
			return errors.New("scopes must not contain duplicates")
		}
		seen[scope] = true
		allowed := false
		for _, permission := range p.Permissions {
			if permission == scope {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("scope is not granted to the current user: " + scope)
		}
	}
	return nil
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var input apiKeyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateKeyInput(p, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_api_key", err.Error())
		return
	}
	id, _ := secure.NewID()
	expiresAt, _ := parseAPIKeyExpiry(input.ExpiresAt)
	random, err := secure.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "entropy_error", "could not generate API key")
		return
	}
	token := "rdk_" + random
	prefix := token[:12]
	_, err = s.store.Pool.Exec(r.Context(), `INSERT INTO api_keys(id,user_id,name,prefix,secret_hash,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, p.UserID, strings.TrimSpace(input.Name), prefix, secure.TokenHash(token), input.Scopes, expiresAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not create API key")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "api_key.create", "api_key", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": strings.TrimSpace(input.Name), "prefix": prefix, "permissions": input.Scopes, "expiresAt": expiresAt, "secret": token, "tokenVisibleOnce": true, "active": true, "createdAt": time.Now()})
}

func (s *Server) rotateAPIKey(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	id := r.PathValue("id")
	random, err := secure.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "entropy_error", "could not rotate API key")
		return
	}
	token := "rdk_" + random
	prefix := token[:12]
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE api_keys SET prefix=$3,secret_hash=$4,last_used_at=NULL,updated_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, p.UserID, prefix, secure.TokenHash(token))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not rotate API key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "api_key.rotate", "api_key", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "prefix": prefix, "secret": token, "tokenVisibleOnce": true, "rotatedAt": time.Now(), "active": true})
}

func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	var input apiKeyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateKeyInput(p, input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_api_key", err.Error())
		return
	}
	id := r.PathValue("id")
	expiresAt, _ := parseAPIKeyExpiry(input.ExpiresAt)
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE api_keys SET name=$3,scopes=$4,expires_at=CASE WHEN $6 THEN NULL WHEN $5::timestamptz IS NOT NULL THEN $5 ELSE expires_at END,updated_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, p.UserID, strings.TrimSpace(input.Name), input.Scopes, expiresAt, input.ClearExpiresAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not update API key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "api_key.update", "api_key", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": strings.TrimSpace(input.Name), "permissions": input.Scopes, "expiresAt": expiresAt, "active": true})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r)
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE api_keys SET revoked_at=now(),updated_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not revoke API key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "api_key.revoke", "api_key", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}

var _ = pgx.ErrNoRows
