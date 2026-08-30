//go:build ignore
// +build ignore
package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)

	// role_permissions seed
	rps := []struct{ role, perm string }{
		{"owner", "*"}, {"owner", "admin"},
		{"manager", "session:create"}, {"manager", "session:read"},
		{"manager", "session:update"}, {"manager", "session:delete"},
		{"manager", "row:update"}, {"manager", "row:delete"},
		{"manager", "plan:read"}, {"manager", "plan:create"},
		{"manager", "restock:feedback"},
		{"manager", "inventory:view"}, {"manager", "inventory:adjust"},
		{"manager", "report:view"}, {"manager", "user:manage"},
		{"buyer", "session:create"}, {"buyer", "session:read"},
		{"buyer", "session:update"}, {"buyer", "session:delete"},
		{"buyer", "row:update"}, {"buyer", "row:delete"},
		{"buyer", "plan:read"}, {"buyer", "plan:create"},
		{"buyer", "plan:approve"}, {"buyer", "inventory:view"},
		{"cashier", "session:read"}, {"cashier", "plan:read"},
		{"cashier", "restock:feedback"},
		{"floor", "plan:read"}, {"floor", "restock:feedback"},
		{"office", "plan:read"}, {"office", "plan:create"},
		{"office", "inventory:view"}, {"office", "report:view"},
		{"office", "restock:feedback"},
	}
	for _, rp := range rps {
		_, _ = conn.Exec(ctx, `INSERT INTO role_permissions (role_id, perm_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, rp.role, rp.perm)
	}
	fmt.Printf("role_permissions: %d\n", len(rps))

	// user_roles seed
	urs := []struct {
		user, role, scopeType, scopeID string
		primary                        bool
	}{
		{"u_owner", "owner", "platform", "", true},
		{"u_manager", "manager", "store", "0001", true},
		{"u_buyer", "buyer", "platform", "", true},
		{"u_cashier", "cashier", "store", "0001", true},
		{"u_cashier", "floor", "store", "0001", false},
	}
	for _, ur := range urs {
		_, _ = conn.Exec(ctx, `
			INSERT INTO user_roles (user_id, role_id, scope_type, scope_id, is_primary, granted_by)
			VALUES ($1,$2,$3,$4,$5,'seed')
			ON CONFLICT DO NOTHING
		`, ur.user, ur.role, ur.scopeType, ur.scopeID, ur.primary)
	}
	fmt.Printf("user_roles: %d\n", len(urs))
}
