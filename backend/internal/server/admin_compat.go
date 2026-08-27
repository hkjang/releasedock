package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type roleCompatInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Permissions json.RawMessage `json:"permissions"`
}

func parseStringList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		return values, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		values = nil
		for _, value := range strings.Split(text, ",") {
			if value = strings.TrimSpace(value); value != "" {
				values = append(values, value)
			}
		}
		return values, nil
	}
	return nil, errors.New("value must be a comma-separated string or string array")
}
func (s *Server) assignRolePermissions(ctx context.Context, tx pgx.Tx, actorID, roleID string, permissions []string) error {
	if err := validateRolePermissionMutation(ctx, tx, actorID, roleID, permissions); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id=$1`, roleID); err != nil {
		return err
	}
	for _, permission := range permissions {
		tag, insertErr := tx.Exec(ctx, `INSERT INTO role_permissions(role_id,permission_code) SELECT $1,code FROM permissions WHERE code=$2`, roleID, permission)
		if insertErr != nil || tag.RowsAffected() == 0 {
			return errors.New("unknown permission: " + permission)
		}
	}
	return nil
}
func (s *Server) createRoleCompat(w http.ResponseWriter, r *http.Request) {
	var input roleCompatInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, 400, "invalid_role", "name is required")
		return
	}
	permissions, err := parseStringList(input.Permissions)
	if err != nil {
		writeError(w, 400, "invalid_role", err.Error())
		return
	}
	id, _ := secure.NewID()
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not create role")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO roles(id,name,description) VALUES($1,$2,$3)`, id, input.Name, input.Description)
	if err != nil {
		writeError(w, 409, "role_conflict", "role name already exists")
		return
	}
	if err = s.assignRolePermissions(r.Context(), tx, p.UserID, id, permissions); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "role_conflict", "role could not be committed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "role.create", "role", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name, "description": input.Description, "permissions": permissions, "system": false})
}
func (s *Server) putRole(w http.ResponseWriter, r *http.Request) {
	var input roleCompatInput
	if !decodeJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	permissions, err := parseStringList(input.Permissions)
	if err != nil {
		writeError(w, 400, "invalid_role", err.Error())
		return
	}
	if id == "role-admin" {
		writeError(w, 400, "protected_role", "Administrator role is protected")
		return
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update role")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE roles SET name=CASE WHEN system THEN name ELSE $2 END,description=$3,updated_at=now() WHERE id=$1`, id, strings.TrimSpace(input.Name), input.Description)
	if err != nil {
		writeError(w, 409, "role_conflict", "role name already exists")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "not_found", "role not found")
		return
	}
	if err = s.assignRolePermissions(r.Context(), tx, p.UserID, id, permissions); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "role_conflict", "role update could not be committed")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "role.update", "role", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "name": input.Name, "description": input.Description, "permissions": permissions})
}

type userCompatInput struct {
	Username    string          `json:"username"`
	DisplayName string          `json:"displayName"`
	Email       string          `json:"email"`
	Password    string          `json:"password"`
	Roles       json.RawMessage `json:"roles"`
	Active      *bool           `json:"active"`
}

func (s *Server) resolveRoleIDs(ctx context.Context, queryer dependencyQueryer, raw json.RawMessage) ([]string, error) {
	roles, err := parseStringList(raw)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, role := range roles {
		var id string
		err = queryer.QueryRow(ctx, `SELECT id FROM roles WHERE id=$1 OR lower(name)=lower($2) OR id=$3`, role, role, "role-"+strings.ToLower(role)).Scan(&id)
		if err != nil {
			return nil, errors.New("unknown role: " + role)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var input userCompatInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Username = store.NormalizeUsername(input.Username)
	if input.Username == "" || len(input.Password) < 12 {
		writeError(w, 400, "invalid_user", "username and password of at least 12 characters are required")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, 500, "password_error", "could not hash password")
		return
	}
	id, _ := secure.NewID()
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	p, _ := principalFrom(r)
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not create user")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	roleIDs, err := s.resolveRoleIDs(r.Context(), tx, input.Roles)
	if err != nil {
		writeError(w, 400, "invalid_role", err.Error())
		return
	}
	if err = validateDelegatedRoles(r.Context(), tx, p.UserID, roleIDs); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO users(id,username,password_hash,display_name,email,active,auth_source) VALUES($1,$2,$3,$4,$5,$6,'local')`, id, input.Username, string(hash), input.DisplayName, input.Email, active)
	for _, roleID := range roleIDs {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, id, roleID)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 409, "user_conflict", "username already exists or role is invalid")
		return
	}
	s.store.Audit(r.Context(), p.UserID, "user.create", "user", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 201, map[string]any{"id": id, "username": input.Username, "displayName": input.DisplayName, "email": input.Email, "roles": roleIDs, "source": "local", "active": active})
}
func (s *Server) putUser(w http.ResponseWriter, r *http.Request) {
	var input userCompatInput
	if !decodeJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	actor, _ := principalFrom(r)
	active := true
	if input.Active != nil {
		active = *input.Active
	}
	if id == actor.UserID && !active {
		writeError(w, 400, "self_deactivation", "cannot deactivate your own account")
		return
	}
	tx, err := s.store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		writeError(w, 500, "database_error", "could not update user")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err = lockAdminDelegation(r.Context(), tx); err != nil {
		writeError(w, 500, "database_error", "could not lock role delegation")
		return
	}
	roleIDs, err := s.resolveRoleIDs(r.Context(), tx, input.Roles)
	if err != nil {
		writeError(w, 400, "invalid_role", err.Error())
		return
	}
	if err = validateDelegatedRoles(r.Context(), tx, actor.UserID, roleIDs); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	var currentActive, currentlyAdmin bool
	if err = tx.QueryRow(r.Context(), `SELECT active FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&currentActive); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "user not found")
		return
	} else if err != nil {
		writeError(w, 500, "database_error", "could not load user")
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM user_roles WHERE user_id=$1 AND role_id='role-admin')`, id).Scan(&currentlyAdmin); err != nil {
		writeError(w, 500, "database_error", "could not load user roles")
		return
	}
	if err = validateTargetUserAuthority(r.Context(), tx, actor.UserID, id); err != nil {
		writeError(w, http.StatusForbidden, "delegation_forbidden", err.Error())
		return
	}
	if id == actor.UserID && currentlyAdmin && !contains(roleIDs, "role-admin") {
		writeError(w, 400, "self_lockout", "cannot remove your own Administrator role")
		return
	}
	if err = protectLastActiveAdmin(r.Context(), tx, id, active, roleIDs); err != nil {
		writeError(w, 409, "last_active_admin", err.Error())
		return
	}
	input.Username = store.NormalizeUsername(input.Username)
	tag, err := tx.Exec(r.Context(), `UPDATE users SET username=CASE WHEN auth_source='local' THEN $2 ELSE username END,display_name=$3,email=$4,active=$5,updated_at=now() WHERE id=$1`, id, input.Username, input.DisplayName, input.Email, active)
	if err == nil && tag.RowsAffected() == 0 {
		err = errors.New("not found")
	}
	if err == nil && input.Password != "" {
		if len(input.Password) < 12 {
			writeError(w, 400, "invalid_password", "password must contain at least 12 characters")
			return
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		_, err = tx.Exec(r.Context(), `UPDATE users SET password_hash=$2 WHERE id=$1 AND auth_source='local'`, id, string(hash))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM user_roles WHERE user_id=$1`, id)
	}
	for _, roleID := range roleIDs {
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, id, roleID)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 409, "user_conflict", "could not update user")
		return
	}
	s.store.Audit(r.Context(), actor.UserID, "user.update", "user", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, 200, map[string]any{"id": id, "username": input.Username, "displayName": input.DisplayName, "email": input.Email, "roles": roleIDs, "active": active})
}
