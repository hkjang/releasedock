package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL DSN: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(752199330072)`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(752199330072)`) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied bool
		err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, entry.Name()).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sqlBytes)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, entry.Name())
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// BootstrapAdmin creates the initial administrator, or safely restores the
// Administrator role to an existing bootstrap account. It never overwrites an
// existing password.
func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) error {
	username = NormalizeUsername(username)
	if username == "" {
		return errors.New("bootstrap username cannot be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var userID string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(username)=lower($1) FOR UPDATE`, username).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		userID, err = secure.NewID()
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO users(id,username,password_hash,display_name,auth_source) VALUES($1,$2,$3,$2,'local')`, userID, username, string(hash))
	}
	if err != nil {
		return fmt.Errorf("bootstrap admin lookup/create: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_roles(user_id,role_id) VALUES($1,'role-admin') ON CONFLICT DO NOTHING`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

type Principal struct {
	UserID      string   `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	AuthSource  string   `json:"source"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Scopes      []string `json:"scopes,omitempty"`
	ViaAPIKey   bool     `json:"-"`
	CSRFHash    []byte   `json:"-"`
}

func (p Principal) Has(permission string) bool {
	for _, granted := range p.Permissions {
		if granted == permission {
			if !p.ViaAPIKey {
				return true
			}
			for _, scope := range p.Scopes {
				if scope == permission || scope == "*" {
					return true
				}
			}
		}
	}
	return false
}

func (s *Store) principal(ctx context.Context, userID string) (Principal, error) {
	var p Principal
	err := s.Pool.QueryRow(ctx, `SELECT id,username,display_name,email,auth_source FROM users WHERE id=$1 AND active=TRUE`, userID).
		Scan(&p.UserID, &p.Username, &p.DisplayName, &p.Email, &p.AuthSource)
	if err != nil {
		return Principal{}, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT rp.permission_code FROM user_roles ur JOIN role_permissions rp ON rp.role_id=ur.role_id WHERE ur.user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return Principal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return Principal{}, err
		}
		p.Permissions = append(p.Permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return Principal{}, err
	}
	roleRows, err := s.Pool.Query(ctx, `SELECT CASE r.id WHEN 'role-admin' THEN 'admin' WHEN 'role-operator' THEN 'operator' WHEN 'role-viewer' THEN 'viewer' ELSE r.name END FROM roles r JOIN user_roles ur ON ur.role_id=r.id WHERE ur.user_id=$1 ORDER BY 1`, userID)
	if err != nil {
		return Principal{}, err
	}
	defer roleRows.Close()
	for roleRows.Next() {
		var role string
		if err := roleRows.Scan(&role); err != nil {
			return Principal{}, err
		}
		p.Roles = append(p.Roles, role)
	}
	return p, roleRows.Err()
}

func (s *Store) AuthenticateSession(ctx context.Context, token string) (Principal, error) {
	hash := secure.TokenHash(token)
	var userID string
	var csrfHash []byte
	err := s.Pool.QueryRow(ctx, `
		UPDATE sessions SET last_seen_at=now()
		WHERE token_hash=$1 AND expires_at>now()
		RETURNING user_id,csrf_hash`, hash).Scan(&userID, &csrfHash)
	if err != nil {
		return Principal{}, err
	}
	p, err := s.principal(ctx, userID)
	if err != nil {
		return Principal{}, err
	}
	p.CSRFHash = csrfHash
	return p, nil
}

func (s *Store) AuthenticateAPIKey(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, "rdk_") || len(token) < 24 {
		return Principal{}, pgx.ErrNoRows
	}
	hash := secure.TokenHash(token)
	var userID string
	var scopes []string
	err := s.Pool.QueryRow(ctx, `
		UPDATE api_keys SET last_used_at=now()
		WHERE secret_hash=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())
		RETURNING user_id,scopes`, hash).Scan(&userID, &scopes)
	if err != nil {
		return Principal{}, err
	}
	p, err := s.principal(ctx, userID)
	if err != nil {
		return Principal{}, err
	}
	p.ViaAPIKey = true
	p.Scopes = scopes
	return p, nil
}

func (s *Store) Audit(ctx context.Context, actorID, action, resourceType, resourceID, outcome, ip, userAgent string, details []byte) {
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	_, _ = s.Pool.Exec(ctx, `INSERT INTO audit_logs(actor_id,action,resource_type,resource_id,outcome,ip,user_agent,details) VALUES(NULLIF($1,''),$2,$3,$4,$5,$6,$7,$8)`, actorID, action, resourceType, resourceID, outcome, ip, userAgent, details)
}
