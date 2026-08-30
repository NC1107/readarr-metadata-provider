// Command serve exposes the Readarr/Bookshelf metadata API over the
// prebuilt SQLite dataset. Configuration comes from flags, with defaults
// from the environment or a .env file in the working directory.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/NC1107/readarr-metadata-provider/internal/envfile"
	"github.com/NC1107/readarr-metadata-provider/internal/server"
)

func main() {
	envfile.Load(".env")

	dbPath := flag.String("db", "data/dataset/metadata.db", "path to metadata.db")
	addr := flag.String("addr", ":8816", "listen address")
	maxWorks := flag.Int("max-works", 2000, "maximum works returned per author or series")
	officialBase := flag.String("official", "", "reference metadata service for the /ui comparison console")
	searchLangs := flag.String("search-languages", envOr("SEARCH_LANGUAGES", "en"), "comma-separated edition language codes search results may have; empty disables the filter (non-Latin queries always bypass it)")
	minRatings := flag.Int64("min-ratings", envInt("SEARCH_MIN_RATINGS", 5), "drop search results with fewer ratings than this unless nothing else matches")
	flag.Parse()

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
	log.Printf("serving %s on %s", *dbPath, *addr)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
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
