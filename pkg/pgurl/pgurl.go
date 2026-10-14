// Package pgurl reads a Postgres connection string without letting its password
// reach an error message.
//
// pgx quotes a string it cannot parse in its error and hides the password only in
// the forms it recognizes, so `password =secret`, a valid libpq form, shows it. Parse
// with ParseConfig, and pass the errors of a pool made from its result through Redact.
package pgurl

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidURL is the only error ParseConfig returns. It never includes the
// connection string.
var ErrInvalidURL = errors.New("the database URL is not valid; its value is not shown")

// ParseConfig parses url, a Postgres URL or keyword/value string, into a pool
// configuration. It drops pgx's error, which may show the password, and returns
// ErrInvalidURL instead.
func ParseConfig(url string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, ErrInvalidURL
	}
	return cfg, nil
}

// Redact returns err with each occurrence of password in its text replaced by
// xxxxx, or err itself when password is empty or not in the text. The new error does
// not unwrap, so errors.As cannot reach an error that still holds the password;
// errors.Is still matches whatever err matches, such as context.DeadlineExceeded.
func Redact(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, password) {
		return err
	}
	return &redacted{msg: strings.ReplaceAll(msg, password, "xxxxx"), err: err}
}

// redacted is an error whose text no longer shows the password. It keeps the
// original error only to answer Is.
type redacted struct {
	msg string
	err error
}

func (e *redacted) Error() string { return e.msg }

// Is reports whether the original error matches target.
func (e *redacted) Is(target error) bool { return errors.Is(e.err, target) }
