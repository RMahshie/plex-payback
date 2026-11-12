package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Plex reads watch history and title matches from Plex Media Server. The token comes
// from PLEX_TOKEN, or is read from Plex's Preferences.xml on each call (handy when
// running next to Plex: nothing to copy, and a new token is picked up by itself).
type Plex struct {
	baseURL string
	token   string
	prefs   string
	client  *http.Client
}

var reToken = regexp.MustCompile(`PlexOnlineToken="([^"]+)"`)

func NewPlex(url, token, prefs string) *Plex {
	return &Plex{baseURL: strings.TrimSuffix(url, "/"), token: token, prefs: prefs, client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *Plex) enabled() bool { return p.baseURL != "" && (p.token != "" || p.prefs != "") }

func (p *Plex) tokenNow() (string, error) {
	if p.token != "" {
		return p.token, nil
	}
	data, err := os.ReadFile(p.prefs)
	if err != nil {
		return "", err
	}
	m := reToken.FindSubmatch(data)
	if m == nil {
		return "", errors.New("no PlexOnlineToken in Preferences.xml (is the server signed in?)")
	}
	return string(m[1]), nil
}

// fetch GETs a Plex API path and decodes the XML answer into v.
func (p *Plex) fetch(path string, v any) error {
	token, err := p.tokenNow()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", p.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/xml")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("plex %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	return xml.NewDecoder(resp.Body).Decode(v)
}
