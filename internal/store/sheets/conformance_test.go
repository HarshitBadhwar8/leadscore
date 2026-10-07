package sheets_test

import (
	"net/http/httptest"
	"testing"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	"github.com/HarshitBadhwar8/leadscore/storetest"
)

// fakeGoogle serves the Sheets, Drive and Cloud Storage fakes at one base URL.
type fakeGoogle struct {
	Sheets *fakesheets.Server
	GCS    *gcs.Server
	URL    string
	srv    *httptest.Server
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{Sheets: fakesheets.New(), GCS: gcs.New()}
	f.srv = httptest.NewServer(gcs.Route(f.GCS, f.Sheets))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

// cfg is a store block pointing at the fakes.
func (f *fakeGoogle) cfg(spreadsheet string) api.Config {
	return api.Config{"type": "sheets", "spreadsheet": spreadsheet, "lease_bucket": "lease",
		"base_url": f.URL, "_http_client": f.srv.Client()}
}

func openStore(t *testing.T, f *fakeGoogle) *sheets.Store {
	t.Helper()
	f.GCS.CreateBucket("lease")
	s, err := sheets.Open(t.Context(), f.cfg(f.Sheets.NewSpreadsheet("leadscore")))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The S5 proof, first half: the public conformance suite, green on the Sheets
// store over the Sheets and Cloud Storage fakes.
func TestStoretest(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (api.Backend, api.EventLog) {
		s := openStore(t, newFakeGoogle(t))
		return s, s
	})
}
