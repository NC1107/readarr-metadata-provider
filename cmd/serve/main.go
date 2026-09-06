// Command serve exposes the Readarr/Bookshelf metadata API over the
// prebuilt SQLite dataset. Configuration comes from flags, with defaults
// from the environment or a .env file in the working directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NC1107/readarr-metadata-provider/internal/bootstrap"
	"github.com/NC1107/readarr-metadata-provider/internal/envfile"
	"github.com/NC1107/readarr-metadata-provider/internal/server"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// defaultDatasetURL is the published snapshot a fresh install downloads
// when it has no dataset of its own.
const defaultDatasetURL = "https://github.com/NC1107/readarr-metadata-provider/releases/latest/download/metadata.db.zst"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	envfile.Load(".env")

	// Every flag can be set from the environment as RMP_<FLAG>. The older
	// unprefixed names (DATASET_URL, SEARCH_LANGUAGES, ...) still work so an
	// existing compose file keeps running. A value that does not parse is
	// an error rather than a silent fallback to the default: an operator who
	// wrote RMP_DATASET_REFRESH=1d wants refresh on, and finding out months
	// later that it silently ran hourly is worse than a refused start.
	env := &envConfig{}
	dbPath := flag.String("db", env.str("data/dataset/metadata.db", "RMP_DB", "DB"), "path to metadata.db")
	addr := flag.String("addr", env.str(":8816", "RMP_ADDR", "ADDR"), "listen address")
	maxWorks := flag.Int("max-works", env.integer(2000, "RMP_MAX_WORKS", "MAX_WORKS"), "maximum works returned per author or series")
	web := flag.Bool("web", env.boolean(false, "RMP_WEB", "WEB"), "mount the side-by-side comparison console at /ui (unauthenticated; keep it off a shared network)")
	officialBase := flag.String("official", env.str("", "RMP_OFFICIAL", "OFFICIAL"), "reference metadata service for the /ui comparison console")
	searchLangs := flag.String("search-languages", env.str("en", "RMP_SEARCH_LANGUAGES", "SEARCH_LANGUAGES"), "comma-separated edition language codes search results may have; empty disables the filter (non-Latin queries always bypass it)")
	minRatings := flag.Int64("min-ratings", env.int64(5, "RMP_SEARCH_MIN_RATINGS", "SEARCH_MIN_RATINGS"), "drop search results with fewer ratings than this unless nothing else matches")
	datasetURL := flag.String("dataset-url", env.str(defaultDatasetURL, "RMP_DATASET_URL", "DATASET_URL"), "dataset to download when -db does not exist; empty disables downloading")
	refresh := flag.Duration("dataset-refresh", env.dur(6*time.Hour, "RMP_DATASET_REFRESH", "DATASET_REFRESH"), "check dataset-url this often and install a newer dataset when one is published; 0 disables")
	flag.Parse()
	if err := env.err(); err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(*datasetURL), "http://") {
		log.Printf("warning: dataset-url is plain http; the checksum guards against a truncated download, not a tampered one")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := bootstrap.Options{Validate: server.Validate}
	if err := bootstrap.Ensure(ctx, *dbPath, *datasetURL, opts); err != nil {
		if ctx.Err() != nil {
			log.Print("stopped during the dataset download")
			return nil
		}
		return err
	}

	s, err := server.New(server.Config{
		DBPath:       *dbPath,
		Version:      version,
		MaxWorks:     *maxWorks,
		EnableWebUI:  *web,
		OfficialBase: *officialBase,
		HCToken:      os.Getenv("HARDCOVER_TOKEN"),
		SearchLangs:  *searchLangs,
		MinRatings:   *minRatings,
	})
	if err != nil {
		return err
	}
	defer s.Close()

	srv := &http.Server{
		Addr:    *addr,
		Handler: s.Handler(),
		// Author payloads reach several megabytes, so writes get room;
		// everything else is bounded so a stalled client cannot pin a
		// connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// One check at startup, then on a timer. Each check is a small manifest
	// request; only a changed checksum costs a download.
	if *refresh > 0 && *datasetURL != "" {
		go watchDataset(ctx, s, *dbPath, *datasetURL, *refresh, opts)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("serving %s on %s (version %s)", *dbPath, *addr, version)
		if *web {
			log.Printf("comparison console at http://%s/ui", consoleHost(*addr))
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Shutdown returns only once in-flight requests have finished (or
		// the deadline passes), and main waits for it. Returning as soon as
		// ListenAndServe unblocked would drop every request still writing.
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// watchDataset installs newer datasets as they are published, swapping them
// in without dropping a request. A failed check is logged and retried on the
// next tick rather than taken as fatal: serving slightly old data beats not
// serving.
func watchDataset(ctx context.Context, s *server.Server, dbPath, url string, every time.Duration, opts bootstrap.Options) {
	check := func() {
		c, cancel := context.WithTimeout(ctx, 6*time.Hour)
		defer cancel()
		updated, err := bootstrap.Update(c, dbPath, url, opts)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("dataset check failed: %v", err)
			}
			return
		}
		if !updated {
			return
		}
		if err := s.Swap(dbPath); err != nil {
			log.Printf("installed a new dataset but could not open it, keeping the current one: %v", err)
			return
		}
		log.Print("now serving the updated dataset")
	}

	check()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// envConfig reads flag defaults from the environment, trying each name in
// order so the RMP_ prefix wins over a legacy unprefixed name, and collects
// parse errors so a misspelt value refuses the start with a message naming
// the variable instead of silently running with the default.
type envConfig struct {
	errs []error
}

func (e *envConfig) err() error { return errors.Join(e.errs...) }

func (e *envConfig) lookup(keys ...string) (string, string, bool) {
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return k, v, true
		}
	}
	return "", "", false
}

func (e *envConfig) str(fallback string, keys ...string) string {
	if _, v, ok := e.lookup(keys...); ok {
		return v
	}
	return fallback
}

func (e *envConfig) dur(fallback time.Duration, keys ...string) time.Duration {
	k, v, ok := e.lookup(keys...)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not a duration (use forms like 6h, 72h, 30m)", k, v))
		return fallback
	}
	return d
}

func (e *envConfig) boolean(fallback bool, keys ...string) bool {
	k, v, ok := e.lookup(keys...)
	if !ok {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not a boolean (use true or false)", k, v))
		return fallback
	}
	return b
}

func (e *envConfig) integer(fallback int, keys ...string) int {
	k, v, ok := e.lookup(keys...)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not an integer", k, v))
		return fallback
	}
	return n
}

func (e *envConfig) int64(fallback int64, keys ...string) int64 {
	k, v, ok := e.lookup(keys...)
	if !ok {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s=%q is not an integer", k, v))
		return fallback
	}
	return n
}

func consoleHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
