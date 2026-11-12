package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// planReply is a coding plan's billing reply as a reseller's panel
// answers it: a 5-hour and a 7-day window, each its credits used of its
// limit, when it resets and how long it runs, beside the wallet. The
// 5-hour window resets as RFC 3339 text, the week as milliseconds.
func planReply(five, week time.Time) []byte {
	return []byte(`{"wallet":2.5,"plan":{"tier":"go","windows":[` +
		`{"durationSeconds":18000,"usedCredits":9000,"limitCredits":60000,"resetsAt":"` + five.UTC().Format(time.RFC3339Nano) + `"},` +
		`{"durationSeconds":604800,"usedCredits":14000,"limitCredits":200000,"resetsAt":` + strconv.FormatInt(week.UnixMilli(), 10) + `}]}}`)
}

const planWindowsPath = "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits % resets plan.windows.0.resetsAt every 5h; " +
	"7d: plan.windows.1.usedCredits / plan.windows.1.limitCredits % resets plan.windows.1.resetsAt every plan.windows.1.durationSeconds; " +
	"$wallet"

// A part of a balance field asked as a share and followed by "resets" is
// a window of the key's own, by its label, share, reset and span; the
// other parts are amounts, as they were.
func TestBalanceFieldWindows(t *testing.T) {
	five := time.Now().Add(2*time.Hour + 30*time.Minute).Truncate(time.Second)
	week := time.Now().Add(160 * time.Hour).Truncate(time.Millisecond)
	parts, err := readBalanceParts(planReply(five, week), planWindowsPath)
	if err != nil {
		t.Fatal(err)
	}
	rest, ws := balanceWindows(parts)
	if len(rest) != 1 || rest[0].Text != "$2.50" || rest[0].Label != "" {
		t.Fatalf("amounts %+v, want the wallet alone", rest)
	}
	if len(ws) != 2 {
		t.Fatalf("windows %+v", ws)
	}
	want := []struct {
		name   string
		used   float64
		resets time.Time
		span   time.Duration
	}{
		{"5h", 15, five, 5 * time.Hour},
		{"7d", 7, week, 7 * 24 * time.Hour},
	}
	for i, w := range want {
		got := ws[i]
		if got.Name != w.name || got.ResetsAt == nil || !got.ResetsAt.Equal(w.resets) || got.Span != w.span {
			t.Errorf("window %d: %+v, want %s resetting %v over %v", i, got, w.name, w.resets, w.span)
		}
		if d := got.Used - w.used; d > 1e-9 || d < -1e-9 {
			t.Errorf("window %d: used %v, want %v", i, got.Used, w.used)
		}
	}
	// the line every other caller of the field reads still has them all
	if s := joinBalanceParts(parts); s != "5h 15% · 7d 7% · $2.50" {
		t.Errorf("line %q", s)
	}
}

// What a window part can't read whole is unknown: "—" on the card with
// why, and no window, never a window 0% used. A share past 100 or under 0
// is clamped; a field written before, with no "resets", has no windows.
func TestBalanceFieldWindowEdges(t *testing.T) {
	five := time.Now().Add(time.Hour).Truncate(time.Second)
	reply := planReply(five, five.Add(100*time.Hour))
	for _, c := range []struct {
		name, path string
		windows    int
		bad        string // the part's error says it
	}{
		{"no such reset", "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits % resets plan.windows.0.gone; $wallet", 0, "5h: resets: "},
		{"reset not a time", "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits % resets plan.tier; $wallet", 0, `5h: resets: "plan.tier" in the reply is not a time`},
		{"no such share", "5h: plan.windows.0.nothing / plan.windows.0.limitCredits % resets plan.windows.0.resetsAt; $wallet", 0, "nothing"},
		{"span not seconds", "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits % resets plan.windows.0.resetsAt every plan.tier; $wallet", 0, `5h: every: "plan.tier" in the reply is not a number of seconds`},
		{"a field from before", "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits %; $wallet", 0, ""},
		{"a field named resets", "$plan.windows.0.resets; $wallet", 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			parts, err := readBalanceParts(reply, c.path)
			if err != nil {
				t.Fatal(err) // the wallet reads: the field as a whole does
			}
			rest, ws := balanceWindows(parts)
			if len(ws) != c.windows {
				t.Fatalf("windows %+v", ws)
			}
			if c.bad == "" {
				if c.name == "a field from before" && (len(rest) != 2 || rest[0].Percent == nil || rest[0].Error != "") {
					t.Errorf("the share as it was, an amount with its bar: %+v", rest)
				}
				return
			}
			if len(rest) != 2 || rest[0].Text != "—" || !strings.Contains(rest[0].Error, c.bad) || strings.Contains(rest[0].Error, "can't read") {
				t.Errorf("parts %+v, want the window's part unknown, saying %q", rest, c.bad)
			}
			if rest[1].Text != "$2.50" {
				t.Errorf("the wallet: %+v", rest[1])
			}
		})
	}

	over := []byte(`{"a":{"used":120,"limit":100,"at":1900000000},"b":{"used":-5,"limit":100,"at":1900000000}}`)
	parts, err := readBalanceParts(over, "over: a.used / a.limit % resets a.at; under: b.used / b.limit % resets b.at")
	if err != nil {
		t.Fatal(err)
	}
	_, ws := balanceWindows(parts)
	if len(ws) != 2 || ws[0].Used != 100 || ws[1].Used != 0 {
		t.Fatalf("clamped: %+v", ws)
	}
	// seconds since 1970, below 1e11
	if !ws[0].ResetsAt.Equal(time.Unix(1900000000, 0)) {
		t.Errorf("reset %v", ws[0].ResetsAt)
	}
	// a window with no label has no name to go by
	if _, err := readBalanceParts(over, "a.used / a.limit % resets a.at"); err == nil || !strings.Contains(err.Error(), "label") {
		t.Errorf("no label: %v", err)
	}
}

// A key whose balance field names windows gives routing those windows,
// read behind the request as a sub2api key's limits are, and its card
// shows them as windows beside the amounts; a key whose field names none
// reads no windows and is asked nothing for routing.
func TestBalanceFieldWindowsRoute(t *testing.T) {
	keyLimitsHome(t)
	five := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	week := time.Now().Add(150 * time.Hour).Truncate(time.Millisecond)
	var asked atomic.Int32
	var gone atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer acct-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		asked.Add(1)
		if gone.Load() {
			// the plan's windows left out of a reply that is still 200
			w.Write([]byte(`{"wallet":2.5,"plan":{"tier":"go","windows":[]}}`))
			return
		}
		w.Write(planReply(five, week))
	}))
	defer srv.Close()
	plan := Provider{ID: "plan", Name: "Plan", Key: "sk-plan", Chat: srv.URL + "/v1",
		BalanceURL: srv.URL + "/billing", BalanceToken: "Bearer acct-token", BalancePath: planWindowsPath}
	amounts := Provider{ID: "amounts", Name: "Amounts", Key: "sk-amounts", Chat: srv.URL + "/v1",
		BalanceURL: srv.URL + "/billing", BalanceToken: "Bearer acct-token",
		BalancePath: "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits %; $wallet"}
	for _, p := range []Provider{plan, amounts} {
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if KeyAllowanceID(amounts) != "" || amounts.TakesUsageCap() {
		t.Fatal("a balance field with no windows reads windows")
	}
	if _, ok := KeyAllowance(amounts); ok {
		t.Fatal("a balance field with no windows has an allowance")
	}
	if KeyAllowanceID(plan) == "" || !plan.TakesUsageCap() {
		t.Fatal("a balance field with windows reads none")
	}
	a := waitAllowance(t, plan, func(Allowance) bool { return true })
	now := time.Now()
	used, renews := a.For("", now)
	if want := 15.0; used < want-1e-9 || used > want+1e-9 || len(renews) != 2 {
		t.Fatalf("used %v, renews %v of %+v", used, renews, a)
	}
	// the week is the pace's: what it has left per hour till it resets,
	// the 5 hours a rate cap that doesn't set it
	pace, due := a.Pace("", now)
	if want := (100 - 7.0) / week.Sub(now).Hours(); pace < want*0.99 || pace > want*1.01 || !due.Equal(week) {
		t.Errorf("pace %v due %v, want %v due %v", pace, due, want, week)
	}

	got := map[string]SubscriptionQuota{}
	for _, q := range KeyBalances(context.Background()) {
		got[q.Provider] = q
	}
	q := got["plan"]
	if q.Balance != "$2.50" || len(q.Windows) != 2 || q.Windows[0].Name != "5h" || q.Windows[1].Name != "7d" {
		t.Errorf("plan's card: balance %q windows %+v", q.Balance, q.Windows)
	}
	if q := got["amounts"]; len(q.Windows) != 0 || q.Balance != "5h 15% · $2.50" {
		t.Errorf("amounts' card: balance %q windows %+v", q.Balance, q.Windows)
	}
	// windows alone tell no amount: the balance is none, not $0.00, and
	// the card is the windows with no error
	alone := plan
	alone.ID, alone.Key = "alone", "sk-alone"
	alone.BalancePath = "5h: plan.windows.0.usedCredits / plan.windows.0.limitCredits % resets plan.windows.0.resetsAt every 5h; " +
		"7d: plan.windows.1.usedCredits / plan.windows.1.limitCredits % resets plan.windows.1.resetsAt every 7d"
	if err := Save(alone); err != nil {
		t.Fatal(err)
	}
	if amount, ok, err := Balance(context.Background(), alone); err != nil || !ok || amount != "" {
		t.Errorf("windows alone: %q %v %v", amount, ok, err)
	}
	ForgetBalances()
	for _, q := range KeyBalances(context.Background()) {
		if q.Provider == "alone" && (q.Error != "" || q.Balance != "" || q.BalanceParts != nil || len(q.Windows) != 2) {
			t.Errorf("windows alone, the card: %+v", q)
		}
	}

	// a reply that is 200 but leaves a window out is no reading of it:
	// routing keeps the windows it read last, never goes on with none
	gone.Store(true)
	before := asked.Load()
	StaleKeyAllowance(plan)
	KeyAllowance(plan)
	waitRead(t)
	if asked.Load() == before {
		t.Fatal("not asked again")
	}
	a, ok := KeyAllowance(plan)
	if used, _ := a.For("", time.Now()); !ok || len(a) != 2 || used < 15-1e-9 || used > 15+1e-9 {
		t.Errorf("after a reply without its windows: %v %+v, want the last reading", ok, a)
	}
}

// A key magpie reads a plan's windows of keeps those: windows its balance
// field names are amounts on its card, as before, and routing doesn't
// read them in the plan's place.
func TestBalanceFieldWindowsLeaveAPlanKeys(t *testing.T) {
	keyLimitsHome(t)
	five := time.Now().Add(time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(planReply(five, five.Add(100*time.Hour)))
	}))
	defer srv.Close()
	p := kimiKey
	p.BalanceURL, p.BalancePath = srv.URL+"/billing", planWindowsPath
	if balancePathWindows(p) {
		t.Fatal("a plan key's balance field reads windows in its plan's place")
	}
	amount, _, _, ws, _, ok, err := balanceRead(context.Background(), p)
	if err != nil || !ok || len(ws) != 0 || amount != "5h 15% · 7d 7% · $2.50" {
		t.Errorf("balance %q windows %+v ok %v err %v, want the amounts as before", amount, ws, ok, err)
	}
}
