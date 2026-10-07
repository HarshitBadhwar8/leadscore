package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

// personClient signs in as the person running `setup sheet`, with their own
// gcloud login rather than the run account, so the person owns the
// spreadsheet (contracts section 9). Their login needs Drive access:
// `gcloud auth login --enable-gdrive-access`. A variable so tests replace it.
var personClient = func(ctx context.Context) (*http.Client, error) {
	out, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
	if err != nil {
		var ee *exec.ExitError
		detail := err.Error()
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("signing in as you with `gcloud auth print-access-token` failed (%s); "+
			"install gcloud and run `gcloud auth login --enable-gdrive-access`", detail)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return nil, errors.New("`gcloud auth print-access-token` printed no token; run `gcloud auth login --enable-gdrive-access`")
	}
	return &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}, Timeout: 60 * time.Second}, nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// setupNow is the clock that names the first Events tab.
var setupNow = time.Now

// testGoogleClient is the HTTP client tests give the Google clients when
// leadscore.yml points `base_url` at a fake; nil outside tests, so a
// `base_url` in a real file is refused (sheets.Connect).
var testGoogleClient *http.Client

// runSetupSheet is `leadscore setup sheet [--view] [--repair]` (contracts
// section 9.1 step 6, and 9.2 for the view).
func runSetupSheet(inv *invocation) int {
	ctx := context.Background()
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	_, view := inv.flags["view"]
	_, repair := inv.flags["repair"]
	key, id := "spreadsheet", c.Store.Spreadsheet
	switch {
	case c.Store.Type == "sqlite" && (view || repair):
		key, id = "view_spreadsheet", c.Store.ViewSpreadsheet
	case view:
		return inv.fail(fmt.Errorf("--view makes the read-only view of a SQLite store, and store.type is %q", c.Store.Type))
	case c.Store.Type != "sheets":
		return inv.fail(fmt.Errorf("setup sheet makes the Sheets store, and store.type is %q (use --view for a SQLite store's view)", c.Store.Type))
	}
	isView := key == "view_spreadsheet"
	acc, err := sheets.AccountsFrom(c, isView)
	if err != nil {
		return inv.fail(err)
	}
	block := api.Config{}
	for k, v := range c.Store.Block {
		block[k] = v
	}
	if base, _ := block["base_url"].(string); base != "" {
		if testGoogleClient != nil {
			block["_http_client"] = testGoogleClient
		}
	} else {
		client, err := personClient(ctx)
		if err != nil {
			return inv.fail(err)
		}
		block["_http_client"] = client
	}
	svc, err := sheets.Connect(ctx, block)
	if err != nil {
		return inv.fail(err)
	}

	if repair {
		if id == "" {
			return inv.fail(fmt.Errorf("store.%s is not set; run `leadscore setup sheet` first", key))
		}
		if err := sheets.Repair(ctx, svc, id, acc); err != nil {
			return inv.fail(err)
		}
		if err := sheets.Share(ctx, svc, id, acc); err != nil {
			return inv.fail(shareError(err))
		}
		fmt.Fprintf(inv.stdout, "repaired %s: hourly recalculation, UTC, tab protection, the Health formula, and sharing with %s\n",
			sheetURL(id), strings.Join(acc.List(), " and "))
		return exitOK
	}

	if id != "" {
		return inv.fail(fmt.Errorf("store.%s is already set (%s); use --repair to fix its settings", key, id))
	}
	if c.Bundle {
		return inv.fail(fmt.Errorf("%s is a hosted bundle; run setup sheet against leadscore.yml, then `leadscore config push`", c.Path))
	}
	tmpl := sheets.Template{Title: "leadscore", View: isView, Accounts: acc, Now: setupNow()}
	if isView {
		tmpl.Title = "leadscore view"
		if tmpl.ExportLanes, err = exportLanes(c); err != nil {
			return inv.fail(err)
		}
	}
	newID, err := sheets.Create(ctx, svc, tmpl)
	if newID != "" {
		// The spreadsheet exists, so record it even if a later step failed:
		// --repair then finishes the job instead of a second spreadsheet.
		if werr := config.SetStoreKey(c.Path, key, newID); werr != nil {
			return inv.fail(fmt.Errorf("created %s but could not record it: %w; set store.%s: %q in leadscore.yml",
				sheetURL(newID), werr, key, newID))
		}
	}
	if err != nil {
		if newID != "" {
			err = fmt.Errorf("%w; the spreadsheet %s is recorded as store.%s, so run `leadscore setup sheet --repair`", err, sheetURL(newID), key)
		}
		return inv.fail(err)
	}
	fmt.Fprintf(inv.stdout, "created %s\nwrote store.%s to %s\n", sheetURL(newID), key, c.Path)
	if err := sheets.Share(ctx, svc, newID, acc); err != nil {
		return inv.fail(fmt.Errorf("%w; then run `leadscore setup sheet --repair`", shareError(err)))
	}
	fmt.Fprintf(inv.stdout, "shared it with %s\n", strings.Join(acc.List(), " and "))
	return exitOK
}

// shareError adds the fix for the usual cause: a Workspace policy that blocks
// sharing with accounts outside the domain.
func shareError(err error) error {
	return fmt.Errorf("%w. If a Google Workspace sharing policy blocks it, ask your Workspace admin "+
		"to allow sharing this file with the service accounts (gserviceaccount.com)", err)
}

// exportLanes reads the rubric's export lane ids, for the view's export tabs
// (contracts section 9.2).
func exportLanes(c *config.Config) ([]string, error) {
	src, err := c.Rubric()
	if err != nil {
		return nil, err
	}
	r, err := rules.Compile(src)
	if err != nil {
		return nil, fmt.Errorf("the view needs the rubric's export lanes, and the rubric does not compile: %w", err)
	}
	var out []string
	for _, l := range r.Lanes() {
		if l.Kind == "export" {
			out = append(out, l.ID)
		}
	}
	return out, nil
}

func sheetURL(id string) string { return "https://docs.google.com/spreadsheets/d/" + id }
