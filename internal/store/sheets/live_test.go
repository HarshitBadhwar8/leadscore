package sheets_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
)

// liveSize is how much the live check saves.
type liveSize struct{ leads, days, perDay int }

// The 20,000-lead target and a year of events at 1,000 a day (the Sheets
// cell budget), loaded within a minute.
var (
	liveFull      = liveSize{leads: 20_000, days: 365, perDay: 1_000}
	liveLoadLimit = time.Minute
)

// TestLiveSheets is the Sheets store's live measurement
// (LEADSCORE_LIVE_SHEETS): it creates a scratch spreadsheet in real Google
// Sheets, saves 20,000 leads and a year of synthetic events, and checks a run's
// load of them takes under a minute. It builds the spreadsheet from the setup
// template, checks that text Sheets would otherwise reinterpret round-trips
// exactly, and that Health!H1 reads "ok" after a fresh last_success_at. It
// signs in with Google's standard credentials, or with the service-account key
// file LEADSCORE_LIVE_SHEETS names, and deletes the spreadsheet at the end.
func TestLiveSheets(t *testing.T) {
	v := os.Getenv("LEADSCORE_LIVE_SHEETS")
	if v == "" {
		t.Skip("set LEADSCORE_LIVE_SHEETS=1 (or to a service-account key file) to run against real Google Sheets")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	cfg := api.Config{}
	if fi, err := os.Stat(v); err == nil && !fi.IsDir() {
		cfg["credentials"] = v
	}
	svc, err := sheets.Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	liveCheck(ctx, t, svc, liveFull, true)
}

// The live check's own code, run small against the fakes so it is known to
// work before anyone spends a real quota on it.
func TestLiveCheckOnFakes(t *testing.T) {
	f := newFakeGoogle(t)
	// A small request cap makes the save split its commits, as the real size does.
	defer func(old int) { sheets.MaxCommitBytes = old }(sheets.MaxCommitBytes)
	sheets.MaxCommitBytes = 20_000
	f.Sheets.SetCaller("live@p.iam.gserviceaccount.com")
	liveCheck(t.Context(), t, mustConnect(t, f), liveSize{leads: 40, days: 70, perDay: 6}, false)
	if n := f.Sheets.Calls("batchUpdate"); n < 20 {
		t.Errorf("only %d batchUpdates: the save did not split", n)
	}
}

// liveCheck runs the check; formulas says the backend evaluates formulas (the
// fake does not).
func liveCheck(ctx context.Context, t *testing.T, svc *sheets.Services, size liveSize, formulas bool) {
	liveLeads, liveEventDays, liveEventsDay := size.leads, size.days, size.perDay
	about, err := svc.Drive.About.Get().Fields("user(emailAddress)").Context(ctx).Do()
	if err != nil || about.User == nil || about.User.EmailAddress == "" {
		t.Fatalf("who is signed in: %v", err)
	}
	me := about.User.EmailAddress
	id, err := sheets.Create(ctx, svc, sheets.Template{Title: "leadscore live check " + time.Now().UTC().Format(time.RFC3339),
		Accounts: sheets.Accounts{Run: me, Receiver: me}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scratch spreadsheet https://docs.google.com/spreadsheets/d/%s", id)
	t.Cleanup(func() {
		if err := svc.Drive.Files.Delete(id).SupportsAllDrives(true).Do(); err != nil {
			t.Logf("deleting the scratch spreadsheet: %v", err)
		}
	})
	s := sheets.New(svc, id, "")

	// Raw text stays text: formulas, leading zeros, apostrophes, numbers.
	tricky := []string{"=1+1", "0123", "1e5", "TRUE", "2026-01-02", "  padded  ", "'apostrophe", "-0", "+44 20 7946 0000", "line1\nline2"}
	var trickyRows []api.Row
	for i, v := range tricky {
		trickyRows = append(trickyRows, api.Row{"key": fmt.Sprintf("t%d", i), "value": v})
	}
	if err := s.Commit(ctx, []api.TableWrite{{Table: "Live tricky", Op: api.OpAppend, Rows: trickyRows}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadTable(ctx, "Live tricky")
	if err != nil || len(got) != len(tricky) {
		t.Fatalf("tricky values: %d rows, %v", len(got), err)
	}
	for i, r := range got {
		if r["value"] != tricky[i] {
			t.Errorf("value %q came back as %q", tricky[i], r["value"])
		}
	}

	// The staleness formula reads "ok" after a fresh success.
	health := []api.Row{{"kind": "result", "key": "schedule", "value": "15m"},
		{"kind": "result", "key": "last_success_at", "value": model.FormatTime(time.Now())}}
	if err := s.Commit(ctx, []api.TableWrite{{Table: model.TableHealth, Op: api.OpUpsert, Key: []string{"kind", "key"}, Rows: health}}); err != nil {
		t.Fatal(err)
	}
	h1, err := svc.Sheets.Spreadsheets.Values.Get(id, "Health!H1").ValueRenderOption("FORMATTED_VALUE").Context(ctx).Do()
	if err != nil || len(h1.Values) != 1 {
		t.Fatalf("Health!H1: %v", err)
	}
	if formulas && h1.Values[0][0] != "ok" {
		t.Errorf("Health!H1 = %v after a fresh success, want ok", h1.Values[0][0])
	}

	// Save: 20,000 leads with their identities, applied rows, outcomes and
	// verdicts; a year of seen-event keys; 90 days of window events; and a
	// year of raw events in monthly tabs.
	m := model.New()
	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range liveLeads {
		lead := api.LeadID(fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-%012d", i))
		email := fmt.Sprintf("person%d@company%d.example", i, i%5000)
		at := start.Add(time.Duration(i) * time.Minute)
		m.Put(model.TablePeople, model.Person{LeadID: lead, CreatedAt: at,
			Fields: map[string]model.Field{"company.domain": {Value: fmt.Sprintf("company%d.example", i%5000), SourceID: "csv", At: at}}})
		m.Put(model.TableIdentities, model.Identity{Key: email, Kind: "email", LeadID: lead, SourceID: "csv", FirstSeenAt: at})
		m.Put(model.TableIdentities, model.Identity{Key: fmt.Sprintf("https://www.linkedin.com/in/person%d", i), Kind: "linkedin", LeadID: lead, SourceID: "csv", FirstSeenAt: at})
		m.Put(model.TableAppliedRows, model.AppliedRow{SourceID: "csv", RowID: email, RowHash: fmt.Sprintf("%064d", i), LeadID: lead, FirstAppliedAt: at})
		m.Put(model.TableOutcomes, model.Outcome{LeadID: lead, Status: "new", StatusAt: at})
		m.Put(model.TableRanked, model.RankedRow{LeadID: lead, Email: email, CompanyDomain: fmt.Sprintf("company%d.example", i%5000),
			Derived: map[string]string{"tier": "B", "priority": "P2", "fit_signal": "medium"}, Score: 0.5, Status: "new", Lane: "nurture",
			Reasons: "title matches; company size fits", RubricVersion: "1"})
	}
	end := start.AddDate(0, 0, liveEventDays)
	for d := range liveEventDays {
		for j := range liveEventsDay {
			at := start.AddDate(0, 0, d).Add(time.Duration(j) * 86 * time.Second)
			key := fmt.Sprintf("visit:%d:%d", d, j)
			m.Put(model.TableSeenEvents, model.SeenEvent{EventKey: key, FirstReceivedAt: at, RunID: "run"})
			if d >= liveEventDays-90 {
				m.Put(model.TableWindowEvents, model.WindowEvent{EventKey: key, Subject: "lead", Kind: "visit_pricing", At: at,
					LeadID: api.LeadID(fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-%012d", j%liveLeads)), Attrs: map[string]string{"page": "/pricing"}})
			}
		}
	}
	saved := time.Now()
	for _, w := range codec.Encode(m) {
		if err := saveHalving(ctx, w, func(c api.TableWrite) error { return s.Commit(ctx, []api.TableWrite{c}) }); err != nil {
			t.Fatalf("saving %s: %v", w.Table, err)
		}
	}
	var batch []api.RawEvent
	for d := range liveEventDays {
		for j := range liveEventsDay {
			at := start.AddDate(0, 0, d).Add(time.Duration(j) * 86 * time.Second)
			batch = append(batch, api.RawEvent{Kind: "apollo_visit", ReceivedAt: at,
				Body: []byte(fmt.Sprintf(`{"event":"visit","email":"person%d@company%d.example","page":"/pricing","visited_at":%q}`, j, j%5000, model.FormatTime(at)))})
		}
	}
	for i := 0; i < len(batch); {
		n := min(20_000, len(batch)-i)
		err := s.AppendEvents(ctx, batch[i:i+n])
		for errors.Is(err, api.ErrTooLarge) && n > 1 {
			n /= 2
			err = s.AppendEvents(ctx, batch[i:i+n])
		}
		if err != nil {
			t.Fatal(err)
		}
		i += n
	}
	t.Logf("saved %d leads and %d events (%s to %s) in %s", liveLeads, liveEventDays*liveEventsDay,
		start.Format(time.DateOnly), end.Format(time.DateOnly), time.Since(saved).Round(time.Second))

	// Load, as a run does: the model, then every raw event from the start.
	began := time.Now()
	loaded, err := codec.Load(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := s.ReadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	t.Logf("loaded %d people, %d identities, %d seen keys, %d window events and %d raw events in %s",
		len(loaded.People), len(loaded.Identities), len(loaded.SeenEvents), len(loaded.WindowEvents), len(events), took.Round(time.Millisecond))
	if len(loaded.People) != liveLeads || len(loaded.SeenEvents) != liveEventDays*liveEventsDay || len(events) != liveEventDays*liveEventsDay {
		t.Errorf("loaded %d people, %d seen keys, %d raw events; want %d, %d, %d", len(loaded.People), len(loaded.SeenEvents),
			len(events), liveLeads, liveEventDays*liveEventsDay, liveEventDays*liveEventsDay)
	}
	if took > liveLoadLimit {
		t.Errorf("loading took %s, over %s", took, liveLoadLimit)
	}
	if info, err := sheets.Inspect(ctx, svc, id); err == nil {
		t.Logf("cell use: %d of 10,000,000 (largest: %v)", info.Cells, info.Tabs[:min(3, len(info.Tabs))])
	}
}

// saveHalving commits a write in chunks of 20,000 rows, halving a chunk the
// store refuses as too large (each commit must stay under 9MB).
func saveHalving(ctx context.Context, w api.TableWrite, commit func(api.TableWrite) error) error {
	for _, c := range codec.Chunk(w, 20_000) {
		err := commit(c)
		if errors.Is(err, api.ErrTooLarge) && len(c.Rows) > 1 {
			half := len(c.Rows) / 2
			a, b := c, c
			a.Rows, b.Rows = c.Rows[:half], c.Rows[half:]
			if b.Op == api.OpReplace {
				b.Op = api.OpAppend
			}
			if err = saveHalving(ctx, a, commit); err == nil {
				err = saveHalving(ctx, b, commit)
			}
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
