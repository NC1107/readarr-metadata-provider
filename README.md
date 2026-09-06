# readarr-metadata-provider

A self-hosted metadata server for readarr and its forks, built on hardcover data.
Same idea as [lidarr-metadata-provider](https://github.com/NC1107/lidarr-metadata-provider): the dataset gets built ahead of time, you run one binary that serves it from sqlite, and after that nothing depends on anyone else's server staying up.

Readarr was retired in 2025 pretty much because its metadata service died, so the failure mode this protects against isn't hypothetical, it's the thing that already happened.
The community replacement most people use now is [rreading-glasses](https://github.com/blampe/rreading-glasses), which works well but is a live cache, meaning it still needs its upstream sources answering.
This is the offline version of that: the whole dataset (about 2.8 million books, 1.2 million authors, 200k series, with covers, descriptions and ratings from hardcover) sits in one sqlite file and the server answers everything from it.

It speaks the same API as rreading-glasses' hardcover flavor, same shapes, same redirect flows, same hardcover ids, so bookshelf and readarr treat it as a drop-in.
Responses are checked field by field against the live service so imports behave the same.

Status: working end to end. Dataset and container image are both published, so compose just pulls.
Book data comes from [hardcover](https://hardcover.app), who are good people, be considerate with their API.

## Quick start

```
git clone https://github.com/NC1107/readarr-metadata-provider
cd readarr-metadata-provider
docker compose up -d
```

On first boot it downloads the dataset (1.1GB compressed, 6.6GB on disk), checks it against its published checksum, test-opens it, and serves it.
No hardcover key needed, no seeding, no import step.

After that it keeps itself current. It checks for a newer dataset when it starts and every 6 hours after, and only downloads when the published checksum differs from what it already has, so a check that finds nothing costs one small request.
A new dataset is verified and test-opened before it replaces the old one, and swapped in without dropping a request, so a failed or corrupt download leaves the working dataset alone.
New datasets are published weekly.
Set `RMP_DATASET_REFRESH=0` to pin the one you have; either way it serves offline once it has a dataset.
A dataset you built yourself, or copied into place by hand, is never replaced: the updater only touches files it installed.

## What it needs

Measured on the real dataset, not estimated:

| | |
|---|---|
| Disk | 7GB to run (the 6.6GB dataset, streamed straight to its final file). A refresh downloads the new dataset beside the old one, which keeps serving until the new one is verified, so leave room for two (about 14GB) if you keep updates on |
| RAM, idle | ~10MB |
| RAM, normal searches | ~15MB |
| RAM, after a misspelled search | ~130MB (loads the author-name table once, for typo correction) |
| RAM, ten concurrent worst-case author pages | ~200MB |
| RAM, during first-boot download | ~190MB (decompression buffers) |

So 256MB is comfortable and 512MB has room to spare.
It's a single go binary reading sqlite, there's no database server, no cache layer and no background workers.
CPU is idle except while answering, and the first-boot download takes about 20 seconds on a fast connection.

Building your own dataset needs more: about 2.5GB for the raw layer plus the 6.6GB output, and the build itself peaks around 1GB of RAM.

Then point readarr or bookshelf at it.
The metadata source has no field in the UI, so `switch.sh` sets it through the REST API, live, no restart:

```
./switch.sh --readarr http://localhost:8787 --api-key <key> --to http://localhost:8816/
```

Your API key is under Settings > General > Security.
Run it again with `--revert` to clear the setting. Readarr's original service no longer exists, so that only helps on a fork that ships its own default.

## Building the dataset yourself

You don't need to, the releases are there.
This is if you want your own, or newer data than the last snapshot.
Here's what it costs: a full seed is about 11,600 API queries (two sweeps of the ~2.9M book id space at 500 rows per query).
Hardcover's free tier allows 5,000 queries a day, so that's three days on a free account, or a single day as a supporter (50,000/day).
The seeder checkpoints as it goes and stops cleanly when the daily budget runs low, so you just rerun it the next day and it resumes.
Daily delta syncs after that are a few hundred queries.

You need a hardcover API token (free account, Settings > Hardcover API).

```sh
cp .env.example .env   # put your token in it
go build -o bin/seed ./cmd/seed
./bin/seed -mode seed -entity all     # full seed, resumable
./bin/seed -mode delta -entity books  # changed rows since last sync
```

Raw rows land in `data/raw/` as gzipped JSONL exactly as the API returned them, so schema changes later never force a refetch.
Then the builder turns that into the served database, takes a couple minutes:

```sh
go build -o bin/build ./cmd/build
./bin/build   # data/raw -> data/dataset/metadata.db, about 6GB
```

The builder marks its output as locally built, so a server with automatic updates on serves it and leaves it alone rather than replacing it with the published snapshot.

## Running the server

```sh
go build -o bin/serve ./cmd/serve
./bin/serve -db data/dataset/metadata.db -addr :8816
```

Same as the container: if that file doesn't exist it downloads the published dataset first, then checks for a newer one every 6 hours (`-dataset-refresh`, `0` disables).
Pass `-dataset-url ""` to stop it downloading at all, or point it at a specific snapshot to pin that one.

### Flags

Every flag can also be set from the environment as `RMP_` plus the flag name in upper case with dashes as underscores (`RMP_DATASET_URL`, `RMP_WEB`, ...), or in a `.env` file in the working directory (`/data` in the container). A flag on the command line wins. The older unprefixed names (`DATASET_URL`, `DATASET_REFRESH`, `SEARCH_LANGUAGES`, `SEARCH_MIN_RATINGS`) still work. A value that doesn't parse refuses to start rather than silently running with the default.

- `-db` (default `data/dataset/metadata.db`, `/data/metadata.db` in the container) - the dataset file to serve.
- `-addr` (default `:8816`) - the address it listens on.
- `-dataset-url` - where to download the dataset from when `-db` doesn't exist. Defaults to the latest github release; empty disables downloading. A `manifest.txt` with sha256 lines must be published beside it, since a download that can't be verified isn't installed. Use https; plain http gets a warning.
- `-dataset-refresh` (default `6h`) - how often to check `dataset-url` for a newer dataset and swap it in, live. `0` disables. A refresh needs room for a second copy of the dataset while it downloads.
- `-max-works` (default `2000`) - the most works one author or series page returns. Bookshelf handles the full count; the cap keeps a pathological author from producing a hundred-megabyte page.
- `-search-languages` (default `en`) - comma-separated edition language codes search results may have. Empty disables the filter. A query in non-Latin script bypasses it.
- `-min-ratings` (default `5`) - drop search results with fewer ratings than this, unless that would leave nothing.
- `-web` - turns on the `/ui` comparison console (below). Off by default: it is unauthenticated and makes the server query the public rreading-glasses instance, and hardcover if a token is set, on a visitor's behalf, so keep it off on a port other people can reach.
- `-official` - the reference service the console compares against. Defaults to the public rreading-glasses hardcover instance.
- `HARDCOVER_TOKEN` (environment only) - adds a live hardcover column to the console. Serving never needs it.

`GET /healthz` runs a real lookup against the served dataset and answers 200 or 503, which is what the container's healthcheck and any orchestrator probe should use. `GET /` reports the version and dataset counts.

## Comparing it against the public service

There's a side-by-side console at http://localhost:8816/ui, when the server runs with `-web`, if you want to see how the data stacks up.
You type a query and it runs against this server and the public rreading-glasses instance at once, shows latency, payload size, result rankings, and flags any response keys readarr wouldn't recognize.

`cmd/parity` does the same from the command line: search ranking agreement over a set of queries, or `-deep` for field-by-field comparison of known works.
Both are deliberately slow toward the public instance, it's community-funded, don't hammer it.

Search ranking sits at about 92% top-1 agreement with hardcover's own search, and most of the remaining disagreements are cases where I think our answer is better, like their popularity-first sort returning lord of the flies when you search stephen king because he wrote an introduction for it once.

## How it works

The server is a single go binary with sqlite opened read only, search runs on FTS5 with popularity blended into the ranking.
Author pages, the thing that famously choked readarr's original service, assemble in single-digit milliseconds for normal authors and about half a second for the worst author in the entire dataset.
Editions are capped at the 20 most-shelved per book, same as rreading-glasses does, which is what keeps the dataset a sane size.

A dataset update downloads beside the live file, is checked against its published checksum and opened once to prove it serves, and only then replaces the file. The database that was serving keeps the file handles it already had, so requests in flight finish on the dataset they started on, and the new one takes over for everything after.

Datasets are rebuilt weekly on github actions from a delta sync against hardcover, smoke-tested, and attached to a release. PLAN.md has the design notes and what changed along the way.

## License and data

GPL-3.0, see LICENSE. The API shapes follow [rreading-glasses](https://github.com/blampe/rreading-glasses) (MIT) as a behavioural reference; none of its code is reused.

Book data comes from [hardcover](https://hardcover.app) via their API. Hardcover asserts no proprietary rights over the database and asks for attribution on aggregate data, so: the dataset carries only catalogue data and aggregate ratings, no user-owned content (reviews, lists, shelves), and cover images are linked from hardcover's CDN rather than redistributed. If you publish anything built on this dataset, credit hardcover. Takedown requests for catalogue data should go to hardcover; for anything specific to this project, open an issue.
