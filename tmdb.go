package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TMDB looks up release dates, runtimes and streaming availability in one region
// (TMDB's watch providers, from JustWatch). Answers are cached on disk and
// refreshed after a week, so a nightly run only asks about new or stale titles.
type TMDB struct {
	token  string
	region string
	path   string
	client *http.Client

	mu     sync.Mutex
	titles map[string]*Title // "movie/62", "tv/1399"
	found  map[string]string // "movie|office space|1999" -> "1542"
}

type Title struct {
	Name     string      `json:"name"`
	Date     string      `json:"date"`     // release date (movies) or first air date (shows)
	Runtime  int         `json:"runtime"`  // minutes per movie or per episode
	Services []string    `json:"services"` // subscription services carrying it in the region
	Sellable bool        `json:"sellable"` // rentable or buyable in the region
	Seasons  map[int]int `json:"seasons,omitempty"`
	Fetched  time.Time   `json:"fetched"`
}

const tmdbFresh = 7 * 24 * time.Hour

func NewTMDB(token, region, path string) *TMDB {
	t := &TMDB{token: token, region: region, path: path, client: &http.Client{Timeout: 20 * time.Second},
		titles: map[string]*Title{}, found: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		var disk struct {
			Region string            `json:"region"`
			Titles map[string]*Title `json:"titles"`
			Found  map[string]string `json:"found"`
		}
		if json.Unmarshal(data, &disk) == nil {
			if disk.Titles != nil && disk.Region == region { // availability is per region
				t.titles = disk.Titles
			}
			if disk.Found != nil {
				t.found = disk.Found
			}
		}
	}
	return t
}

func (t *TMDB) Save() error {
	t.mu.Lock()
	data, err := json.Marshal(map[string]any{"region": t.region, "titles": t.titles, "found": t.found})
	t.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

func (t *TMDB) get(ctx context.Context, path string, q url.Values, v any) error {
	u := "https://api.themoviedb.org/3" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("TMDB %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Search finds a title's TMDB id ("" if nothing matches). Answers are cached for good.
func (t *TMDB) Search(ctx context.Context, kind, title string, year int) (string, error) {
	key := kind + "|" + strings.ToLower(title) + "|" + strconv.Itoa(year)
	t.mu.Lock()
	id, ok := t.found[key]
	t.mu.Unlock()
	if ok {
		return id, nil
	}
	q := url.Values{"query": {title}}
	if year > 0 {
		q.Set(map[string]string{"movie": "year", "tv": "first_air_date_year"}[kind], strconv.Itoa(year))
	}
	var r struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := t.get(ctx, "/search/"+kind, q, &r); err != nil {
		return "", err
	}
	if len(r.Results) > 0 {
		id = strconv.Itoa(r.Results[0].ID)
	}
	t.mu.Lock()
	t.found[key] = id
	t.mu.Unlock()
	return id, nil
}

// Title returns cached details, fetching them when missing or older than a week.
// If TMDB can't be reached, stale details are better than none.
func (t *TMDB) Title(ctx context.Context, kind, id string) (*Title, error) {
	key := kind + "/" + id
	t.mu.Lock()
	cur := t.titles[key]
	t.mu.Unlock()
	if cur != nil && time.Since(cur.Fetched) < tmdbFresh {
		return cur, nil
	}
	var r struct {
		Title        string `json:"title"`
		Name         string `json:"name"`
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
		Runtime      int    `json:"runtime"`
		EpRuntime    []int  `json:"episode_run_time"`
		LastEpisode  *struct {
			Runtime int `json:"runtime"`
		} `json:"last_episode_to_air"`
		Seasons []struct {
			Number   int `json:"season_number"`
			Episodes int `json:"episode_count"`
		} `json:"seasons"`
		Providers struct {
			Results map[string]struct {
				Flatrate []struct {
					Name string `json:"provider_name"`
				} `json:"flatrate"`
				Rent []struct{} `json:"rent"`
				Buy  []struct{} `json:"buy"`
			} `json:"results"`
		} `json:"watch/providers"`
	}
	if err := t.get(ctx, "/"+kind+"/"+id, url.Values{"append_to_response": {"watch/providers"}}, &r); err != nil {
		if cur != nil {
			return cur, nil
		}
		return nil, err
	}
	n := &Title{Name: r.Title + r.Name, Date: r.ReleaseDate + r.FirstAirDate, Runtime: r.Runtime, Fetched: time.Now()}
	if kind == "tv" {
		switch {
		case len(r.EpRuntime) > 0:
			n.Runtime = r.EpRuntime[0]
		case r.LastEpisode != nil:
			n.Runtime = r.LastEpisode.Runtime
		}
		n.Seasons = map[int]int{}
		for _, s := range r.Seasons {
			n.Seasons[s.Number] = s.Episodes
		}
	}
	here := r.Providers.Results[t.region]
	seen := map[string]bool{}
	for _, p := range here.Flatrate {
		if s := serviceOf(p.Name); s != "" && !seen[s] {
			seen[s] = true
			n.Services = append(n.Services, s)
		}
	}
	n.Sellable = len(here.Rent) > 0 || len(here.Buy) > 0
	t.mu.Lock()
	t.titles[key] = n
	t.mu.Unlock()
	return n, nil
}

// serviceOf folds TMDB's provider names into one name per service: "HBO Max Amazon
// Channel" and "Max" are both "HBO Max", "Netflix Standard with Ads" is "Netflix".
// Unknown services keep their own name, so they can be priced too.
func serviceOf(provider string) string {
	n := strings.ToLower(strings.TrimSpace(provider))
	switch n {
	case "apple tv", "apple tv plus", "apple tv+", "apple tv amazon channel":
		return "Apple TV"
	case "max", "max amazon channel":
		return "HBO Max"
	}
	for _, m := range [][2]string{{"netflix", "Netflix"}, {"hbo max", "HBO Max"}, {"disney", "Disney+"},
		{"hulu", "Hulu"}, {"amazon prime video", "Prime Video"}, {"peacock", "Peacock"}, {"paramount", "Paramount+"},
		{"starz", "Starz"}, {"mgm", "MGM+"}, {"crunchyroll", "Crunchyroll"}} {
		if strings.HasPrefix(n, m[0]) {
			return m[1]
		}
	}
	// "AMC+ Amazon Channel", "Shudder Apple TV Channel", "BritBox Roku Premium Channel" -> the service itself
	name := strings.TrimSpace(provider)
	for _, suffix := range []string{" Amazon Channel", " Apple TV Channel", " Apple TV channel", " Roku Premium Channel"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}
