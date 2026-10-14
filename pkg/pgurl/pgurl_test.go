package pgurl_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bibi45c/gateflow/pkg/pgurl"
)

// secret stands in for a password; no error text may show it.
const secret = "REVIEW_FAKE_SECRET"

func TestParseConfigReadsThePassword(t *testing.T) {
	for _, url := range []string{
		"host=127.0.0.1 user=review password =" + secret + " port=5432",
		"postgres://review:" + secret + "@127.0.0.1:5432/db",
	} {
		cfg, err := pgurl.ParseConfig(url)
		if err != nil {
			t.Errorf("%s: %v", url, err)
			continue
		}
		if cfg.ConnConfig.User != "review" || cfg.ConnConfig.Password != secret {
			t.Errorf("%s: user %q, password %q", url, cfg.ConnConfig.User, cfg.ConnConfig.Password)
		}
	}
}

func TestParseConfigHidesAStringItCannotParse(t *testing.T) {
	cases := []struct {
		name, url string
		hidden    []string // forms of the password the error must not show
	}{
		{"space before =", "host=127.0.0.1 user=review password =" + secret + " port=invalid", []string{secret}},
		{"keyword/value", "host=127.0.0.1 user=review password=" + secret + " port=invalid", []string{secret}},
		{"quoted with a space", "password='CAN ARY' port=invalid", []string{"CAN ARY", "CAN", "ARY"}},
		{"quoted with an escaped quote", `password='CAN\'ARY' port=invalid`, []string{`CAN\'ARY`, "CAN'ARY", "CAN", "ARY"}},
		{"URL", "postgres://review:CANARY@127.0.0.1:invalid/db", []string{"CANARY"}},
		{"URL with an escape", "postgres://review:CAN%41RY@127.0.0.1:invalid/db", []string{"CAN%41RY", "CANARY"}},
	}
	for _, c := range cases {
		cfg, err := pgurl.ParseConfig(c.url)
		if cfg != nil || !errors.Is(err, pgurl.ErrInvalidURL) {
			t.Errorf("%s: got %v, want ErrInvalidURL", c.name, err)
			continue
		}
		var parseErr *pgconn.ParseConfigError
		if errors.As(err, &parseErr) {
			t.Errorf("%s: the error still holds pgx's", c.name)
		}
		for _, form := range c.hidden {
			if strings.Contains(err.Error(), form) {
				t.Errorf("%s: %q shows %q", c.name, err, form)
			}
		}
	}
}

func TestRedactReplacesEveryOccurrence(t *testing.T) {
	err := pgurl.Redact(fmt.Errorf("password %s for review: %s rejected", secret, secret), secret)
	if want := "password xxxxx for review: xxxxx rejected"; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
}

func TestRedactReturnsOtherErrorsUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		password string
	}{
		{"empty password", fmt.Errorf("password %s rejected", secret), ""},
		{"password not in the text", errors.New("connection refused"), secret},
		{"no error", nil, secret},
	}
	for _, c := range cases {
		if got := pgurl.Redact(c.err, c.password); got != c.err {
			t.Errorf("%s: got %v, want the error unchanged", c.name, got)
		}
	}
}

func TestRedactedErrorDoesNotUnwrap(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"server error", fmt.Errorf("ping: %w",
			&pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password " + secret + " rejected"})},
		{"connect error", connectError(t)},
	}
	for _, c := range cases {
		err := pgurl.Redact(c.err, secret)
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: %q shows the password", c.name, err)
		}
		var pgErr *pgconn.PgError
		var connectErr *pgconn.ConnectError
		if errors.Unwrap(err) != nil || errors.As(err, &pgErr) || errors.As(err, &connectErr) {
			t.Errorf("%s: the original error is still reachable", c.name)
		}
	}
}

func TestRedactedErrorKeepsErrorsIs(t *testing.T) {
	err := pgurl.Redact(fmt.Errorf("connect with %s: %w", secret, context.DeadlineExceeded), secret)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is no longer finds context.DeadlineExceeded")
	}
	if errors.Is(err, context.Canceled) {
		t.Error("errors.Is finds context.Canceled, which the original does not hold")
	}
}

// connectError returns the *pgconn.ConnectError that pgconn gives when dialing fails
// with an error that shows the password. Nothing goes over the network.
func connectError(t *testing.T) error {
	t.Helper()
	cfg, err := pgconn.ParseConfig("host=127.0.0.1 user=review password=" + secret)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("refused password %s", secret)
	}
	_, err = pgconn.ConnectConfig(context.Background(), cfg)
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) || !strings.Contains(err.Error(), secret) {
		t.Fatalf("got %v, want a *pgconn.ConnectError that shows the password", err)
	}
	return err
}
