// Command serve exposes the Readarr/Bookshelf metadata API over the
// prebuilt SQLite dataset. Configuration comes from flags, with defaults
// from the environment or a .env file in the working directory.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NC1107/readarr-metadata-provider/internal/bootstrap"
	"github.com/NC1107/readarr-metadata-provider/internal/envfile"
	"github.com/NC1107/readarr-metadata-provider/internal/server"
)

// defaultDatasetURL is the published snapshot a fresh install downloads
// when it has no dataset of its own.
const defaultDatasetURL = "https://github.com/NC1107/readarr-metadata-provider/releases/latest/download/metadata.db.zst"

func main() {
	envfile.Load(".env")

	dbPath := flag.String("db", "data/dataset/metadata.db", "path to metadata.db")
	addr := flag.String("addr", ":8816", "listen address")
	maxWorks := flag.Int("max-works", 2000, "maximum works returned per author or series")
	officialBase := flag.String("official", "", "reference metadata service for the /ui comparison console")
	searchLangs := flag.String("search-languages", envOr("SEARCH_LANGUAGES", "en"), "comma-separated edition language codes search results may have; empty disables the filter (non-Latin queries always bypass it)")
	minRatings := flag.Int64("min-ratings", envInt("SEARCH_MIN_RATINGS", 5), "drop search results with fewer ratings than this unless nothing else matches")
	datasetURL := flag.String("dataset-url", envOr("DATASET_URL", defaultDatasetURL), "dataset to download when -db does not exist; empty disables downloading")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.Ensure(ctx, *dbPath, *datasetURL); err != nil {
		log.Fatal(err)
	}

	s, err := server.New(server.Config{
		DBPath:       *dbPath,
		MaxWorks:     *maxWorks,
		OfficialBase: *officialBase,
		HCToken:      os.Getenv("HARDCOVER_TOKEN"),
		SearchLangs:  *searchLangs,
		MinRatings:   *minRatings,
	})
	if err != nil {
		log.Fatal(err)
	}
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

	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("serving %s on %s", *dbPath, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
