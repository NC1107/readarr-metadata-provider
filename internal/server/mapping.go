package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Hardcover reading_format_id values.
const (
	formatPhysical = 1
	formatAudio    = 2
	formatEbook    = 4
)

// bestAuthorID mirrors rreading-glasses' contributor filtering: the primary
// author is the first contribution whose role is empty or a plain author
// role; translators, narrators, illustrators, and the like never are.
func bestAuthorID(b *rawBook) (int64, string) {
	for _, c := range b.Contributions {
		if c.AuthorID == nil {
			continue
		}
		role := ""
		if c.Contribution != nil {
			role = strings.ToLower(strings.TrimSpace(*c.Contribution))
		}
		switch role {
		case "", "author", "author/narrator":
			return *c.AuthorID, "Author"
		}
	}
	return 0, ""
}

// splitTitle applies the upstream title convention: Title drops the
// ": subtitle" suffix, FullTitle is "title: subtitle".
func splitTitle(title, subtitle string) (short, full string) {
	short, full = title, title
	if subtitle != "" {
		short = strings.ReplaceAll(title, ": "+subtitle, "")
		full = short + ": " + subtitle
	}
	return short, full
}

// releaseDate returns d only when it is a sane YYYY-MM-DD date, matching
// upstream behavior (unparseable dates are omitted so the client skips them).
func releaseDate(d string) string {
	if strings.HasSuffix(d, "BC") {
		return "0001-01-01"
	}
	t, err := time.Parse(time.DateOnly, d)
	if err != nil || t.Year() > 9999 {
		return ""
	}
	return d
}

// editionFormat prefers Hardcover's edition_format string (what upstream
// serves); older raw rows without it fall back to the reading format id.
func editionFormat(e *rawEdition) (format string, isEbook bool) {
	isEbook = e.FormatID == formatEbook ||
		e.EditionFormat == "ebook" || e.EditionFormat == "Kindle Edition"
	if e.EditionFormat != "" {
		return e.EditionFormat, isEbook
	}
	switch e.FormatID {
	case formatEbook:
		return "ebook", true
	case formatAudio:
		return "Audiobook", false
	}
	return e.Physical, isEbook
}

func pickEdition(b *rawBook, editionID int64) *rawEdition {
	if len(b.Editions) == 0 {
		return nil
	}
	if editionID != 0 {
		for i := range b.Editions {
			if b.Editions[i].ID == editionID {
				return &b.Editions[i]
			}
		}
	}
	return &b.Editions[0] // Stored most-shelved first.
}

func orNA(s string) string {
	if s == "" {
		return "N/A"
	}
	return s
}

// app assembles API resources from the store.
type app struct {
	store *store
}

// workResource builds the full response for one work as seen through one
// edition (editionID 0 selects the most popular edition). Series rows are
// resolved via the provided cache so bulk assembly reuses lookups.
func (a *app) workResource(b *rawBook, editionID int64, seriesCache map[int64]*rawSeries) (*workResource, error) {
	authorID, role := bestAuthorID(b)
	if authorID == 0 {
		return nil, fmt.Errorf("%w: work %d has no primary author", errNotFound, b.ID)
	}
	author, err := a.store.author(authorID)
	if err != nil {
		return nil, err
	}
	edition := pickEdition(b, editionID)
	if edition == nil {
		return nil, fmt.Errorf("%w: work %d has no editions", errNotFound, b.ID)
	}

	series := a.seriesResources(b, seriesCache)

	genres := b.CachedTags.Genres
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	workShort, workFull := splitTitle(b.Title, b.Subtitle)
	ratingSum := int64(float64(b.RatingsCount) * b.Rating)

	work := workResource{
		ForeignID:      b.ID,
		Title:          workShort,
		FullTitle:      workFull,
		ShortTitle:     workShort,
		URL:            "https://hardcover.app/books/" + b.Slug,
		ReleaseDate:    releaseDate(b.ReleaseDate),
		ReleaseDateRaw: b.ReleaseDate,
		Genres:         genres,
		RelatedWorks:   []int{},
		Series:         series,
		BestBookID:     b.Editions[0].ID,
		RatingCount:    b.RatingsCount,
		AverageRating:  b.Rating,
		RatingSum:      ratingSum,
	}

	book := a.bookResource(b, edition, authorID, role)

	authorRsc := authorResource{
		ForeignID:   author.ID,
		Name:        author.Name,
		Description: orNA(author.Bio),
		ImageURL:    author.CachedImage.URL,
		URL:         "https://hardcover.app/authors/" + author.Slug,
		Series:      series,
		Works:       []workResource{work}, // Bare copy without relations.
	}

	work.Books = []bookResource{book}
	work.Authors = []authorResource{authorRsc}
	return &work, nil
}

func (a *app) bookResource(b *rawBook, e *rawEdition, authorID int64, role string) bookResource {
	short, full := splitTitle(e.Title, e.Subtitle)
	format, isEbook := editionFormat(e)
	publisher := ""
	if e.Publisher != nil {
		publisher = e.Publisher.Name
	}
	language := ""
	if e.Language != nil {
		if language = e.Language.Code3; language == "" {
			language = iso639_3(e.Language.Code2)
		}
	}
	imageURL := e.CachedImage.URL
	if imageURL == "" {
		imageURL = b.CachedImage.URL
	}
	release := e.ReleaseDate
	if release == "" {
		release = b.ReleaseDate
	}
	return bookResource{
		ForeignID:          e.ID,
		Asin:               e.ASIN,
		Description:        orNA(b.Description),
		Isbn13:             e.ISBN13,
		Title:              short,
		FullTitle:          full,
		ShortTitle:         short,
		Language:           language,
		Format:             format,
		EditionInformation: e.EditionInfo,
		Publisher:          publisher,
		ImageURL:           imageURL,
		IsEbook:            isEbook,
		NumPages:           e.Pages,
		RatingCount:        b.RatingsCount,
		AverageRating:      b.Rating,
		RatingSum:          int64(float64(b.RatingsCount) * b.Rating),
		URL:                "https://hardcover.app/books/" + b.Slug,
		ReleaseDate:        releaseDate(release),
		ReleaseDateRaw:     release,
		Contributors:       []contributorResource{{ForeignID: authorID, Role: role}},
	}
}

func (a *app) seriesResources(b *rawBook, cache map[int64]*rawSeries) []seriesResource {
	out := []seriesResource{}
	for _, bs := range b.BookSeries {
		if bs.SeriesID == nil {
			continue
		}
		sr, ok := cache[*bs.SeriesID]
		if !ok {
			sr, _ = a.store.series(*bs.SeriesID)
			cache[*bs.SeriesID] = sr
		}
		if sr == nil {
			continue
		}
		position := 0.0
		if bs.Position != nil {
			position = *bs.Position
		}
		out = append(out, seriesResource{
			ForeignID:   sr.ID,
			Title:       sr.Name,
			Description: sr.Description,
			LinkItems: []seriesWorkLinkResource{{
				ForeignWorkID:    b.ID,
				PositionInSeries: strconv.FormatFloat(position, 'f', -1, 64),
				SeriesPosition:   int(position),
				Primary:          false,
			}},
		})
	}
	return out
}

// authorResource builds the fat author payload: the author plus every work
// where they are the primary author, most popular first.
func (a *app) authorResource(authorID int64, maxWorks int) (*authorResource, error) {
	author, err := a.store.author(authorID)
	if err != nil {
		return nil, err
	}
	workIDs, err := a.store.authorWorkIDs(authorID, maxWorks)
	if err != nil {
		return nil, err
	}

	seriesCache := map[int64]*rawSeries{}
	works := []workResource{}
	seriesByID := map[int64]*seriesResource{}
	seriesOrder := []int64{}
	var ratingSum float64
	var ratingCount int64

	for _, workID := range workIDs {
		b, err := a.store.work(workID)
		if err != nil {
			continue
		}
		if primary, _ := bestAuthorID(b); primary != authorID {
			continue // Translated, narrated, or co-credited work.
		}
		w, err := a.workResource(b, 0, seriesCache)
		if err != nil {
			continue
		}
		// Aggregate series links across the author's works.
		for _, s := range w.Series {
			existing, ok := seriesByID[s.ForeignID]
			if !ok {
				copied := s
				seriesByID[s.ForeignID] = &copied
				seriesOrder = append(seriesOrder, s.ForeignID)
				continue
			}
			existing.LinkItems = append(existing.LinkItems, s.LinkItems...)
		}
		ratingCount += w.RatingCount
		ratingSum += float64(w.RatingSum)
		works = append(works, *w)
	}

	if len(works) == 0 {
		return nil, fmt.Errorf("%w: author %d has no primary works", errNotFound, authorID)
	}

	series := []seriesResource{}
	for _, id := range seriesOrder {
		series = append(series, *seriesByID[id])
	}

	avg := float32(0)
	if ratingCount > 0 {
		avg = float32(ratingSum / float64(ratingCount))
	}

	return &authorResource{
		ForeignID:     author.ID,
		Name:          author.Name,
		Description:   orNA(author.Bio),
		ImageURL:      author.CachedImage.URL,
		URL:           "https://hardcover.app/authors/" + author.Slug,
		RatingCount:   ratingCount,
		AverageRating: avg,
		Works:         works,
		Series:        series,
	}, nil
}
