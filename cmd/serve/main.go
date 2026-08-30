// Command serve exposes the Readarr/Bookshelf metadata API over the
// prebuilt SQLite dataset.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
	"github.com/NC1107/readarr-metadata-provider/internal/server"
)

func main() {
	dbPath := flag.String("db", "data/dataset/metadata.db", "path to metadata.db")
	addr := flag.String("addr", ":8816", "listen address")
	maxWorks := flag.Int("max-works", 2000, "maximum works returned per author or series")
	officialBase := flag.String("official", "", "reference metadata service for the /ui comparison console")
	flag.Parse()

	s, err := server.New(*dbPath, *maxWorks, *officialBase, hcapi.TokenFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s on %s", *dbPath, *addr)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
