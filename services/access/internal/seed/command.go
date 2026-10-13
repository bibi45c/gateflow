package seed

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

// Command runs `access seed [--tenants n]`, given the arguments after "seed". It
// applies the migrations, seeds the database at DATABASE_URL and writes one
// "tenant_id key" line per tenant to stdout, the only place a key is ever written.
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
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	keys, err := Run(ctx, pool, cfg, *tenants)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := fmt.Fprintln(stdout, k.TenantID, k.Key); err != nil {
			return err
		}
	}
	return nil
}
