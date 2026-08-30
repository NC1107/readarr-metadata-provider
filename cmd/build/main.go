// Command build transforms the raw JSONL layer into the served SQLite
// dataset.
package main

import (
	"flag"
	"log"

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
}
