package apollo

import "fmt"

// pagination is a search reply's paging record. The shape is unconfirmed (page
// and total_pages under "pagination").
type pagination struct {
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

// eachPage calls fetch for page 1, 2, ... until the reply's total_pages is
// reached. fetch returns the number of records on its page and the page's
// pagination. An answer that cannot be shown complete is an error, never read
// as complete: more pages than maxPages (known from page 1, before reading
// on), or a full page (perPage records) with no total_pages.
func eachPage(maxPages int, fetch func(page int) (int, pagination, error)) error {
	for page := 1; ; page++ {
		n, pg, err := fetch(page)
		if err != nil {
			return err
		}
		if pg.TotalPages > maxPages {
			return fmt.Errorf("apollo: the search has %d pages, over the limit of %d; nothing was read", pg.TotalPages, maxPages)
		}
		if pg.TotalPages <= 0 && n >= perPage {
			return fmt.Errorf("apollo: a full page of the search came back without total_pages, so its end cannot be known")
		}
		if page >= pg.TotalPages {
			return nil
		}
	}
}
