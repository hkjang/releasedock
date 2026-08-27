package server

import (
	"context"
	"errors"

	"github.com/hkjang/releasedock/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	errDelegationEscalation = errors.New("requested role or permission exceeds the actor's authority")
	errTargetAuthority      = errors.New("the target currently has authority that exceeds the actor's authority")
	errProtectedAdminOnly   = errors.New("only an existing protected Administrator may delegate Administrator")
	errLastActiveAdmin      = errors.New("the last active Administrator cannot be removed or deactivated")
	errProtectedAdminRole   = errors.New("the protected Administrator role cannot be modified")
)

func permissionSubset(granted, requested []string) bool {
	allowed := make(map[string]bool, len(granted))
	for _, permission := range granted {
		allowed[permission] = true
	}
	for _, permission := range requested {
		if !allowed[permission] {
			return false
		}
	}
	return true
}

func lockAdminDelegation(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(752199330073)`)
	return err
}

func actorAuthority(ctx context.Context, tx pgx.Tx, actorID string) ([]string, bool, error) {
	var permissions []string
	var protectedAdmin bool
	err := tx.QueryRow(ctx, `SELECT
		COALESCE(array_agg(DISTINCT rp.permission_code) FILTER(WHERE rp.permission_code IS NOT NULL),'{}'),
		COALESCE(bool_or(ur.role_id='role-admin'),FALSE)
		FROM users u LEFT JOIN user_roles ur ON ur.user_id=u.id
		LEFT JOIN role_permissions rp ON rp.role_id=ur.role_id
		WHERE u.id=$1 AND u.active GROUP BY u.id`, actorID).Scan(&permissions, &protectedAdmin)
	if err != nil {
		return nil, false, err
	}
	principal, ok := ctx.Value(principalKey).(store.Principal)
	effective, effectiveAdmin := attenuateDelegatedAuthority(permissions, protectedAdmin, principal, ok && principal.UserID == actorID)
	return effective, effectiveAdmin, nil
}

func attenuateDelegatedAuthority(permissions []string, protectedAdmin bool, principal store.Principal, matchesActor bool) ([]string, bool) {
	if !matchesActor || !principal.ViaAPIKey {
		return permissions, protectedAdmin
	}
	effective := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		if principal.Has(permission) {
			effective = append(effective, permission)
		}
	}
	// Protected Administrator delegation is deliberately session-only. An API
	// key remains useful for bounded administration but cannot mint an admin.
	return effective, false
}

func validateDelegatedPermissions(ctx context.Context, tx pgx.Tx, actorID string, requested []string) error {
	actorPermissions, _, err := actorAuthority(ctx, tx, actorID)
	if err != nil {
		return err
	}
	if !permissionSubset(actorPermissions, requested) {
		return errDelegationEscalation
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM permissions WHERE code=ANY($1)`, requested).Scan(&count); err != nil {
		return err
	}
	if count != len(requested) {
		return errors.New("one or more permissions do not exist")
	}
	return nil
}

func validateDelegatedRoles(ctx context.Context, tx pgx.Tx, actorID string, roleIDs []string) error {
	actorPermissions, protectedAdmin, err := actorAuthority(ctx, tx, actorID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, roleID := range roleIDs {
		if seen[roleID] {
			return errors.New("roles must not contain duplicates")
		}
		seen[roleID] = true
		if roleID == "role-admin" && !protectedAdmin {
			return errProtectedAdminOnly
		}
		var permissions []string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(rp.permission_code) FILTER(WHERE rp.permission_code IS NOT NULL),'{}')
			FROM roles r LEFT JOIN role_permissions rp ON rp.role_id=r.id WHERE r.id=$1 GROUP BY r.id`, roleID).Scan(&permissions); err != nil {
			return errors.New("one or more roles do not exist")
		}
		if !permissionSubset(actorPermissions, permissions) {
			return errDelegationEscalation
		}
	}
	return nil
}

func roleAuthorityForUpdate(ctx context.Context, tx pgx.Tx, roleID string) ([]string, bool, error) {
	var system bool
	if err := tx.QueryRow(ctx, `SELECT system FROM roles WHERE id=$1 FOR UPDATE`, roleID).Scan(&system); err != nil {
		return nil, false, err
	}
	var permissions []string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(permission_code ORDER BY permission_code),'{}') FROM role_permissions WHERE role_id=$1`, roleID).Scan(&permissions); err != nil {
		return nil, false, err
	}
	return permissions, system, nil
}

// validateRoleMutationTarget prevents an actor from editing or deleting a role
// that already carries authority the actor does not possess. Administrator is a
// protected bootstrap role and is deliberately immutable through these APIs.
func validateRoleMutationTarget(ctx context.Context, tx pgx.Tx, actorID, roleID string) (bool, error) {
	actorPermissions, _, err := actorAuthority(ctx, tx, actorID)
	if err != nil {
		return false, err
	}
	permissions, system, err := roleAuthorityForUpdate(ctx, tx, roleID)
	if err != nil {
		return false, err
	}
	if roleID == "role-admin" {
		return system, errProtectedAdminRole
	}
	if !permissionSubset(actorPermissions, permissions) {
		return system, errTargetAuthority
	}
	return system, nil
}

func validateRolePermissionMutation(ctx context.Context, tx pgx.Tx, actorID, roleID string, requested []string) error {
	if _, err := validateRoleMutationTarget(ctx, tx, actorID, roleID); err != nil {
		return err
	}
	return validateDelegatedPermissions(ctx, tx, actorID, requested)
}

func validateTargetUserAuthority(ctx context.Context, tx pgx.Tx, actorID, targetID string) error {
	actorPermissions, actorAdmin, err := actorAuthority(ctx, tx, actorID)
	if err != nil {
		return err
	}
	var targetPermissions []string
	var targetAdmin bool
	err = tx.QueryRow(ctx, `SELECT
		COALESCE(array_agg(DISTINCT rp.permission_code) FILTER(WHERE rp.permission_code IS NOT NULL),'{}'),
		COALESCE(bool_or(ur.role_id='role-admin'),FALSE)
		FROM users u LEFT JOIN user_roles ur ON ur.user_id=u.id
		LEFT JOIN role_permissions rp ON rp.role_id=ur.role_id
		WHERE u.id=$1 GROUP BY u.id`, targetID).Scan(&targetPermissions, &targetAdmin)
	if err != nil {
		return err
	}
	if targetAdmin && !actorAdmin {
		return errProtectedAdminOnly
	}
	if !permissionSubset(actorPermissions, targetPermissions) {
		return errTargetAuthority
	}
	return nil
}

func protectLastActiveAdmin(ctx context.Context, tx pgx.Tx, userID string, nextActive bool, nextRoleIDs []string) error {
	var currentlyActiveAdmin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN user_roles ur ON ur.user_id=u.id WHERE u.id=$1 AND u.active AND ur.role_id='role-admin')`, userID).Scan(&currentlyActiveAdmin); err != nil {
		return err
	}
	if !currentlyActiveAdmin || nextActive && contains(nextRoleIDs, "role-admin") {
		return nil
	}
	var activeAdmins int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT u.id) FROM users u JOIN user_roles ur ON ur.user_id=u.id WHERE u.active AND ur.role_id='role-admin'`).Scan(&activeAdmins); err != nil {
		return err
	}
	if activeAdmins <= 1 {
		return errLastActiveAdmin
	}
	return nil
}
