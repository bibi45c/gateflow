package seed

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var planAssignment = regexp.MustCompile(`^([A-Za-z0-9._-]{1,64})=([a-z0-9-]{1,32})$`)

// ParsePlans reads the --plans value "tenant=plan,tenant=plan" into a map. An empty
// value assigns nothing.
func ParsePlans(value string) (map[string]string, error) {
	plans := map[string]string{}
	if value == "" {
		return plans, nil
	}
	for _, item := range strings.Split(value, ",") {
		m := planAssignment.FindStringSubmatch(item)
		if m == nil {
			return nil, fmt.Errorf("seed: --plans: %q is not tenant=plan", item)
		}
		if _, dup := plans[m[1]]; dup {
			return nil, fmt.Errorf("seed: --plans: tenant %s is named twice", m[1])
		}
		plans[m[1]] = m[2]
	}
	return plans, nil
}

// ApplyPlans puts each named tenant on its plan, in one transaction. It fails, and
// changes nothing, when a tenant or a plan does not exist. Applying the same plans
// again changes nothing.
func ApplyPlans(ctx context.Context, pool *pgxpool.Pool, plans map[string]string) error {
	tenants := make([]string, 0, len(plans))
	for tenant := range plans {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, tenant := range tenants {
			tag, err := tx.Exec(ctx, `UPDATE access.tenants SET plan_id = $2 WHERE id = $1`, tenant, plans[tenant])
			var pgErr *pgconn.PgError
			switch {
			case errors.As(err, &pgErr) && pgErr.Code == "23503":
				return fmt.Errorf("seed: --plans: plan %q does not exist", plans[tenant])
			case err != nil:
				return fmt.Errorf("seed: --plans: %w", err)
			case tag.RowsAffected() == 0:
				return fmt.Errorf("seed: --plans: tenant %q does not exist", tenant)
			}
		}
		return nil
	})
}
