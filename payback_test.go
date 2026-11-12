package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPriceWatches(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC).AddDate(0, 0, d) }
	inf := map[string]*Title{
		"tv/1":     {Name: "Show", Services: []string{"Netflix"}, Sellable: true},
		"tv/2":     {Name: "Indie", Sellable: true},                                                  // on no service: buy
		"movie/10": {Name: "Old", Date: "1999-01-01", Services: []string{"Netflix"}, Sellable: true}, // rent beats a plan
		"movie/11": {Name: "Original", Date: "2025-06-01", Services: []string{"Netflix"}},            // exclusive: plan
		"movie/12": {Name: "New", Date: "2026-01-20", Services: []string{"Starz"}, Sellable: true},   // plan beats premium VOD
		"movie/13": {Name: "Covered", Date: "1990-01-01", Services: []string{"Netflix"}, Sellable: true},
	}
	ep := func(d int, id string, s, e int) watch {
		return watch{When: day(d), Kind: "tv", ID: id, Title: inf["tv/"+id].Name, Season: s, Episode: e}
	}
	mv := func(d int, id string) watch {
		return watch{When: day(d), Kind: "movie", ID: id, Title: inf["movie/"+id].Name}
	}
	ws := []watch{
		mv(0, "10"), mv(0, "10"), // a repeat play counts once
		ep(1, "1", 1, 1), ep(10, "1", 1, 2), mv(12, "13"), // one Netflix period covers both and the movie
		ep(45, "1", 1, 3), // after it ran out: a second period
		mv(46, "11"),      // Netflix is running again: covered
		mv(50, "12"),      // 30 days after release: Starz beats a premium rental
	}
	for e := 1; e <= 10; e++ {
		ws = append(ws, ep(60, "2", 1, e))
	}
	ws = append(ws, ep(61, "2", 1, 1)) // already bought
	set := defaultSettings("US")
	set.Costs = []Cost{{Name: "Server", Amount: 200, Every: "once"}}
	r := priceWatches(ws, inf, set, day(70))

	type row struct {
		Type, Via string
		Cost      float64
		Titles    []string
	}
	var got []row
	for i := len(r.Ledger) - 1; i >= 0; i-- { // oldest first
		c := r.Ledger[i]
		got = append(got, row{c.Type, c.Via, c.Cost, c.Titles})
	}
	want := []row{
		{"rent", "Older", 3.99, []string{"Old"}},
		{"plan", "Netflix", 26.99, []string{"Show | 2 ep", "Covered"}},
		{"plan", "Netflix", 26.99, []string{"Show | 1 ep", "Original"}},
		{"plan", "Starz", 11.99, []string{"New"}},
		{"buy", "Buy", 24.99, []string{"Indie | 10 ep"}}, // ten episodes, capped at the season price
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger:\n got %+v\nwant %+v", got, want)
	}
	if r.Total != 94.95 {
		t.Errorf("total %v, want 94.95", r.Total)
	}
	if len(r.Months) != 3 || r.Months[0].Cost != 30.98 || r.Months[1].Cost != 38.98 || r.Months[2].Cost != 24.99 {
		t.Errorf("months %+v", r.Months)
	}
	if r.Costs != 200 || int(r.PaidPct) != 47 {
		t.Errorf("costs %v paid %v", r.Costs, r.PaidPct)
	}
	// Full months average (30.98+38.98)/2 = 34.98; 105.05 to go → 4 months after March.
	if r.Payback != "2026-07" {
		t.Errorf("payback %q, want 2026-07", r.Payback)
	}
}

func TestCosts(t *testing.T) {
	// History runs Jan–Apr 2026.
	for _, c := range []struct {
		cost Cost
		want float64
	}{
		{Cost{Amount: 500, Every: "once"}, 500},
		{Cost{Amount: 500, Every: "once", From: "2026-06"}, 0}, // not bought yet
		{Cost{Amount: 5, Every: "month"}, 20},                  // from the first month watched
		{Cost{Amount: 5, Every: "month", From: "2025-11", To: "2026-02"}, 20},
		{Cost{Amount: 60, Every: "year"}, 20}, // spread over the months: 4 x 5
	} {
		if got := c.cost.Amount * c.cost.periodsUntil("2026-01", "2026-04"); got != c.want {
			t.Errorf("%+v: %v, want %v", c.cost, got, c.want)
		}
	}
}

func TestUnpriced(t *testing.T) {
	when := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	inf := map[string]*Title{
		"movie/1": {Name: "A", Date: "2000-01-01", Services: []string{"Shudder", "Netflix"}, Sellable: true},
		"movie/2": {Name: "B", Date: "2000-01-01", Services: []string{"Shudder"}, Sellable: true},
		"movie/3": {Name: "C", Date: "2000-01-01", Services: []string{"BritBox"}, Sellable: true},
	}
	ws := []watch{{When: when, Kind: "movie", ID: "1"}, {When: when, Kind: "movie", ID: "2"}, {When: when, Kind: "movie", ID: "3"}}
	r := priceWatches(ws, inf, defaultSettings("US"), when)
	if want := []string{"Shudder", "BritBox"}; !reflect.DeepEqual(r.Unpriced, want) {
		t.Errorf("unpriced %v, want %v", r.Unpriced, want)
	}
}

func TestServiceOf(t *testing.T) {
	for in, want := range map[string]string{
		"Netflix Standard with Ads": "Netflix", "HBO Max Amazon Channel": "HBO Max", "Max": "HBO Max",
		"Peacock Premium Plus": "Peacock", "Paramount+ Roku Premium Channel": "Paramount+", "Apple TV": "Apple TV",
		"Disney Plus": "Disney+", "Amazon Prime Video with Ads": "Prime Video",
		"fuboTV": "fuboTV", "Shudder Amazon Channel": "Shudder", "BritBox Apple TV Channel": "BritBox",
	} {
		if got := serviceOf(in); got != want {
			t.Errorf("serviceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidSettings(t *testing.T) {
	for _, region := range []string{"US", "GB", "ZZ"} {
		if err := validSettings(defaultSettings(region)); err != nil {
			t.Fatalf("%s defaults: %v", region, err)
		}
	}
	bad := map[string]func(*Settings){
		"bad month":       func(s *Settings) { s.Manual = []ManualWatch{{Kind: "movie", Title: "Groundhog Day", Month: "2025-13"}} },
		"negative amount": func(s *Settings) { s.Costs = []Cost{{Name: "NAS", Amount: -1, Every: "once"}} },
		"unnamed cost":    func(s *Settings) { s.Costs = []Cost{{Amount: 1, Every: "once"}} },
		"bad period":      func(s *Settings) { s.Costs = []Cost{{Name: "VPN", Amount: 1, Every: "weekly"}} },
		"ends early": func(s *Settings) {
			s.Costs = []Cost{{Name: "VPN", Amount: 1, Every: "month", From: "2026-05", To: "2026-01"}}
		},
		"currency": func(s *Settings) { s.Currency = "dollars" },
		"account":  func(s *Settings) { s.Account = -2 },
	}
	for name, edit := range bad {
		s := defaultSettings("US")
		edit(&s)
		if validSettings(s) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

type offSource struct{}

func (offSource) ready() error { return errors.New("off") }
func (offSource) collect(context.Context, Settings) ([]watch, map[string]*Title, error) {
	return nil, nil, nil
}

func TestResetPrices(t *testing.T) {
	p := NewPayback(offSource{}, t.TempDir(), "US", time.UTC)
	ctx := context.Background()
	edit := `{"costs": [{"name": "NAS", "amount": 1234, "every": "once"}], "plans": {"Netflix": 99}, "rentNew": 50, "season": 1}`
	if _, err := p.Update(ctx, strings.NewReader(edit)); err != nil {
		t.Fatal(err)
	}
	if set := p.Settings(); set.Plans["Netflix"] != 99 || set.RentNew != 50 || len(set.Costs) != 1 {
		t.Fatalf("edit not saved: %+v", set)
	}
	if _, err := p.ResetPrices(ctx); err != nil {
		t.Fatal(err)
	}
	set, d := p.Settings(), defaultSettings("US")
	if !reflect.DeepEqual(set.Plans, d.Plans) || set.RentNew != d.RentNew || set.Season != d.Season {
		t.Errorf("prices not reset: %+v", set)
	}
	if len(set.Costs) != 1 || set.Costs[0].Amount != 1234 {
		t.Errorf("reset touched costs: %+v", set.Costs)
	}
}

func TestDemo(t *testing.T) {
	ws, inf, err := demoSource{time.UTC}.collect(context.Background(), Settings{})
	if err != nil || len(ws) == 0 {
		t.Fatalf("demo: %d plays, %v", len(ws), err)
	}
	r := priceWatches(ws, inf, defaultSettings("US"), time.Now())
	kinds := map[string]bool{}
	for _, c := range r.Ledger {
		kinds[c.Type] = true
	}
	if r.Total <= 0 || !kinds["plan"] || !kinds["rent"] || !kinds["buy"] {
		t.Errorf("demo should show plans, rentals and buys: total %v, %v", r.Total, kinds)
	}
}

func TestSecure(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := secure(ok, "hunter2")
	do := func(method, path, pw string, csrf bool) int {
		req := httptest.NewRequest(method, path, nil)
		if pw != "" {
			req.SetBasicAuth("me", pw)
		}
		if csrf {
			req.Header.Set("X-Requested-With", "payback")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cases := []struct {
		method, path, pw string
		csrf             bool
		want             int
	}{
		{"GET", "/healthz", "", false, 200},
		{"GET", "/", "", false, 401},
		{"GET", "/", "wrong", false, 401},
		{"GET", "/", "hunter2", false, 200},
		{"PUT", "/api/settings", "hunter2", false, 403}, // no custom header: could be another site
		{"PUT", "/api/settings", "hunter2", true, 200},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.pw, c.csrf); got != c.want {
			t.Errorf("%s %s pw=%q csrf=%v: %d, want %d", c.method, c.path, c.pw, c.csrf, got, c.want)
		}
	}
}
