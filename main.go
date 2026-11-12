// Payback: what everything you've watched on Plex would have cost on streaming
// services, and how far that has paid back your server.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // months are counted in local time; slim containers have no zoneinfo
)

//go:embed web
var webFS embed.FS

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" { // Docker HEALTHCHECK: the image has no curl
		addr := env("LISTEN", ":8095")
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	dir := env("DATA_DIR", "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	loc, err := time.LoadLocation(env("TZ", "Local"))
	if err != nil {
		log.Printf("TZ: %v; using UTC", err)
		loc = time.UTC
	}
	region := strings.ToUpper(env("REGION", "US"))

	var src source = demoSource{loc}
	if os.Getenv("DEMO") == "" {
		src = plexSource{
			plex: NewPlex(os.Getenv("PLEX_URL"), os.Getenv("PLEX_TOKEN"), os.Getenv("PLEX_PREFS")),
			tmdb: NewTMDB(os.Getenv("TMDB_TOKEN"), region, dir+"/tmdb.json"),
			loc:  loc,
		}
	}
	p := NewPayback(src, dir, region, loc)
	if _, demo := src.(demoSource); demo {
		p.seedDemo()
	}

	ctx, stop := context.WithCancel(context.Background())
	go p.Loop(ctx)

	srv := &http.Server{Addr: env("LISTEN", ":8095"), Handler: routes(p, ctx, os.Getenv("APP_PASSWORD")),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 3 * time.Minute}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		stop()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	_, demo := src.(demoSource)
	log.Printf("payback %s listening on %s (region %s, demo %v, password %v)", version, srv.Addr, region, demo, os.Getenv("APP_PASSWORD") != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func routes(p *Payback, ctx context.Context, password string) http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/result", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, p.Result()) })
	edit := func(f func(context.Context) (*Result, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			res, err := f(rctx)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, res)
		}
	}
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		body := http.MaxBytesReader(w, r.Body, 256<<10)
		edit(func(c context.Context) (*Result, error) { return p.Update(c, body) })(w, r)
	})
	mux.HandleFunc("POST /api/prices/reset", edit(p.ResetPrices))
	// A full refresh re-reads Plex history and can take minutes the first time: it
	// runs in the background and the page polls for the result.
	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			rctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
			defer cancel()
			if err := p.Refresh(rctx, true); err != nil {
				log.Printf("refresh: %v", err)
			}
		}()
		writeJSON(w, map[string]bool{"started": true})
	})
	return secure(mux, password)
}

// secure adds security headers, an optional password (HTTP basic auth, any user
// name), and requires a custom header on changes so other sites can't make them.
func secure(next http.Handler, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if password != "" {
			_, pw, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(password)) != 1 {
				h.Set("WWW-Authenticate", `Basic realm="Payback", charset="UTF-8"`)
				writeErr(w, http.StatusUnauthorized, "password required")
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "payback" {
			writeErr(w, http.StatusForbidden, "missing X-Requested-With: payback")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
