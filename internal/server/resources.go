// Package server serves the Readarr/Bookshelf metadata API contract from
// the prebuilt SQLite dataset. Response shapes mirror rreading-glasses'
// resources so the server is a drop-in alternative for its Hardcover flavor.
package server

type bulkBookResource struct {
	Works   []workResource   `json:"Works"`
	Series  []seriesResource `json:"Series"`
	Authors []authorResource `json:"Authors"`
}

type workResource struct {
	ForeignID      int64    `json:"ForeignId"`
	Title          string   `json:"Title"`
	FullTitle      string   `json:"FullTitle"`
	ShortTitle     string   `json:"ShortTitle"`
	URL            string   `json:"Url"`
	ReleaseDate    string   `json:"ReleaseDate,omitempty"`
	ReleaseDateRaw string   `json:"ReleaseDateRaw,omitempty"`
	Genres         []string `json:"Genres"`
	RelatedWorks   []int    `json:"RelatedWorks"`

	Books   []bookResource   `json:"Books"`
	Series  []seriesResource `json:"Series"`
	Authors []authorResource `json:"Authors"`

	KCA        string `json:"KCA"`
	BestBookID int64  `json:"BestBookId"`

	RatingCount   int64   `json:"RatingCount"`
	AverageRating float64 `json:"AverageRating"`
	RatingSum     int64   `json:"RatingSum"`
}

type authorResource struct {
	ForeignID     int64   `json:"ForeignId"`
	Name          string  `json:"Name"`
	Description   string  `json:"Description"`
	ImageURL      string  `json:"ImageUrl"`
	URL           string  `json:"Url"`
	RatingCount   int64   `json:"RatingCount"`
	AverageRating float32 `json:"AverageRating"`

	Works  []workResource   `json:"Works"`
	Series []seriesResource `json:"Series"`

	KCA string `json:"KCA"`
}

type bookResource struct {
	ForeignID          int64   `json:"ForeignId"`
	Asin               string  `json:"Asin"`
	Description        string  `json:"Description"`
	Isbn13             string  `json:"Isbn13,omitempty"`
	Title              string  `json:"Title"`
	FullTitle          string  `json:"FullTitle"`
	ShortTitle         string  `json:"ShortTitle"`
	Language           string  `json:"Language"`
	Format             string  `json:"Format"`
	EditionInformation string  `json:"EditionInformation"`
	Publisher          string  `json:"Publisher"`
	ImageURL           string  `json:"ImageUrl"`
	IsEbook            bool    `json:"IsEbook"`
	NumPages           int64   `json:"NumPages"`
	RatingCount        int64   `json:"RatingCount"`
	AverageRating      float64 `json:"AverageRating"`
	URL                string  `json:"Url"`
	ReleaseDate        string  `json:"ReleaseDate,omitempty"`
	ReleaseDateRaw     string  `json:"ReleaseDateRaw,omitempty"`

	Contributors []contributorResource `json:"Contributors"`

	KCA       string `json:"KCA"`
	RatingSum int64  `json:"RatingSum"`
}

type seriesResource struct {
	ForeignID   int64  `json:"ForeignId"`
	Title       string `json:"Title"`
	Description string `json:"Description"`

	LinkItems []seriesWorkLinkResource `json:"LinkItems"`

	KCA string `json:"KCA"`
}

type seriesWorkLinkResource struct {
	ForeignWorkID    int64  `json:"ForeignWorkId"`
	PositionInSeries string `json:"PositionInSeries"`
	SeriesPosition   int    `json:"SeriesPosition"`
	Primary          bool   `json:"Primary"`
}

type contributorResource struct {
	ForeignID int64  `json:"ForeignId"`
	Role      string `json:"Role"`
}

type searchResource struct {
	BookID int64                `json:"bookId"`
	WorkID int64                `json:"workId"`
	Author searchResourceAuthor `json:"author"`
}

type searchResourceAuthor struct {
	ID int64 `json:"id"`
}

type recommendationsResource struct {
	WorkIDs []int64 `json:"workIds"`
}
