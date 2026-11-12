package provider

// A balance field's windows: a key whose vendor tells its plan's windows
// at a query magpie doesn't know (a reseller's coding plan, a panel with a
// 5-hour and a weekly cap) can say so in the balance field the user wrote.
// A labeled part asked as a share used ("%") and followed by "resets
// <path>" is a window: "5h: plan.windows.0.used / plan.windows.0.limit %
// resets plan.windows.0.resetsAt every 5h". The key's card shows it as a
// window, and routing weighs and rests the key by it, as it does a sub2api
// key's limits or a plan key's windows (key_allowance.go). A part without
// "resets" is an amount on the card as before, so a balance field written
// before routes as it did.
//
// The reset is a time in the reply: RFC 3339 text, or a number of seconds
// (milliseconds above 1e11) since 1970. "every" is how long the window
// runs, a span written as 90m, 5h, 7d or 2w, or a path to its seconds in
// the reply; a window under a day is then a rate cap to routing, as a
// plan's 5 hours are, and one without "every" counts as a budget. A share
// is clamped to 0–100. A window part that can't be read whole — its share,
// its reset or its span — is "—" on the card, with why, and no window: a
// failed read is unknown, never 0% used.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/lazyre"
)

// balanceWindowClause is what makes a part of a balance field a window:
// "%" (a share), then "resets <path>", and "every <span>" if given. "%"
// is only ever the last character of an amount, so no balance field
// written before has it followed by anything.
var balanceWindowClause = lazyre.New(`%\s+resets\s+(\S+)(?:\s+every\s+(\S+))?\s*$`)

// balanceWindowSpan is a span written out: 90m, 5h, 7d, 2w.
var balanceWindowSpan = lazyre.New(`^(\d+(?:\.\d+)?)([mhdw])$`)

// balancePathWindows says p's balance field names windows of its own,
// said without asking anything, as a request routes by it. A key magpie
// reads a plan's windows of (planQuotaSourceOf, accountPlanOf) keeps
// those: its field's window parts are amounts on its card, as before.
func balancePathWindows(p Provider) bool {
	if readsPlanWindows(p) {
		return false
	}
	p = keyQueryOf(p)
	if p.BalanceURL == "" {
		return false
	}
	for part := range strings.SplitSeq(p.BalancePath, ";") {
		if balanceWindowClause.MatchString(part) {
			return true
		}
	}
	return false
}

// readsPlanWindows says magpie reads p's key's windows from its plan,
// which a balance field's windows don't stand in for.
func readsPlanWindows(p Provider) bool {
	if _, ok := planQuotaSourceOf(p); ok {
		return true
	}
	return accountPlanOf(p) != ""
}

// readBalanceWindow reads a part of a balance field that is a window, its
// label its name, the clause after its share cut off; ok is false for a
// part that isn't one.
func readBalanceWindow(b []byte, label, expr string) (part BalancePart, ok bool, err error) {
	m := balanceWindowClause.FindStringSubmatchIndex(expr)
	if m == nil {
		return BalancePart{}, false, nil
	}
	if label == "" {
		return BalancePart{}, true, errors.New(`a window needs a label to be its name, e.g. "5h: a.used / a.limit % resets a.resetsAt"`)
	}
	share := strings.TrimSpace(expr[:m[0]+1]) // through the "%"
	resetsPath := expr[m[2]:m[3]]
	every := ""
	if m[4] >= 0 {
		every = expr[m[4]:m[5]]
	}
	text, pct, err := readBalanceOne(b, share)
	if err != nil {
		return BalancePart{}, true, err
	}
	if pct == nil || math.IsNaN(*pct) || math.IsInf(*pct, 0) {
		return BalancePart{}, true, errors.New("the window's share is not a number")
	}
	resets, err := balanceTimeAt(b, resetsPath)
	if err != nil {
		return BalancePart{}, true, fmt.Errorf("resets: %w", err)
	}
	var span time.Duration
	if every != "" {
		if span, err = balanceSpanOf(b, every); err != nil {
			return BalancePart{}, true, fmt.Errorf("every: %w", err)
		}
	}
	used := min(100, max(0, *pct))
	w := QuotaWindow{Name: label, Used: used, ResetsAt: &resets, Span: span}
	return BalancePart{Label: label, Text: text, Percent: &used, window: &w}, true, nil
}

// balanceTimeAt is the time at path in the reply: RFC 3339 text, or
// seconds since 1970, milliseconds when the number is above 1e11 (a
// year past 5000 in seconds).
func balanceTimeAt(b []byte, path string) (time.Time, error) {
	v, err := balanceValueAt(b, path)
	if err != nil {
		return time.Time{}, err
	}
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	n, ok := number(v)
	if !ok || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return time.Time{}, fmt.Errorf("%q in the reply is not a time", path)
	}
	if n > 1e11 {
		return time.UnixMilli(int64(n)), nil
	}
	return time.Unix(int64(n), 0), nil
}

// balanceSpanOf is a window's span: written out (5h, 7d), or the
// seconds at a path in the reply.
func balanceSpanOf(b []byte, s string) (time.Duration, error) {
	if m := balanceWindowSpan.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		if d := time.Duration(n * float64(unit)); d > 0 {
			return d, nil
		}
		return 0, fmt.Errorf("%q is no span", s)
	}
	v, err := balanceValueAt(b, s)
	if err != nil {
		return 0, err
	}
	n, ok := number(v)
	if !ok || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, fmt.Errorf("%q in the reply is not a number of seconds", s)
	}
	return time.Duration(n * float64(time.Second)), nil
}

// balanceValueAt is the one value at a dotted path in the reply.
func balanceValueAt(b []byte, path string) (any, error) {
	var reply any
	if err := json.Unmarshal(b, &reply); err != nil {
		return nil, errors.New("the reply is not JSON")
	}
	e := &balanceExpr{src: path, reply: reply}
	return e.at(path)
}

// balanceWindows takes the windows out of a balance field's parts: the
// parts left are the card's amounts, and the windows are the key's own,
// for its card and for routing.
func balanceWindows(parts []BalancePart) (rest []BalancePart, ws []QuotaWindow) {
	for _, p := range parts {
		if p.window != nil {
			ws = append(ws, *p.window)
			continue
		}
		rest = append(rest, p)
	}
	return rest, ws
}
