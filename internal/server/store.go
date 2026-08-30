package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

var errNotFound = errors.New("not found")

// rawBook mirrors one row of the seeded books table's JSON payload.
type rawBook struct {
	ID           int64      `json:"id"`
	Title        string     `json:"title"`
	Subtitle     string     `json:"subtitle"`
	Description  string     `json:"description"`
	ReleaseDate  string     `json:"release_date"`
	Slug         string     `json:"slug"`
	Rating       float64    `json:"rating"`
	RatingsCount int64      `json:"ratings_count"`
	UsersCount   int64      `json:"users_count"`
	CachedTags   cachedTags `json:"cached_tags"`
	CachedImage  struct {
		URL string `json:"url"`
	} `json:"cached_image"`
	Contributions []struct {
		AuthorID     *int64  `json:"author_id"`
		Contribution *string `json:"contribution"`
	} `json:"contributions"`
	BookSeries []struct {
		SeriesID *int64   `json:"series_id"`
		Position *float64 `json:"position"`
	} `json:"book_series"`
	Editions []rawEdition `json:"editions"`
}

// cachedTags tolerates both Hardcover's category-keyed object form and any
// other shape by ignoring parse failures.
type cachedTags struct {
	Genres []string
}

func (c *cachedTags) UnmarshalJSON(b []byte) error {
	var byCategory map[string][]struct {
		Tag   string `json:"tag"`
		Count int64  `json:"count"`
	}
	if err := json.Unmarshal(b, &byCategory); err != nil {
		return nil
	}
	for _, t := range byCategory["Genre"] {
		if t.Tag != "" {
			c.Genres = append(c.Genres, t.Tag)
		}
	}
	return nil
}

type rawEdition struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Subtitle    string  `json:"subtitle"`
	ISBN13      string  `json:"isbn_13"`
	ISBN10      string  `json:"isbn_10"`
	ASIN        string  `json:"asin"`
	UsersCount  int64   `json:"users_count"`
	ReleaseDate string  `json:"release_date"`
	Pages       int64   `json:"pages"`
	Physical    string  `json:"physical_format"`
	FormatID    int64   `json:"reading_format_id"`
	Publisher   *struct {
		Name string `json:"name"`
	} `json:"publisher"`
	Language *struct {
		Code2 string `json:"code2"`
		Name  string `json:"language"`
	} `json:"language"`
	CachedImage struct {
		URL string `json:"url"`
	} `json:"cached_image"`
}

type rawAuthor struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Bio         string `json:"bio"`
	CachedImage struct {
		URL string `json:"url"`
	} `json:"cached_image"`
}

type rawSeries struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func getJSON[T any](s *store, query string, id int64) (*T, error) {
	var raw string
	err := s.db.QueryRow(query, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: id %d", errNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *store) work(id int64) (*rawBook, error) {
	return getJSON[rawBook](s, `SELECT json FROM works WHERE id = ?`, id)
}

func (s *store) author(id int64) (*rawAuthor, error) {
	return getJSON[rawAuthor](s, `SELECT json FROM authors WHERE id = ?`, id)
}

func (s *store) series(id int64) (*rawSeries, error) {
	return getJSON[rawSeries](s, `SELECT json FROM series WHERE id = ?`, id)
}

func (s *store) workIDForEdition(editionID int64) (int64, error) {
	var workID int64
	err := s.db.QueryRow(`SELECT work_id FROM editions WHERE id = ?`, editionID).Scan(&workID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: edition %d", errNotFound, editionID)
	}
	return workID, err
}

func (s *store) editionByISBN(isbn string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM editions WHERE isbn13 = ?1 OR isbn10 = ?1
		ORDER BY id LIMIT 1`, isbn).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: isbn %s", errNotFound, isbn)
	}
	return id, err
}

func (s *store) editionByASIN(asin string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM editions WHERE asin = ? ORDER BY id LIMIT 1`, asin).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: asin %s", errNotFound, asin)
	}
	return id, err
}

// authorWorkIDs returns the author's works, most popular first.
func (s *store) authorWorkIDs(authorID int64, limit int) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT w.id FROM work_authors wa
		JOIN works w ON w.id = wa.work_id
		WHERE wa.author_id = ?
		ORDER BY w.users_count DESC, w.id
		LIMIT ?`, authorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type seriesLink struct {
	WorkID   int64
	Position *float64
}

func (s *store) seriesWorks(seriesID int64, limit int) ([]seriesLink, error) {
	rows, err := s.db.Query(`
		SELECT ws.work_id, ws.position FROM work_series ws
		JOIN works w ON w.id = ws.work_id
		WHERE ws.series_id = ?
		ORDER BY ws.position IS NULL, ws.position, w.users_count DESC
		LIMIT ?`, seriesID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []seriesLink
	for rows.Next() {
		var l seriesLink
		if err := rows.Scan(&l.WorkID, &l.Position); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// searchWorks runs the FTS query with popularity-blended ranking, validated
// during the dataset build to surface canonical works above Hardcover's
// zero-shelf duplicate imports.
func (s *store) searchWorks(query string, limit int) ([]int64, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT w.id
		FROM search s JOIN works w ON w.id = s.rowid
		WHERE search MATCH ?
		ORDER BY bm25(search, 10.0, 5.0, 3.0) - 2.0*ln(1 + w.users_count)
		LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *store) topWorkIDs(limit, offset int) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM works ORDER BY users_count DESC, id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ftsQuery converts free text into a safe FTS5 match expression: each token
// quoted (implicit AND), with a prefix match on the final token.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	terms := make([]string, len(fields))
	for i, f := range fields {
		terms[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	terms[len(terms)-1] += "*"
	return strings.Join(terms, " ")
}
