package seeder

// EntitySpec describes how to bulk-fetch one Hardcover table.
//
// Ranges are partitioned by id with width == the 1000-row query cap (or an
// even divisor of it), so no id range can ever overflow a single query.
// Hardcover allows 5 aliased top-level queries per request.
type EntitySpec struct {
	Name       string // logical name, used for state and raw-data paths
	Root       string // GraphQL root field; defaults to Name
	RangeWidth int64  // ids per aliased query; must not exceed RowCap
	RowCap     int64  // server-side row clamp for this root
	Fields     string // GraphQL selection set for one row
	HasUpdated bool   // supports updated_at delta sync
}

func (s EntitySpec) root() string {
	if s.Root != "" {
		return s.Root
	}
	return s.Name
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

// book_links re-walks the books table but selects only nested author and
// series objects. Hardcover clamps the authors and series roots to 100 rows
// per query, which makes direct sweeps of those tables ~3x more expensive
// than reading the same rows through the 1000-row books root. Authors and
// series that no book references are invisible here, and irrelevant.
const bookLinkFields = `
	id
	updated_at
	contributions {
		contribution
		author { ` + authorFields + ` }
	}
	book_series {
		position
		series { ` + seriesFields + ` }
	}`

var Entities = map[string]EntitySpec{
	// Books carry heavy nested payloads, so use narrower ranges to keep
	// responses around 2MB (measured ~1.9MB per 500 books).
	"books":      {Name: "books", RangeWidth: 500, RowCap: 1000, Fields: bookFields, HasUpdated: true},
	"book_links": {Name: "book_links", Root: "books", RangeWidth: 500, RowCap: 1000, Fields: bookLinkFields, HasUpdated: true},
	// Direct sweeps kept for targeted use; both roots are clamped to 100
	// rows per query, so range width must match.
	"authors": {Name: "authors", RangeWidth: 100, RowCap: 100, Fields: authorFields, HasUpdated: true},
	"series":  {Name: "series", RangeWidth: 100, RowCap: 100, Fields: seriesFields},
}

// SeedOrder lists entities in the order a full seed processes them.
// Authors and series are assembled from book_links during the dataset
// build rather than swept directly.
var SeedOrder = []string{"books", "book_links"}
