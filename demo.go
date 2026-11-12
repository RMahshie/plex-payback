package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// demoSource is a made-up year of watching (DEMO=1), for trying the page without
// Plex or TMDB and for screenshots. Where things stream is invented too.
type demoSource struct{ loc *time.Location }

// seedDemo gives a fresh demo some costs to pay back.
func (p *Payback) seedDemo() {
	if _, err := os.Stat(p.file("settings.json")); err == nil {
		return
	}
	set := defaultSettings(p.region)
	set.Costs = []Cost{{Name: "Server and drives", Amount: 640, Every: "once"}, {Name: "Plex Pass", Amount: 6.99, Every: "month"}}
	if data, err := json.MarshalIndent(set, "", "  "); err == nil {
		os.WriteFile(p.file("settings.json"), data, 0o600)
	}
}

func (demoSource) ready() error { return nil }

func (d demoSource) collect(_ context.Context, _ Settings) ([]watch, map[string]*Title, error) {
	start := time.Now().In(d.loc).AddDate(0, -13, 0)
	day := func(n int) time.Time { return start.AddDate(0, 0, n).Add(21 * time.Hour) }
	inf := map[string]*Title{}
	var ws []watch

	shows := []struct {
		name, svc, aired  string
		from, eps, perDay int
	}{
		{"Ozark", "Netflix", "2017-07-21", 3, 62, 2},
		{"Ted Lasso", "Apple TV", "2020-08-14", 40, 19, 2},
		{"Abbott Elementary", "Hulu", "2021-12-07", 70, 28, 3},
		{"Veep", "HBO Max", "2012-04-22", 95, 39, 2},
		{"The Mandalorian", "Disney+", "2019-11-12", 150, 24, 2},
		{"Reacher", "Prime Video", "2022-02-04", 185, 32, 2},
		{"Silo", "Apple TV", "2023-05-05", 230, 24, 2},
		{"House of the Dragon", "HBO Max", "2022-08-21", 262, 16, 1},
		{"Only Murders in the Building", "Hulu", "2021-08-31", 300, 10, 1},
		{"Wednesday", "Netflix", "2022-11-23", 330, 34, 2},
		{"Downton Abbey", "", "2010-09-26", 360, 10, 1}, // on no service: bought by the episode
		{"Hacks", "HBO Max", "2021-05-13", 385, 8, 1},
	}
	for i, s := range shows {
		id := fmt.Sprint(9000 + i)
		t := &Title{Name: s.name, Date: s.aired, Runtime: 50, Seasons: map[int]int{}, Sellable: true}
		if s.svc != "" {
			t.Services = []string{s.svc}
		}
		for e := 0; e < s.eps; e++ {
			season, ep := e/10+1, e%10+1
			t.Seasons[season] = max(t.Seasons[season], ep)
			ws = append(ws, watch{When: day(s.from + e/s.perDay), Kind: "tv", ID: id, Title: s.name, Season: season, Episode: ep})
		}
		inf["tv/"+id] = t
	}

	movies := []struct {
		name, released string
		day            int
		svcs           []string
	}{
		{"Back to the Future", "1985-07-03", 10, nil},
		{"Groundhog Day", "1993-02-12", 18, nil},
		{"Get Out", "2017-02-24", 25, []string{"Hulu"}},
		{"Furiosa: A Mad Max Saga", "2024-05-24", 33, []string{"HBO Max"}},
		{"Coco", "2017-11-22", 44, []string{"Disney+"}},
		{"The Truman Show", "1998-06-05", 52, nil},
		{"Nope", "2022-07-22", 61, []string{"Peacock"}},
		{"Aliens", "1986-07-18", 79, []string{"Hulu"}},
		{"Goodfellas", "1990-09-19", 104, nil},
		{"Godzilla Minus One", "2023-11-03", 120, []string{"HBO Max"}},
		{"Jurassic Park", "1993-06-11", 141, nil},
		{"Memento", "2000-10-11", 166, nil},
		{"The Martian", "2015-10-02", 199, []string{"Paramount+"}},
		{"La La Land", "2016-12-09", 214, nil},
		{"Drive", "2011-09-16", 249, nil},
		{"Challengers", "2024-04-26", 271, []string{"Prime Video"}},
		{"Inside Llewyn Davis", "2013-12-06", 288, nil},
		{"Gravity", "2013-10-04", 316, []string{"Paramount+"}},
		{"Ford v Ferrari", "2019-11-15", 352, []string{"Paramount+"}},
		{"The Shining", "1980-05-23", 371, nil},
		{"Wicked", time.Now().AddDate(0, 0, -60).Format("2006-01-02"), 392, nil}, // a new release
	}
	for i, m := range movies {
		id := fmt.Sprint(8000 + i)
		inf["movie/"+id] = &Title{Name: m.name, Date: m.released, Runtime: 120, Services: m.svcs, Sellable: true}
		ws = append(ws, watch{When: day(m.day), Kind: "movie", ID: id, Title: m.name})
	}
	now := time.Now()
	kept := ws[:0]
	for _, w := range ws {
		if w.When.Before(now) {
			kept = append(kept, w)
		}
	}
	return kept, inf, nil
}
