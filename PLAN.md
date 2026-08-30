# readarr-metadata-provider - Plan

A self-hosted, offline-capable metadata provider for Bookshelf/Readarr, built the same way as lidarr-metadata-provider: a prebuilt dataset served by a single Go binary over read-only SQLite.
The dataset is built once from the Hardcover API by us, published as versioned snapshots, and end users never query Hardcover at all.

## Why this works (verified 2026-08-30)

Measured directly against `api.hardcover.app/v1/graphql` with a free-tier token:

- Rate limits (from response headers): 60 requests/min, burst 10, **5,000 requests/day**.
- Row cap: 1,000 rows per top-level query, max 5 aliased top-level queries per request.
- Dataset size: **2,756,861 books**, ~1.55M authors (max id 1,553,450), ~33.2M editions (max id 33,239,422), book id space ~2.9M.
- A 500-book batch with full nesting (contributions, series links, tags, top-20 editions with ISBN/ASIN/publisher/language/format/image) returns ~1.9MB in ~0.4s.
- `updated_at` exists on books and authors, so daily delta syncs are possible with Hasura `where` filters.

Request budget for a full seed:

| Entity | Strategy | Requests |
|---|---|---|
| Books + nested editions/contributions/series links | 5 aliased id-range queries x 500 ids per request | ~1,160 |
| Authors | 5 aliased id-range queries x 1,000 ids per request | ~310 |
| Series | id-range queries | ~50 |
| **Total** | | **~1,500-2,000** |

The entire seed fits inside a single day's 5,000-request budget.
Daily deltas via `updated_at` will be a few hundred requests at most.
This directly answers the fairness concern: one account builds the dataset, everyone else downloads it.

Editions beyond the top 20 per book (by `users_count`) are intentionally skipped.
rreading-glasses caps at 20 editions per book for usability, so this loses nothing in practice and avoids paging 33M edition rows.

## API contract (from rreading-glasses source)

We serve the exact HTTP contract Bookshelf/Readarr already speak, using Hardcover IDs as ForeignIds.
This makes us a drop-in alternative for the `hardcover.bookinfo.pro` flavor: same IDs, so existing hardcover-flavor libraries keep working.

Endpoints:

```
GET /search?q=
GET /author/{foreignAuthorID}
GET /author/changed
GET /work/{foreignID}
GET /book/{foreignEditionID}
GET /book/isbn/{isbn}
GET /book/asin/{asin}
POST /book/bulk
GET /series/{seriesID}
GET /recommended
```

Resource shapes are in `rreading-glasses/internal/resources.go` (workResource, AuthorResource, bookResource, SeriesResource, SearchResource).
Entity mapping: Hardcover book -> Work, Hardcover edition -> Book, Hardcover author -> Author, Hardcover series -> Series.

Backward compatibility with old Goodreads-ID Readarr databases is explicitly out of scope, same as rreading-glasses' Hardcover flavor: fresh installs (or hardcover-flavor migrations) only.

## Phases

### Phase 0: Hardcover relations and licensing hygiene

- Hardcover's policy states they assert no proprietary rights over the database, personal projects may use API data freely, but user-owned data may not be used in public products and aggregate data requires attribution.
- Therefore: exclude all user-owned content (reviews, lists, user shelves), include aggregate ratings with Hardcover attribution, hotlink images from `assets.hardcover.app` with a link back rather than redistributing image files, and add a DMCA contact note to the README.
- Reach out to Hardcover (Discord/email) about publishing periodic dataset snapshots.
  They cooperate with rreading-glasses already, and our model reduces their API load rather than adding to it.
  Not a hard blocker, but do it early.

Status as of 2026-08-30: phases 1-4 are built and deployed; the remaining
work is listed under "Next" at the end.

### Phase 1: Seeder/ETL (Go) - done

- GraphQL puller using id-range partitioning (range width == row cap so no range can overflow), 5 aliased queries per request.
- Adaptive rate limiting driven by the `ratelimit` response headers (token bucket: burst 10, refill 60/min, stop at daily remaining ~100).
- Resumable: checkpoint file records completed id ranges; raw gzipped JSON responses land on disk first so schema/transform changes never force a refetch.
- Delta mode: pull books/authors where `updated_at > last_sync`, plus a periodic sweep for new max ids.

### Phase 2: Dataset builder - done

- Transform raw JSON into SQLite: `authors`, `works`, `editions`, `series`, `series_works`, `work_authors`, plus precomputed JSON response blobs per entity (the lidarr trick: the server mostly serves prebuilt payloads).
- FTS5 index over title, subtitle, author name, series name for `/search`.
- Output: zstd-compressed, split into <2GB chunks for GitHub release limits, with checksums and a version manifest.

### Phase 3: Server (Go) - done, less the live fallback

- Single binary, SQLite opened read-only, serves the contract above with gzip.
- Search ranking: `bm25(search, 10, 5, 3) - 2*ln(1 + users_count)`, validated to surface canonical works above Hardcover's zero-shelf duplicate imports.
- `/author/changed` served from stored `updated_at` values.
- Optional live fallback (off by default): if the user supplies their own Hardcover token, cache-miss lookups for brand-new books hit Hardcover one item at a time and persist to a writable overlay DB.
  This keeps per-user API usage near zero, well inside the free 5k/day tier.

### Phase 4: Distribution and automation - mostly done

- GitHub Actions: daily delta job, monthly full snapshot release (seed run executed locally or on a runner with the token as a secret).
- `switch.sh` equivalent that points Bookshelf/Readarr's development settings metadata source at the local instance via its REST API.
- Docker image + docker-compose example.
- Comparison web UI against `hardcover.bookinfo.pro` responses, like the lidarr project has.

## What actually happened

- The full seed cost about 11,600 API queries, not the ~1,500 estimated: Hardcover
  counts each aliased top-level query against the quota, not each HTTP request.
- The `authors` and `series` GraphQL roots are clamped to 100 rows per query
  rather than 1,000, which silently truncated the first seed to 13% of authors.
  Reading the same rows nested under the `books` root avoids the clamp entirely.
- Search needed more than bm25: author-name queries rank by the author's own
  works, `<series> <n>` queries resolve through the series tables, and a
  misspelling falls back to bigram matching on author names. Upstream gets the
  last of these from Typesense's typo tolerance, which SQLite FTS5 has no
  equivalent for.
- Popularity means `ratings_count`, not `users_count`: matching upstream's sort
  is what makes result ordering line up.

## Risks

- **Token expiry**: Hardcover API tokens expire every January 1st. Rotation is a documented annual chore; automation uses a repo secret.
- **Limit tightening**: if 5k/day shrinks, the raw response cache means we only ever need deltas after the initial seed.
- **Contract drift**: Bookshelf and other forks may extend the API (see rreading-glasses `FORKS.md`); track their releases.
- **Search quality**: FTS5 over 2.8M titles needs ranking tuning (weight by `users_count`/`ratings_count`) to match upstream relevance.

## Next

- Live fallback for books newer than the snapshot (Phase 3, deferred).
- Publish a container image so compose pulls instead of building.
- Dataset auto-refresh in the running container, like the lidarr provider's
  `-dataset-refresh`.
- Test coverage beyond `internal/bootstrap`.

## Security note

The API token used for research was shared in plaintext; rotate it on the Hardcover API access page before the automation goes live, and keep the replacement in `.env` (gitignored) / CI secrets only.
