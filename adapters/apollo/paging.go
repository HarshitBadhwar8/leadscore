package apollo

import "fmt"

// eachUntilEmpty is eachPage for a search whose reply has no pagination
// record (the emailer message search, live check 2026-10-08): it reads page
// 1, 2, ... until a page comes back empty. A short page is not the end:
// Apollo may cap per_page below what was asked without saying so, and
// stopping early would lose records while the caller counts the read as
// complete. Up to maxPages pages may hold records; one more is read to see
// it empty, and if it is not, the search is an error, never read as complete.
func eachUntilEmpty(maxPages int, fetch func(page int) (int, error)) error {
	for page := 1; ; page++ {
		n, err := fetch(page)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if page > maxPages {
			return fmt.Errorf("apollo: the search has more than %d pages; it was not read in full", maxPages)
		}
	}
}

// pagination is a search reply's paging record: page and total_pages under
// "pagination" (live check 2026-10-08, on the contact and sequence searches;
// the emailer message search has none).
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
