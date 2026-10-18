package seed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/password"
)

// DemoTenantID is the tenant of the console's demo user. It has no seeded key: the
// demo user creates one in the console.
const DemoTenantID = "tenant-demo"

// Console usernames.
const (
	AdminUsername = "admin"
	DemoUsername  = "demo"
)

// Console seeds the console users whose passwords cfg holds: admin, and demo with
// its tenant tenant-demo. A user that exists gets the current password, so changing
// the variable and seeding again changes it. It returns the usernames it seeded.
func Console(ctx context.Context, pool *pgxpool.Pool, cfg config.Seed) ([]string, error) {
	type user struct {
		id, name, password, role string
		tenant                   *string
	}
	demoTenant := DemoTenantID
	var users []user
	if cfg.AdminPassword != "" {
		users = append(users, user{"user-admin", AdminUsername, cfg.AdminPassword, "admin", nil})
	}
	if cfg.DemoPassword != "" {
		users = append(users, user{"user-demo", DemoUsername, cfg.DemoPassword, "user", &demoTenant})
	}
	if len(users) == 0 {
		return nil, nil
	}
	hashes := make([]string, len(users))
	for i, u := range users {
		hash, err := password.Hash(u.password)
		if err != nil {
			return nil, fmt.Errorf("seed console users: %w", err)
		}
		hashes[i] = hash
	}
	var seeded []string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if cfg.DemoPassword != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO access.tenants (id, name) VALUES ($1, 'Demo tenant')
				ON CONFLICT (id) DO NOTHING`, DemoTenantID); err != nil {
				return fmt.Errorf("seed demo tenant: %w", err)
			}
		}
		for i, u := range users {
			if _, err := tx.Exec(ctx, `INSERT INTO access.console_users (id, username, password_hash, role, tenant_id)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (username) DO UPDATE SET password_hash = excluded.password_hash`,
				u.id, u.name, hashes[i], u.role, u.tenant); err != nil {
				return fmt.Errorf("seed console user %s: %w", u.name, err)
			}
			seeded = append(seeded, u.name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return seeded, nil
}
