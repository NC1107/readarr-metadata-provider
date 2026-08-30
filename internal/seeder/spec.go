package seeder

// EntitySpec describes how to bulk-fetch one Hardcover table.
//
// Ranges are partitioned by id with width == the 1000-row query cap (or an
// even divisor of it), so no id range can ever overflow a single query.
// Hardcover allows 5 aliased top-level queries per request.
type EntitySpec struct {
	Name       string // GraphQL root field, e.g. "books"
	RangeWidth int64  // ids per aliased query
	Fields     string // GraphQL selection set for one row
	HasUpdated bool   // supports updated_at delta sync
}

const aliasesPerRequest = 5

// Editions are capped at the 20 most-shelved per book, matching what
// Readarr forks display; this avoids paging Hardcover's ~33M edition rows.
const bookFields = `
	id
	title
	subtitle
	description
	release_date
	updated_at
	slug
	rating
	ratings_count
	users_count
	cached_tags
	cached_image
	contributions { author_id contribution }
	book_series { series_id position }
	editions(limit: 20, order_by: {users_count: desc_nulls_last}) {
		id
		title
		subtitle
		isbn_13
		isbn_10
		asin
		users_count
		publisher { name }
		release_date
		pages
		physical_format
		reading_format_id
		language { code2 language }
		cached_image
	}`

const authorFields = `
	id
	name
	slug
	bio
	born_date
	death_date
	books_count
	users_count
	cached_image
	updated_at
	alternate_names
	state`

const seriesFields = `
	id
	name
	slug
	description
	books_count
	primary_books_count
	author_id
	is_completed`

var Entities = map[string]EntitySpec{
	// Books carry heavy nested payloads, so use narrower ranges to keep
	// responses around 2MB (measured ~1.9MB per 500 books).
	"books":   {Name: "books", RangeWidth: 500, Fields: bookFields, HasUpdated: true},
	"authors": {Name: "authors", RangeWidth: 1000, Fields: authorFields, HasUpdated: true},
	"series":  {Name: "series", RangeWidth: 1000, Fields: seriesFields},
}

// SeedOrder lists entities in the order a full seed processes them.
var SeedOrder = []string{"authors", "series", "books"}
