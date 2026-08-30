# readarr-metadata-provider

Self-hosted, offline-capable metadata provider for Bookshelf/Readarr, backed by a prebuilt dataset from [Hardcover](https://hardcover.app).
Same recipe as [lidarr-metadata-provider](https://github.com/NC1107/lidarr-metadata-provider): a single Go binary serving precomputed responses from read-only SQLite, so end users never query the upstream API.

Status: Phase 1 (seeder). See PLAN.md.

## Seeder

```sh
cp .env.example .env   # add your Hardcover API token
go build -o bin/seed ./cmd/seed
./bin/seed -mode seed -entity all     # full seed, resumable, ~1,500 API requests
./bin/seed -mode delta -entity books  # changed rows since last sync
```

Raw rows land in `data/raw/<entity>/` as gzipped JSONL, exactly as returned by the API.
Progress is checkpointed in `data/state/`; the seeder stops cleanly if the daily API budget runs low and resumes where it left off.

Book and author metadata provided by Hardcover.
