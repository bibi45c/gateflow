package seed

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

// Command runs `access seed [--tenants n]`, given the arguments after "seed". It
// applies the migrations, seeds the database at DATABASE_URL and writes one
// "tenant_id key" line per tenant to stdout, the only place a key is ever written.
// It also seeds the console users whose passwords are set, and names them on stderr.
// Usage messages go to stderr.
func Command(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("access seed", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenants := flags.Int("tenants", 10, "seed tenant-00 to tenant-`n`")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("seed: unexpected argument %q", flags.Arg(0))
	}
	if *tenants < 0 {
		return errors.New("seed: --tenants must be 0 or more")
	}
	cfg, err := config.LoadSeed(getenv)
	if err != nil {
		return err
	}
	// Refuse bad channel settings before touching the database.
	if _, err := seedChannels(cfg); err != nil {
		return err
	}
	// The seed keeps pgx's default pool size.
	pool, err := store.Open(ctx, cfg.DatabaseURL, 0)
	if err != nil {
		return err
	}
	defer pool.Close()
	keys, err := Run(ctx, pool, cfg, *tenants)
	if err != nil {
		return err
	}
	users, err := Console(ctx, pool, cfg)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := fmt.Fprintln(stdout, k.TenantID, k.Key); err != nil {
			return err
		}
	}
	if len(users) > 0 {
		fmt.Fprintln(stderr, "access seed: console users", strings.Join(users, ", "))
	}
	return nil
}
