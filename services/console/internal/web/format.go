package web

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// microPerUSD is how many micro-units make one US dollar.
const microPerUSD = 1_000_000

// usd formats micro-units as dollars with six decimals, such as $12.000345.
func usd(micro int64) string {
	sign := ""
	if micro < 0 {
		sign, micro = "-", -micro
	}
	return fmt.Sprintf("%s$%d.%06d", sign, micro/microPerUSD, micro%microPerUSD)
}

// perMillion formats a per-token price in micro-units as dollars per million tokens.
func perMillion(micro int64) string {
	return fmt.Sprintf("$%d / 1M tokens", micro)
}

var errAmount = errors.New("amount must be a dollar amount such as 10 or -2.5, with at most 6 decimals")

// parseUSD reads a dollar amount, such as "10", "-2.5" or "0.000001", into
// micro-units without floating point.
func parseUSD(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" || len(frac) > 6 || len(whole) > 12 {
		return 0, errAmount
	}
	if whole == "" {
		whole = "0"
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err1 := strconv.ParseUint(whole, 10, 64)
	f, err2 := strconv.ParseUint(frac, 10, 64)
	if err1 != nil || err2 != nil || w > math.MaxInt64/microPerUSD {
		return 0, errAmount
	}
	micro := int64(w)*microPerUSD + int64(f)
	if negative {
		micro = -micro
	}
	return micro, nil
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

var funcs = template.FuncMap{
	"usd":        usd,
	"perMillion": perMillion,
	"usdPtr": func(p *int64) string {
		if p == nil {
			return "—"
		}
		return usd(*p)
	},
	"intPtr": func(p *int64) string {
		if p == nil {
			return "—"
		}
		return strconv.FormatInt(*p, 10)
	},
	"strPtr": func(p *string) string {
		if p == nil || *p == "" {
			return "—"
		}
		return *p
	},
	"deref": deref[string],
}

// timeIn formats t in loc for tables.
func timeIn(loc *time.Location) func(time.Time) string {
	return func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04:05") }
}
