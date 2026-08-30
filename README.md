# readarr-metadata-provider

A self-hosted metadata server for readarr and its forks, built on hardcover data.
Same idea as [lidarr-metadata-provider](https://github.com/NC1107/lidarr-metadata-provider): the dataset gets built ahead of time, you run one binary that serves it from sqlite, and after that nothing depends on anyone else's server staying up.

Readarr was retired in 2025 pretty much because its metadata service died, so the failure mode this protects against isn't hypothetical, it's the thing that already happened.
The community replacement most people use now is [rreading-glasses](https://github.com/blampe/rreading-glasses), which works well but is a live cache, meaning it still needs its upstream sources answering.
This is the offline version of that: the whole dataset (about 2.8 million books, 1.2 million authors, 200k series, with covers, descriptions and ratings from hardcover) sits in one sqlite file and the server answers everything from it.

It speaks the same API as rreading-glasses' hardcover flavor, same shapes, same redirect flows, same hardcover ids, so bookshelf and readarr treat it as a drop-in.
Responses are checked field by field against the live service so imports behave the same.

Status: working end to end, dataset published, no docker image pushed yet so compose builds it locally.
Book data comes from [hardcover](https://hardcover.app), who are good people, be considerate with their API.

## Quick start

```
git clone https://github.com/NC1107/readarr-metadata-provider
cd readarr-metadata-provider
docker compose up -d
```

On first boot it downloads the dataset (1.1GB compressed, 6.6GB on disk), checks it against its published checksum, and serves it.
No hardcover key needed, no seeding, no import step.
After that it works offline.

## What it needs

Measured on the real dataset, not estimated:

| | |
|---|---|
| Disk | 7GB (the 6.6GB dataset, streamed straight to its final file, nothing else kept) |
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
Run it again with `--revert` to go back.

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

## Running the server

```sh
go build -o bin/serve ./cmd/serve
./bin/serve -db data/dataset/metadata.db -addr :8816
```

Same as the container: if that file doesn't exist it downloads the published dataset first.
Pass `-dataset-url ""` to turn that off, or point it at a specific snapshot.

## Comparing it against the public service

There's a side-by-side console at http://localhost:8816/ui if you want to see how the data stacks up.
You type a query and it runs against this server and the public rreading-glasses instance at once, shows latency, payload size, result rankings, and flags any response keys readarr wouldn't recognize.

`cmd/parity` does the same from the command line: search ranking agreement over a set of queries, or `-deep` for field-by-field comparison of known works.
Both are deliberately slow toward the public instance, it's community-funded, don't hammer it.

Search ranking sits at about 92% top-1 agreement with hardcover's own search, and most of the remaining disagreements are cases where I think our answer is better, like their popularity-first sort returning lord of the flies when you search stephen king because he wrote an introduction for it once.

## How it works

The server is a single go binary with sqlite opened read only, search runs on FTS5 with popularity blended into the ranking.
Author pages, the thing that famously choked readarr's original service, assemble in single-digit milliseconds for normal authors and about half a second for the worst author in the entire dataset.
Editions are capped at the 20 most-shelved per book, same as rreading-glasses does, which is what keeps the dataset a sane size.

Planned next: published dataset snapshots on github releases so nobody has to seed their own, a docker image, and delta sync automation.
See PLAN.md for the longer version.
