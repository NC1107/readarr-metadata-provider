# readarr-metadata-provider

Self-hosted, offline-capable metadata provider for Bookshelf/Readarr, backed by a prebuilt dataset from [Hardcover](https://hardcover.app).
Same recipe as [lidarr-metadata-provider](https://github.com/NC1107/lidarr-metadata-provider): a single Go binary serving precomputed responses from read-only SQLite, so end users never query the upstream API.

Status: Phase 3 (server + comparison console). See PLAN.md.

## Seeder

```sh
cp .env.example .env   # add your Hardcover API token
go build -o bin/seed ./cmd/seed
./bin/seed -mode seed -entity all     # full seed, resumable, ~1,500 API requests
./bin/seed -mode delta -entity books  # changed rows since last sync
```

Raw rows land in `data/raw/<entity>/` as gzipped JSONL, exactly as returned by the API.
Progress is checkpointed in `data/state/`; the seeder stops cleanly if the daily API budget runs low and resumes where it left off.

## Dataset builder

```sh
go build -o bin/build ./cmd/build
./bin/build   # data/raw -> data/dataset/metadata.db (~6GB, ~2.5 min)
```

Produces normalized tables (works, authors, series, editions, link tables) keyed by Hardcover ids, plus an FTS5 search index over titles, author names, and series names.

## Server

```sh
go build -o bin/serve ./cmd/serve
./bin/serve -db data/dataset/metadata.db -addr :8816
```

Serves the Readarr/Bookshelf metadata contract (same shapes and redirect flows as rreading-glasses' Hardcover flavor, Hardcover ids as ForeignIds): `/search`, `/author/{id}`, `/work/{id}`, `/book/{id}`, `/book/isbn/{isbn}`, `/book/asin/{asin}`, `/book/bulk`, `/series/{id}`, `/author/changed`, `/recommended`.

Point Readarr or Bookshelf at it without a restart (the key is under Settings > General > Security):

```sh
./switch.sh --readarr http://localhost:8787 --api-key <key> --to http://localhost:8816/
```

## Comparing it against the public service

Open http://localhost:8816/ui and type a query; it runs against this server and the public rreading-glasses Hardcover instance at once, side by side, and flags any response keys the client would not recognize.

`cmd/parity` does the same from the command line: search-ranking agreement over `fixtures/search-queries.txt`, or `-deep` for field-by-field comparison of known works.
Both are deliberately slow toward the public instance; it is community-funded, be considerate.

Book and author metadata provided by Hardcover.
