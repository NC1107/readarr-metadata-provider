// Command seed downloads Hardcover metadata into the local raw data layer.
//
//	seed -mode seed  -entity all      # full (resumable) seed
//	seed -mode delta -entity books    # changed rows since last sync
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
	"github.com/NC1107/readarr-metadata-provider/internal/seeder"
)

func main() {
	entity := flag.String("entity", "all", "entity to fetch: all, books, authors, series")
	mode := flag.String("mode", "seed", "seed (full, resumable) or delta (changed rows)")
	dataDir := flag.String("data", "data", "data directory")
	reserve := flag.Int64("reserve", 100, "daily API requests to leave unused")
	flag.Parse()

	token := hcapi.TokenFromEnv()
	if token == "" {
		log.Fatal("set HARDCOVER_TOKEN (env or .env file)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &seeder.Seeder{
		Client:  hcapi.NewClient(token, hcapi.WithDailyReserve(*reserve)),
		DataDir: *dataDir,
	}

	var names []string
	if *entity == "all" {
		names = seeder.SeedOrder
	} else {
		if _, ok := seeder.Entities[*entity]; !ok {
			log.Fatalf("unknown entity %q", *entity)
		}
		names = []string{*entity}
	}

	for _, name := range names {
		spec := seeder.Entities[name]
		var err error
		switch *mode {
		case "seed":
			err = s.Seed(ctx, spec)
		case "delta":
			if !spec.HasUpdated {
				log.Printf("%s: no updated_at column, skipping delta", name)
				continue
			}
			err = s.Delta(ctx, spec)
		default:
			log.Fatalf("unknown mode %q", *mode)
		}
		if errors.Is(err, hcapi.ErrDailyExhausted) {
			log.Printf("stopping: %v; progress is checkpointed, rerun tomorrow to resume", err)
			return
		}
		if err != nil {
			log.Fatalf("%s %s: %v", name, *mode, err)
		}
	}
}
