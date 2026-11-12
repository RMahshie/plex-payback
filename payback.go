package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Payback works out what everything watched on Plex would have cost on streaming
// services. Every play (plus watches from before Plex, entered by hand) is priced the
// way it would most cheaply have been watched:
//   - Shows: the first episode starts a 30-day subscription to the cheapest service
//     carrying the show; it renews only when something on it is played after it ends.
//   - Movies: free on a subscription that's already running; otherwise the cheaper
//     of a rental or starting a service that has it (exclusives can't be rented).
//   - Shows no service carries: episodes are bought, capped at the season price.
//   - Each movie counts once; repeat plays are mostly glitches.
//
// Charges count in the month they start. A nightly run picks up new plays.
type Payback struct {
	src    source
	dir    string
	region string
	loc    *time.Location

	run sync.Mutex // one refresh or update at a time
	mu  sync.Mutex
	res *Result
	ws  []watch           // last collected plays, for repricing without refetching
	inf map[string]*Title // "movie/62" -> details
	key string            // settings the plays were collected with (account + manual)
}

// source collects the plays to price and the details of every title in them.
type source interface {
	ready() error // nil when it can collect; otherwise what's missing
	collect(ctx context.Context, set Settings) ([]watch, map[string]*Title, error)
}

type Settings struct {
	Costs      []Cost             `json:"costs"`
	Plans      map[string]float64 `json:"plans"`      // per month, the plan you'd pick
	RentOld    float64            `json:"rentOld"`    // rental, released over 2 years ago
	RentRecent float64            `json:"rentRecent"` // released in the last 2 years
	RentNew    float64            `json:"rentNew"`    // first 120 days (premium VOD)
	Episode    float64            `json:"episode"`
	Season     float64            `json:"season"`
	PricesAsOf string             `json:"pricesAsOf"`
	Currency   string             `json:"currency"`
	Account    int                `json:"account"` // Plex account whose plays count: 1 = server owner, 0 = everyone
	Manual     []ManualWatch      `json:"manual"`
}

// Cost is something spent on watching this way: the server, drives, Plex Pass, a VPN.
type Cost struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
	Every  string  `json:"every"`          // once | month | year
	From   string  `json:"from,omitempty"` // first month paid, "2025-06"; default: the first month watched
	To     string  `json:"to,omitempty"`   // last month paid; default: still paying
}

// ManualWatch is something watched outside Plex history (before Plex existed).
type ManualWatch struct {
	Kind    string `json:"kind"` // movie | tv
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	TMDB    string `json:"tmdb,omitempty"`
	Month   string `json:"month"`             // "2025-12"
	Seasons []int  `json:"seasons,omitempty"` // shows: which seasons (all when empty)
}

// usPlans: each service's top ad-free plan with 4K, checked Nov 2026.
var usPlans = map[string]float64{"Netflix": 26.99, "HBO Max": 22.99, "Disney+": 21.49, "Hulu": 21.49, "Prime Video": 19.98,
	"Peacock": 19.99, "Crunchyroll": 17.99, "Apple TV": 14.99, "Paramount+": 13.99, "Starz": 11.99, "MGM+": 7.99}

var currencies = map[string]string{"US": "USD", "CA": "CAD", "GB": "GBP", "AU": "AUD", "NZ": "NZD", "IE": "EUR", "DE": "EUR",
	"FR": "EUR", "ES": "EUR", "IT": "EUR", "NL": "EUR", "BE": "EUR", "AT": "EUR", "PT": "EUR", "FI": "EUR", "SE": "SEK",
	"NO": "NOK", "DK": "DKK", "CH": "CHF", "PL": "PLN", "JP": "JPY", "KR": "KRW", "IN": "INR", "BR": "BRL", "MX": "MXN"}

// defaultSettings: US prices in the US. Elsewhere plans start empty; the services
// your titles are on show up unpriced, ready to fill in.
func defaultSettings(region string) Settings {
	set := Settings{
		Costs:   []Cost{{Name: "Server and drives", Every: "once"}, {Name: "Plex Pass", Every: "month"}},
		Plans:   map[string]float64{},
		RentOld: 3.99, RentRecent: 5.99, RentNew: 19.99, Episode: 2.99, Season: 24.99,
		Currency: currencies[region], Account: 1,
	}
	if set.Currency == "" {
		set.Currency = "USD"
	}
	if region == "US" {
		set.Plans, set.PricesAsOf = maps.Clone(usPlans), "2026-11"
	}
	return set
}

type Result struct {
	Updated  time.Time `json:"updated"`
	Error    string    `json:"error,omitempty"`
	Total    float64   `json:"total"`
	Costs    float64   `json:"costs"` // spent so far
	PaidPct  float64   `json:"paidPct"`
	Payback  string    `json:"payback,omitempty"` // "2027-08", or "paid"
	Months   []Month   `json:"months"`
	Ledger   []Charge  `json:"ledger"`             // newest first
	Unpriced []string  `json:"unpriced,omitempty"` // services your titles are on that have no price yet
	Settings Settings  `json:"settings"`
	Region   string    `json:"region"`
	Demo     bool      `json:"demo,omitempty"`
}

type Month struct {
	Month   string   `json:"m"`
	Cost    float64  `json:"cost"`
	Subs    []string `json:"subs"`
	Rentals int      `json:"rentals"`
	Buys    int      `json:"buys"`
}

// Charge is one thing that would have been paid for: a 30-day subscription (with
// what was watched on it), a rental, or bought episodes.
type Charge struct {
	Type   string    `json:"type"` // plan | rent | buy
	Via    string    `json:"via"`  // service, rental tier, or "Buy"
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitzero"`
	Cost   float64   `json:"cost"`
	Titles []string  `json:"titles"`
	Manual bool      `json:"manual,omitempty"` // dated by hand: show the month only
}

type watch struct {
	When            time.Time
	Kind            string // movie | tv
	ID              string // TMDB id
	Title           string
	Season, Episode int
	Manual          bool
}

const subPeriod = 30 * 24 * time.Hour

func NewPayback(src source, dir, region string, loc *time.Location) *Payback {
	p := &Payback{src: src, dir: dir, region: region, loc: loc}
	if data, err := os.ReadFile(p.file("result.json")); err == nil {
		var r Result
		if json.Unmarshal(data, &r) == nil {
			p.res = &r
		}
	}
	return p
}

func (p *Payback) file(name string) string { return filepath.Join(p.dir, name) }

// Settings reads settings.json over the defaults, so new fields get sensible values.
func (p *Payback) Settings() Settings {
	set := defaultSettings(p.region)
	if data, err := os.ReadFile(p.file("settings.json")); err == nil {
		if err := json.Unmarshal(data, &set); err != nil {
			log.Printf("settings.json: %v", err)
		}
	}
	return set
}

func (p *Payback) Result() *Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.res == nil {
		return &Result{Error: "Working it out", Settings: p.Settings(), Region: p.region}
	}
	return p.res
}

func (p *Payback) store(r *Result) {
	r.Region = p.region
	_, r.Demo = p.src.(demoSource)
	p.mu.Lock()
	p.res = r
	p.mu.Unlock()
	if data, err := json.Marshal(r); err == nil {
		os.WriteFile(p.file("result.json"), data, 0o600)
	}
}

// Loop recalculates shortly after start (sooner if nothing is saved yet) and then nightly.
func (p *Payback) Loop(ctx context.Context) {
	wait := 30 * time.Second
	if p.res == nil {
		wait = time.Second
	}
	for ; ; wait = 24 * time.Hour {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		if err := p.Refresh(rctx, true); err != nil {
			log.Printf("refresh: %v", err)
		}
		cancel()
	}
}

// Refresh recalculates. With fetch set (or when the plays' inputs changed) it
// re-reads the history and title details; otherwise it only reprices what it has.
func (p *Payback) Refresh(ctx context.Context, fetch bool) error {
	p.run.Lock()
	defer p.run.Unlock()
	set := p.Settings()
	if err := p.src.ready(); err != nil {
		p.store(&Result{Updated: time.Now(), Error: err.Error(), Settings: set})
		return nil
	}
	key := collectKey(set)
	if fetch || p.ws == nil || key != p.key {
		start := time.Now()
		ws, inf, err := p.src.collect(ctx, set)
		if err != nil {
			if p.ws == nil { // nothing to fall back on
				p.store(&Result{Updated: time.Now(), Error: err.Error(), Settings: set})
			}
			return err
		}
		p.ws, p.inf, p.key = ws, inf, key
		log.Printf("%d plays, %d titles (%s)", len(ws), len(inf), time.Since(start).Round(time.Millisecond))
	}
	p.store(priceWatches(p.ws, p.inf, set, time.Now().In(p.loc)))
	return nil
}

func collectKey(set Settings) string {
	b, _ := json.Marshal([]any{set.Account, set.Manual})
	return string(b)
}

// Update merges a partial settings object (only the fields sent change; a list sent
// replaces the old one), saves it and recalculates.
func (p *Payback) Update(ctx context.Context, body io.Reader) (*Result, error) {
	return p.change(ctx, func(set *Settings) error { return json.NewDecoder(body).Decode(set) })
}

// ResetPrices puts the streaming prices back to the built-in ones; costs and
// manual watches stay as they are.
func (p *Payback) ResetPrices(ctx context.Context) (*Result, error) {
	return p.change(ctx, func(set *Settings) error {
		d := defaultSettings(p.region)
		set.Plans, set.RentOld, set.RentRecent, set.RentNew = d.Plans, d.RentOld, d.RentRecent, d.RentNew
		set.Episode, set.Season, set.PricesAsOf = d.Episode, d.Season, d.PricesAsOf
		return nil
	})
}

func (p *Payback) change(ctx context.Context, edit func(*Settings) error) (*Result, error) {
	p.run.Lock()
	set := p.Settings()
	err := edit(&set)
	if err == nil {
		err = validSettings(set)
	}
	if err == nil {
		var data []byte
		data, err = json.MarshalIndent(set, "", "  ")
		if err == nil {
			err = os.WriteFile(p.file("settings.json"), data, 0o600)
		}
	}
	p.run.Unlock()
	if err != nil {
		return nil, err
	}
	if err := p.Refresh(ctx, false); err != nil {
		return nil, err
	}
	return p.Result(), nil
}

var (
	reMonth    = regexp.MustCompile(`^(19|20)\d\d-(0[1-9]|1[0-2])$`)
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
)

func validSettings(set Settings) error {
	money := []float64{set.RentOld, set.RentRecent, set.RentNew, set.Episode, set.Season}
	for _, v := range set.Plans {
		money = append(money, v)
	}
	for _, c := range set.Costs {
		money = append(money, c.Amount)
	}
	for _, v := range money {
		if v < 0 || v > 1e7 || math.IsNaN(v) {
			return errors.New("amounts must be between 0 and 10,000,000")
		}
	}
	for _, c := range set.Costs {
		switch {
		case strings.TrimSpace(c.Name) == "":
			return errors.New("every cost needs a name")
		case c.Every != "once" && c.Every != "month" && c.Every != "year":
			return fmt.Errorf("cost %q: every must be once, month or year", c.Name)
		case c.From != "" && !reMonth.MatchString(c.From), c.To != "" && !reMonth.MatchString(c.To):
			return fmt.Errorf("cost %q: months look like 2026-01", c.Name)
		case c.From != "" && c.To != "" && c.To < c.From:
			return fmt.Errorf("cost %q ends before it starts", c.Name)
		}
	}
	if !reCurrency.MatchString(set.Currency) {
		return errors.New("currency is a 3-letter code like USD")
	}
	if set.Account < 0 {
		return errors.New("account is a Plex account ID (1 = server owner) or 0 for everyone")
	}
	for _, m := range set.Manual {
		if (m.Kind != "movie" && m.Kind != "tv") || strings.TrimSpace(m.Title) == "" && m.TMDB == "" || !reMonth.MatchString(m.Month) {
			return fmt.Errorf("manual watch %q needs kind movie|tv, a title and a month like 2025-12", m.Title)
		}
	}
	return nil
}

// ---- collecting plays from Plex ----

type plexSource struct {
	plex *Plex
	tmdb *TMDB
	loc  *time.Location
}

func (s plexSource) ready() error {
	switch {
	case s.tmdb.token == "":
		return errors.New("TMDB_TOKEN is not set")
	case !s.plex.enabled():
		return errors.New("Plex is not set up: set PLEX_URL and PLEX_TOKEN")
	}
	return nil
}

// collect reads the account's Plex history and the manual watches, resolves each
// to a TMDB title and fetches the details.
func (s plexSource) collect(ctx context.Context, set Settings) ([]watch, map[string]*Title, error) {
	var hist struct {
		Videos []struct {
			Key              string `xml:"key,attr"`
			Type             string `xml:"type,attr"`
			Title            string `xml:"title,attr"`
			GrandparentTitle string `xml:"grandparentTitle,attr"`
			GrandparentKey   string `xml:"grandparentKey,attr"`
			ParentIndex      int    `xml:"parentIndex,attr"`
			Index            int    `xml:"index,attr"`
			ViewedAt         int64  `xml:"viewedAt,attr"`
			AccountID        int    `xml:"accountID,attr"`
			Released         string `xml:"originallyAvailableAt,attr"`
		} `xml:"Video"`
	}
	path := "/status/sessions/history/all?sort=viewedAt:asc"
	if set.Account > 0 {
		path += "&accountID=" + strconv.Itoa(set.Account)
	}
	if err := s.plex.fetch(path, &hist); err != nil {
		return nil, nil, fmt.Errorf("plex history: %w", err)
	}
	mine := func(id int) bool { return set.Account == 0 || id == set.Account }
	// Movies are keyed by their own item, episodes by their show.
	type src struct{ kind, title, released string }
	keys := map[string]src{}
	for _, v := range hist.Videos {
		switch {
		case !mine(v.AccountID):
		case v.Type == "movie":
			keys[lastSeg(v.Key)] = src{"movie", v.Title, v.Released}
		case v.Type == "episode":
			keys[lastSeg(v.GrandparentKey)] = src{"tv", v.GrandparentTitle, ""}
		}
	}
	ids := s.plexTMDB(slices.Sorted(maps.Keys(keys)))
	resolve := func(k string) string { // titles since deleted from Plex: search TMDB by name
		if id, ok := ids[k]; ok {
			return id
		}
		sr := keys[k]
		year := 0
		if len(sr.released) >= 4 {
			year, _ = strconv.Atoi(sr.released[:4])
		}
		id, err := s.tmdb.Search(ctx, sr.kind, sr.title, year)
		if err != nil {
			log.Printf("search: %v", err)
		}
		ids[k] = id
		return id
	}
	var ws []watch
	for _, v := range hist.Videos {
		if !mine(v.AccountID) || (v.Type != "movie" && v.Type != "episode") {
			continue
		}
		w := watch{When: time.Unix(v.ViewedAt, 0).In(s.loc), Kind: "movie", Title: v.Title}
		k := lastSeg(v.Key)
		if v.Type == "episode" {
			w.Kind, w.Title, w.Season, w.Episode, k = "tv", v.GrandparentTitle, v.ParentIndex, v.Index, lastSeg(v.GrandparentKey)
		}
		if w.ID = resolve(k); w.ID != "" {
			ws = append(ws, w)
		}
	}

	inf := map[string]*Title{}
	title := func(kind, id string) *Title {
		k := kind + "/" + id
		if t, ok := inf[k]; ok {
			return t
		}
		t, err := s.tmdb.Title(ctx, kind, id)
		if err != nil {
			log.Printf("%v", err)
		}
		inf[k] = t
		return t
	}
	for _, m := range set.Manual {
		id := m.TMDB
		if id == "" {
			var err error
			if id, err = s.tmdb.Search(ctx, m.Kind, m.Title, m.Year); err != nil {
				return nil, nil, err
			}
		}
		t := title(m.Kind, id)
		if id == "" || t == nil {
			log.Printf("no TMDB match for the manual %s %q", m.Kind, m.Title)
			continue
		}
		ws = append(ws, manualWatches(m, id, t, s.loc)...)
	}
	for _, w := range ws {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		title(w.Kind, w.ID)
	}
	if err := s.tmdb.Save(); err != nil {
		log.Printf("tmdb cache: %v", err)
	}
	return ws, inf, nil
}

// manualWatches dates a hand-entered watch mid-month; a show counts every episode
// of the seasons listed (all seasons when none are).
func manualWatches(m ManualWatch, id string, t *Title, loc *time.Location) []watch {
	y, _ := strconv.Atoi(m.Month[:4])
	mo, _ := strconv.Atoi(m.Month[5:])
	when := time.Date(y, time.Month(mo), 15, 12, 0, 0, 0, loc)
	if m.Kind == "movie" {
		return []watch{{When: when, Kind: "movie", ID: id, Title: t.Name, Manual: true}}
	}
	seasons := m.Seasons
	if len(seasons) == 0 {
		for n := range t.Seasons {
			if n > 0 {
				seasons = append(seasons, n)
			}
		}
		sort.Ints(seasons)
	}
	var ws []watch
	for _, n := range seasons {
		for e := 1; e <= t.Seasons[n]; e++ {
			ws = append(ws, watch{When: when, Kind: "tv", ID: id, Title: t.Name, Season: n, Episode: e, Manual: true})
		}
	}
	return ws
}

// plexTMDB maps Plex item keys to TMDB ids from Plex's own matches.
func (s plexSource) plexTMDB(keys []string) map[string]string {
	ids := map[string]string{}
	for len(keys) > 0 {
		n := min(40, len(keys))
		var meta struct {
			Items []struct {
				RatingKey string `xml:"ratingKey,attr"`
				Guids     []struct {
					ID string `xml:"id,attr"`
				} `xml:"Guid"`
			} `xml:",any"`
		}
		if err := s.plex.fetch("/library/metadata/"+strings.Join(keys[:n], ",")+"?includeGuids=1", &meta); err == nil {
			for _, it := range meta.Items {
				for _, g := range it.Guids {
					if id, ok := strings.CutPrefix(g.ID, "tmdb://"); ok {
						ids[it.RatingKey] = id
					}
				}
			}
		}
		keys = keys[n:]
	}
	return ids
}

func lastSeg(key string) string { return key[strings.LastIndex(key, "/")+1:] }

// ---- pricing ----

// priceWatches applies the pricing rules (see Payback) to plays in time order.
func priceWatches(ws []watch, inf map[string]*Title, set Settings, now time.Time) *Result {
	ws = slices.Clone(ws)
	sort.SliceStable(ws, func(i, k int) bool { return ws[i].When.Before(ws[k].When) })
	type period struct {
		svc        string
		start, end time.Time
		plays      []watch
	}
	var periods []*period
	active := map[string]*period{}
	var charges []Charge
	buys := map[string]int{} // show + month -> index in charges
	seenMovie := map[string]bool{}
	owned := map[string]bool{}    // "id/s/e" bought
	spent := map[string]float64{} // "id/s" spent on a season
	buyEps := map[int]map[string]bool{}
	unpriced := map[string]map[string]bool{} // service -> titles on it

	for _, w := range ws {
		t := inf[w.Kind+"/"+w.ID]
		if t == nil {
			continue
		}
		if t.Name != "" {
			w.Title = t.Name // English names (Plex can hold an anime's Japanese title)
		}
		if w.Kind == "movie" {
			if seenMovie[w.ID] {
				continue
			}
			seenMovie[w.ID] = true
		}
		var svcs []string
		for _, sv := range t.Services {
			if _, ok := set.Plans[sv]; ok {
				svcs = append(svcs, sv)
			} else {
				if unpriced[sv] == nil {
					unpriced[sv] = map[string]bool{}
				}
				unpriced[sv][w.ID] = true
			}
		}
		sort.SliceStable(svcs, func(i, k int) bool { return set.Plans[svcs[i]] < set.Plans[svcs[k]] })
		var running *period
		for _, sv := range svcs {
			if p := active[sv]; p != nil && p.end.After(w.When) {
				running = p
				break
			}
		}
		if running != nil {
			running.plays = append(running.plays, w)
			continue
		}
		cheapest := ""
		if len(svcs) > 0 {
			cheapest = svcs[0]
		}
		if w.Kind == "movie" && (t.Sellable || cheapest == "") {
			if rent, tier := rentPrice(t, w.When, set); cheapest == "" || rent <= set.Plans[cheapest] {
				charges = append(charges, Charge{Type: "rent", Via: tier, Start: w.When, Cost: rent, Titles: []string{w.Title}, Manual: w.Manual})
				continue
			}
		}
		if w.Kind == "tv" && cheapest == "" {
			ek, sk := fmt.Sprintf("%s/%d/%d", w.ID, w.Season, w.Episode), fmt.Sprintf("%s/%d", w.ID, w.Season)
			if owned[ek] {
				continue
			}
			owned[ek] = true
			cost := min(set.Episode, max(0, set.Season-spent[sk]))
			spent[sk] += cost
			bk := w.ID + "|" + w.When.Format("2006-01")
			i, ok := buys[bk]
			if !ok {
				i = len(charges)
				buys[bk] = i
				buyEps[i] = map[string]bool{}
				charges = append(charges, Charge{Type: "buy", Via: "Buy", Start: w.When, Manual: w.Manual})
			}
			buyEps[i][ek] = true
			charges[i].Cost += cost
			charges[i].Titles = []string{fmt.Sprintf("%s | %d ep", w.Title, len(buyEps[i]))}
			continue
		}
		p := &period{svc: cheapest, start: w.When, end: w.When.Add(subPeriod), plays: []watch{w}}
		active[cheapest] = p
		periods = append(periods, p)
	}
	for _, p := range periods {
		charges = append(charges, Charge{Type: "plan", Via: p.svc, Start: p.start, End: p.end, Cost: set.Plans[p.svc],
			Titles: nestTitles(p.plays), Manual: p.plays[0].Manual})
	}
	sort.SliceStable(charges, func(i, k int) bool { return charges[i].Start.After(charges[k].Start) })

	r := &Result{Updated: time.Now(), Ledger: charges, Settings: set, Months: []Month{}}
	for sv := range unpriced {
		r.Unpriced = append(r.Unpriced, sv)
	}
	sort.Slice(r.Unpriced, func(i, k int) bool { // most-watched first
		a, b := r.Unpriced[i], r.Unpriced[k]
		if len(unpriced[a]) != len(unpriced[b]) {
			return len(unpriced[a]) > len(unpriced[b])
		}
		return a < b
	})
	if len(charges) == 0 {
		return r
	}
	// One bar per month from the first charge to now, empty months included.
	first := charges[len(charges)-1].Start
	idx := map[string]int{}
	for m := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, now.Location()); !m.After(now); m = m.AddDate(0, 1, 0) {
		idx[m.Format("2006-01")] = len(r.Months)
		r.Months = append(r.Months, Month{Month: m.Format("2006-01"), Subs: []string{}})
	}
	for i := range charges {
		c := &charges[i]
		c.Cost = cents(c.Cost)
		r.Total += c.Cost
		mi, ok := idx[c.Start.In(now.Location()).Format("2006-01")]
		if !ok {
			continue
		}
		m := &r.Months[mi]
		m.Cost += c.Cost
		switch c.Type {
		case "plan":
			m.Subs = append(m.Subs, c.Via)
		case "rent":
			m.Rentals++
		case "buy":
			m.Buys++
		}
	}
	for i := range r.Months {
		r.Months[i].Cost = cents(r.Months[i].Cost)
		slices.Reverse(r.Months[i].Subs) // charges were newest first
	}
	r.Total = cents(r.Total)

	// Spent so far, and when streaming would have caught up with it.
	nowM := now.Format("2006-01")
	spentSoFar, running := 0.0, 0.0
	for _, c := range set.Costs {
		spentSoFar += c.Amount * c.periodsUntil(r.Months[0].Month, nowM)
		if c.Every != "once" && (c.To == "" || c.To >= nowM) && (c.From == "" || c.From <= nowM) {
			running += c.monthly()
		}
	}
	r.Costs = cents(spentSoFar)
	r.PaidPct = 100
	if r.Costs > 0 {
		r.PaidPct = math.Min(100, r.Total/r.Costs*100)
	}
	switch full := r.Months[:len(r.Months)-1]; {
	case r.Total >= r.Costs:
		r.Payback = "paid"
	case len(full) > 0:
		sum := 0.0
		for _, m := range full {
			sum += m.Cost
		}
		if net := sum/float64(len(full)) - running; net > 0 {
			ahead := int(math.Ceil((r.Costs - r.Total) / net))
			r.Payback = time.Date(now.Year(), now.Month()+time.Month(ahead), 1, 0, 0, 0, 0, now.Location()).Format("2006-01")
		}
	}
	return r
}

// periodsUntil is how many times a cost has been paid by month now: once (if it has
// started), or the months it has run (a yearly cost spread over its months).
// Without a start month it runs from the first month watched.
func (c Cost) periodsUntil(firstWatched, now string) float64 {
	from := c.From
	if from == "" {
		from = firstWatched
	}
	if from > now {
		return 0
	}
	if c.Every == "once" {
		return 1
	}
	to := now
	if c.To != "" && c.To < now {
		to = c.To
	}
	n := float64(monthIndex(to) - monthIndex(from) + 1)
	if c.Every == "year" {
		n /= 12
	}
	return max(0, n)
}

func (c Cost) monthly() float64 {
	if c.Every == "year" {
		return c.Amount / 12
	}
	return c.Amount
}

func monthIndex(m string) int {
	y, _ := strconv.Atoi(m[:4])
	mo, _ := strconv.Atoi(m[5:])
	return y*12 + mo - 1
}

// rentPrice is a movie's rental price when watched, and its tier as shown in the ledger.
func rentPrice(t *Title, at time.Time, set Settings) (float64, string) {
	rel, err := time.Parse("2006-01-02", t.Date)
	switch age := at.Sub(rel); {
	case err != nil || age > 730*24*time.Hour:
		return set.RentOld, "Older"
	case age > 120*24*time.Hour:
		return set.RentRecent, "Last 2 years"
	default:
		return set.RentNew, "New release"
	}
}

// nestTitles lists what was watched during a subscription: "The Sopranos | 8 ep".
func nestTitles(ps []watch) []string {
	var order []string
	eps := map[string]map[[2]int]bool{}
	for _, p := range ps {
		if _, ok := eps[p.Title]; !ok {
			order = append(order, p.Title)
			eps[p.Title] = map[[2]int]bool{}
		}
		if p.Kind == "tv" {
			eps[p.Title][[2]int{p.Season, p.Episode}] = true
		}
	}
	out := make([]string, len(order))
	for i, t := range order {
		out[i] = t
		if n := len(eps[t]); n > 0 {
			out[i] = fmt.Sprintf("%s | %d ep", t, n)
		}
	}
	return out
}

func cents(v float64) float64 { return math.Round(v*100) / 100 }
