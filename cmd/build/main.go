// Command build transforms the raw JSONL layer into the served SQLite
// dataset.
package main

import (
	"flag"
	"log"

	"github.com/NC1107/readarr-metadata-provider/internal/bootstrap"
	"github.com/NC1107/readarr-metadata-provider/internal/dataset"
)

func main() {
	dataDir := flag.String("data", "data", "data directory containing raw/")
	out := flag.String("out", "data/dataset/metadata.db", "output SQLite file")
	flag.Parse()

	b := &dataset.Builder{DataDir: *dataDir, Out: *out}
	if err := b.Build(); err != nil {
		log.Fatal(err)
	}
	// Mark the result as built here, so a server with automatic updates on
	// never replaces it with the published dataset.
	if err := bootstrap.RecordLocal(*out); err != nil {
		log.Fatal(err)
	}
}
