// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package e2e is the end-to-end suite: the
// assembled loop through engine.RunWith with DefaultHooks, a fake clock and
// an HTTP client pointed at internal/fakes, plus the receiver's in-process
// handler on the same clock, on both stores. It needs no keys and no network.
//
// The expectations are hand-derived (expected_test.go, testdata/expected.md).
package e2e

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
	_ "github.com/HarshitBadhwar8/leadscore/adapters/csv"
	"github.com/HarshitBadhwar8/leadscore/adapters/hubspot"
	_ "github.com/HarshitBadhwar8/leadscore/adapters/sheetsource"
	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/engine"
	fakeapollo "github.com/HarshitBadhwar8/leadscore/internal/fakes/apollo"
	"github.com/HarshitBadhwar8/leadscore/internal/fakes/gcs"
	fakehub "github.com/HarshitBadhwar8/leadscore/internal/fakes/hubspot"
	fakesheets "github.com/HarshitBadhwar8/leadscore/internal/fakes/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/receiver"
	"github.com/HarshitBadhwar8/leadscore/internal/rules"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sheets"
	"github.com/HarshitBadhwar8/leadscore/internal/store/sqlite"
)

// The suite's clock (testdata/expected.md, "Clock and events").
var (
	t0 = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) // posts before run 1
	t1 = t0.Add(time.Hour)                            // run 1
	t2 = t1.Add(time.Hour)                            // run 2
)

const (
	runAccount     = "leadscore-run@e2e.iam.gserviceaccount.com"
	apolloKey      = "e2e-apollo-key"
	receiverSecret = "e2e-receiver-secret"
	mailbox        = "mb-e2e"
)

// clock is the fake clock the runs and the receiver share.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(at time.Time) { c.mu.Lock(); c.now = at; c.mu.Unlock() }

// variant is how the install is set up (testdata/expected.md, "The two
// install variants").
type variant struct {
	vendor  bool // V: Apollo and HubSpot sinks, the receiver, enrichment; else C: CSV only
	polling bool // V with replies: polling
}

var (
	vendorV  = variant{vendor: true}
	csvOnlyC = variant{}
)

// install is one team's install: a folder with leadscore.yml and the rubric,
// a store (SQLite or the Sheets fake), and the fake vendors.
type install struct {
	t       *testing.T
	v       variant
	kind    string // sqlite or sheets
	dir     string
	clock   *clock
	client  *http.Client
	store   api.Backend
	events  api.EventLog
	fs      *fakesheets.Server // sheets only
	sheetID string
	apollo  *fakeapollo.Server // vendor only
	hub     *fakehub.Server
	recv    *receiver.Handler
	leads   [][]string // the leads file: header, then rows
	visits  [][]string // C: the visits events file
	rubric  *rules.Rubric
}

// newInstall sets up an install on store kind ("sqlite" or "sheets"), with
// examples/leads.csv as its leads.
func newInstall(t *testing.T, kind string, v variant) *install {
	t.Helper()
	in := &install{t: t, v: v, kind: kind, dir: t.TempDir(), clock: &clock{now: t0}, client: loopbackClient(t)}
	in.leads = readCSV(t, filepath.Join("..", "..", "examples", "leads.csv"))

	rubricSrc := filepath.Join("..", "..", "examples", "rubric.yml")
	if !v.vendor {
		rubricSrc = filepath.Join("testdata", "rubric_csv_only.yml")
	}
	text, err := os.ReadFile(rubricSrc)
	if err != nil {
		t.Fatal(err)
	}
	if in.rubric, err = rules.Compile(text); err != nil {
		t.Fatal(err)
	}
	in.write("rubric.yml", string(text))

	var cfg strings.Builder
	// schedule 1h: the runs here are an hour apart, so none counts as skipped.
	cfg.WriteString("version: 1\nrubric: rubric.yml\nexport: { dir: out }\nschedule: 1h\n")
	switch kind {
	case "sqlite":
		cfg.WriteString("store: { type: sqlite, path: leadscore.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\n")
		if !v.vendor {
			cfg.WriteString("  - { id: visits, type: csv, path: visits.csv, events: true }\n")
		}
		s, err := sqlite.Open(filepath.Join(in.dir, "leadscore.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		in.store, in.events = s, s
	case "sheets":
		in.fs = fakesheets.New()
		fg := gcs.New()
		fg.CreateBucket("lease")
		srv := httptest.NewServer(gcs.Route(fg, in.fs))
		t.Cleanup(srv.Close)
		// Made the way `leadscore setup sheet` makes it (hourly recalculation,
		// UTC, the template's tabs), so the sheets checks pass.
		svc, err := sheets.Connect(t.Context(), api.Config{"base_url": srv.URL, "_http_client": srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		in.sheetID, err = sheets.Create(t.Context(), svc, sheets.Template{Title: "leadscore", Accounts: sheets.Accounts{Run: runAccount, Receiver: runAccount}, Now: t0})
		if err != nil {
			t.Fatal(err)
		}
		in.fs.SetCaller(runAccount)
		fmt.Fprintf(&cfg, "store: { type: sheets, spreadsheet: %q, lease_bucket: lease, base_url: %q }\n", in.sheetID, srv.URL)
		fmt.Fprintf(&cfg, "sources:\n  - { id: leads, type: sheetsource, tabs: [Leads], base_url: %q }\n", srv.URL)
		if !v.vendor {
			fmt.Fprintf(&cfg, "  - { id: visits, type: sheetsource, tabs: [Visits], events: true, base_url: %q }\n", srv.URL)
		}
		s, err := sheets.Open(t.Context(), api.Config{"type": "sheets", "spreadsheet": in.sheetID,
			"lease_bucket": "lease", "base_url": srv.URL, "_http_client": srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		in.store, in.events = s, s
	default:
		t.Fatalf("store %q", kind)
	}

	if v.vendor {
		t.Setenv(apollo.KeyVariable, apolloKey)
		t.Setenv(hubspot.TokenVariable, fakehub.Token)
		in.apollo = fakeapollo.New(apolloKey)
		in.apollo.AddMailbox(mailbox)
		in.apollo.AddSequence("seq-demo", "Demo visitors")
		in.apollo.AddSequence("seq-fleet", "Fleet ops intro")
		// The one company the enricher knows (expected.md, "The two install variants").
		emp := 220
		in.apollo.AddOrg("routeclair.example", fakeapollo.Org{Name: "Route Clair", Employees: &emp, Country: "France"})
		asrv := httptest.NewServer(in.apollo)
		t.Cleanup(asrv.Close)
		in.hub = fakehub.New()
		hsrv := in.hub.Serve(t)
		replies := "receiver"
		if v.polling {
			replies = "polling"
		}
		fmt.Fprintf(&cfg, "replies: %s\npushes_enabled: true\n", replies)
		fmt.Fprintf(&cfg, "enrich: { type: apollo, base_url: %q }\n", asrv.URL)
		fmt.Fprintf(&cfg, "sinks:\n  apollo: { base_url: %q, mailbox_id: %q }\n", asrv.URL, mailbox)
		fmt.Fprintf(&cfg, "  hubspot: { base_url: %q, pipeline: %q, stage: %q }\n", hsrv.URL, fakehub.PipelineLabel, fakehub.StageOpenName)
		in.recv = receiver.NewHandler(receiver.Options{Store: in.store, Events: in.events, Now: in.clock.Now, BatchWindow: time.Millisecond,
			Getenv: func(k string) string {
				if k == receiver.SecretVar {
					return receiverSecret
				}
				return ""
			}})
		t.Cleanup(in.recv.Close)
	} else {
		in.visits = [][]string{{"event", "at", "email", "domain"}}
	}
	in.write("leadscore.yml", cfg.String())
	in.saveInputs()
	return in
}

func (in *install) write(name, text string) {
	in.t.Helper()
	if err := os.WriteFile(filepath.Join(in.dir, name), []byte(text), 0o600); err != nil {
		in.t.Fatal(err)
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// saveInputs writes the leads (and, CSV-only, the visits) where the sources
// read them: CSV files on SQLite, tabs of the spreadsheet on Sheets.
func (in *install) saveInputs() {
	in.t.Helper()
	files := map[string][][]string{"leads": in.leads}
	if in.visits != nil {
		files["visits"] = in.visits
	}
	for name, rows := range files {
		if in.kind == "sqlite" {
			var b bytes.Buffer
			w := csv.NewWriter(&b)
			if err := w.WriteAll(rows); err != nil {
				in.t.Fatal(err)
			}
			in.write(name+".csv", b.String())
			continue
		}
		cells := make([][]any, len(rows))
		for i, r := range rows {
			for _, c := range r {
				cells[i] = append(cells[i], c)
			}
		}
		tab := map[string]string{"leads": "Leads", "visits": "Visits"}[name]
		if err := in.fs.Put(in.sheetID, tab, cells); err != nil {
			in.t.Fatal(err)
		}
	}
}

// addLead appends a line to the leads file.
func (in *install) addLead(line string) {
	in.t.Helper()
	r, err := csv.NewReader(strings.NewReader(line)).Read()
	if err != nil {
		in.t.Fatal(err)
	}
	in.leads = append(in.leads, r)
	in.saveInputs()
}

// run is one run at the given time, through RunWith with DefaultHooks
// (its summary line goes to the test log). The run must succeed, be healthy
// and not skipped, and raise no problem except those whose key starts with
// one of allowed (a case lists what it expects).
func (in *install) run(at time.Time, allowed ...string) api.RunResult {
	in.t.Helper()
	in.clock.Set(at)
	var out bytes.Buffer
	res, err := engine.RunWithOutput(context.Background(), api.RunOptions{ConfigPath: filepath.Join(in.dir, "leadscore.yml")},
		engine.DefaultHooks(), in.clock.Now, in.client, &out)
	in.t.Logf("run at %s: %s", at.Format(time.RFC3339), strings.TrimSpace(out.String()))
	if err != nil {
		in.t.Fatalf("run at %s: %v", at.Format(time.RFC3339), err)
	}
	if res.Skipped {
		in.t.Fatalf("run at %s was skipped", at.Format(time.RFC3339))
	}
	for _, p := range res.Problems {
		if !slices.ContainsFunc(allowed, func(a string) bool { return strings.HasPrefix(p, a) }) {
			in.t.Errorf("run at %s raised %s (all problems: %v)", at.Format(time.RFC3339), p, res.Problems)
		}
	}
	if !res.Healthy {
		in.t.Errorf("run at %s is unhealthy: %v", at.Format(time.RFC3339), res.Problems)
	}
	return res
}

// loopbackClient is the runs' HTTP client: every fake listens on loopback,
// so a dial anywhere else fails the test, naming the host.
func loopbackClient(t *testing.T) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			t.Errorf("a run dialled %s, which is not a fake", host)
			return nil, fmt.Errorf("e2e: refusing to dial %s outside loopback", host)
		}
		return dial(ctx, network, addr)
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// receive posts Apollo workflow bodies to the in-process receiver at the
// given time, all at once, and
// requires each to be stored (2xx). Posts that arrive within the batch
// window share an append.
func (in *install) receive(at time.Time, bodies ...string) {
	in.t.Helper()
	in.clock.Set(at)
	var wg sync.WaitGroup
	codes := make([]int, len(bodies))
	for i, body := range bodies {
		path := "/apollo/reply"
		if strings.Contains(body, `"website_visited_`) {
			path = "/apollo/visit"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set(receiver.SecretHeader, receiverSecret)
			rec := httptest.NewRecorder()
			in.recv.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c/100 != 2 {
			in.t.Fatalf("the receiver answered %d to %s", c, bodies[i])
		}
	}
}

// Receiver bodies in the shapes of testdata/events.
func visitBody(event, email, domain string, at time.Time) string {
	contact := "{}"
	if email != "" {
		contact = fmt.Sprintf(`{"email":%q}`, email)
	}
	return fmt.Sprintf(`{"event":"website_visited_%s","domain":"dockyard.example","visited_at":%q,"contact":%s,"account":{"domain":%q}}`,
		event, at.Format(time.RFC3339), contact, domain)
}

func replyBody(event, email string) string {
	return fmt.Sprintf(`{"event":"email_%s","contact_email":%q,"contact_stage":""}`, event, email)
}

// addVisits adds visits: to the receiver in the vendor variant, as a row of
// the visits events source in the CSV-only one (stored at the clock's time).
func (in *install) addVisits(at time.Time, rows ...[4]string) {
	in.t.Helper()
	if in.v.vendor {
		var bodies []string
		for _, r := range rows {
			when, _ := time.Parse(time.RFC3339, r[1])
			bodies = append(bodies, visitBody(strings.TrimPrefix(r[0], "visit_"), r[2], r[3], when))
		}
		in.receive(at, bodies...)
		return
	}
	for _, r := range rows {
		in.visits = append(in.visits, r[:])
	}
	in.saveInputs()
}

// setStatus writes an Overrides status row as `leadscore set-status` does:
// load the store, merge.SetStatus, commit only Overrides.
func (in *install) setStatus(person, status string) {
	in.t.Helper()
	ctx := context.Background()
	m, err := codec.Load(ctx, in.store)
	if err != nil {
		in.t.Fatal(err)
	}
	if _, err := merge.SetStatus(m, person, status, in.clock.Now()); err != nil {
		in.t.Fatal(err)
	}
	if err := in.store.Commit(ctx, codec.Encode(m, model.TableOverrides)); err != nil {
		in.t.Fatal(err)
	}
}

func (in *install) rows(table string) []api.Row {
	in.t.Helper()
	rows, err := in.store.ReadTable(context.Background(), table)
	if err != nil {
		in.t.Fatalf("reading %s: %v", table, err)
	}
	return rows
}

// leadIDs maps each lead's primary email to its lead id, from Ranked.
func (in *install) leadIDs() map[string]string {
	out := map[string]string{}
	for _, r := range in.rows(model.TableRanked) {
		out[r["email"]] = r["lead_id"]
	}
	return out
}

// checkRanked compares Ranked with a hand-derived table.
func (in *install) checkRanked(when string, want []ranked) {
	in.t.Helper()
	got := map[string]api.Row{}
	ids := map[string]bool{}
	for _, r := range in.rows(model.TableRanked) {
		got[r["email"]] = r
		if r["lead_id"] == "" || ids[r["lead_id"]] {
			in.t.Errorf("%s: Ranked lead_id %q missing or repeated", when, r["lead_id"])
		}
		ids[r["lead_id"]] = true
		if r["rubric_version"] != in.rubric.Version() {
			in.t.Errorf("%s: %s rubric_version %q, want %q", when, r["email"], r["rubric_version"], in.rubric.Version())
		}
	}
	if len(got) != len(want) {
		in.t.Errorf("%s: Ranked has %d rows, want %d", when, len(got), len(want))
	}
	for _, w := range want {
		r, ok := got[w.email]
		if !ok {
			in.t.Errorf("%s: no Ranked row for %s", when, w.email)
			continue
		}
		g := ranked{r["email"], r["linkedin_url"], r["full_name"], r["company_domain"],
			r["fit_signal"], r["tier"], r["priority"], r["hot"],
			r["account_score"], r["contact_score"], r["score"], r["status"], r["lane"]}
		if g != w {
			in.t.Errorf("%s: Ranked row for %s\n got %+v\nwant %+v\nreasons: %s", when, w.email, g, w, r["reasons"])
		}
	}
}

// rankedRow returns a lead's Ranked row.
func (in *install) rankedRow(email string) api.Row {
	in.t.Helper()
	for _, r := range in.rows(model.TableRanked) {
		if r["email"] == email {
			return r
		}
	}
	in.t.Errorf("no Ranked row for %s", email)
	return api.Row{}
}

// checkRankedRow compares one lead's Ranked row with a hand-derived one.
func (in *install) checkRankedRow(when string, w ranked) {
	in.t.Helper()
	r := in.rankedRow(w.email)
	g := ranked{r["email"], r["linkedin_url"], r["full_name"], r["company_domain"],
		r["fit_signal"], r["tier"], r["priority"], r["hot"],
		r["account_score"], r["contact_score"], r["score"], r["status"], r["lane"]}
	if g != w {
		in.t.Errorf("%s: Ranked row for %s\n got %+v\nwant %+v\nreasons: %s", when, w.email, g, w, r["reasons"])
	}
}

// checkExportRow compares one lead's nurture row with a hand-derived one.
func (in *install) checkExportRow(when string, w exported) {
	in.t.Helper()
	for _, r := range in.rows(model.ExportTable("nurture")) {
		if r["email"] == w.email {
			if r["score"] != w.score || r["status"] != w.status || r["do_not_contact"] != w.dnc {
				in.t.Errorf("%s: nurture row for %s: score %q status %q do_not_contact %q, want %q %q %q",
					when, w.email, r["score"], r["status"], r["do_not_contact"], w.score, w.status, w.dnc)
			}
			return
		}
	}
	in.t.Errorf("%s: %s is not on the nurture list", when, w.email)
}

// checkExport compares the nurture export with a hand-derived list (score
// is compared when the expectation gives one). On SQLite the CSV file must
// hold the same rows.
func (in *install) checkExport(when string, want []exported) {
	in.t.Helper()
	rows := in.rows(model.ExportTable("nurture"))
	got := map[string]api.Row{}
	for _, r := range rows {
		got[r["email"]] = r
	}
	if len(got) != len(want) {
		var emails []string
		for e := range got {
			emails = append(emails, e)
		}
		slices.Sort(emails)
		in.t.Errorf("%s: nurture lists %v, want %d rows", when, emails, len(want))
	}
	for _, w := range want {
		r, ok := got[w.email]
		if !ok {
			in.t.Errorf("%s: %s is not on the nurture list", when, w.email)
			continue
		}
		if (w.score != "" && r["score"] != w.score) || r["status"] != w.status || r["do_not_contact"] != w.dnc {
			in.t.Errorf("%s: nurture row for %s: score %q status %q do_not_contact %q, want %q %q %q",
				when, w.email, r["score"], r["status"], r["do_not_contact"], w.score, w.status, w.dnc)
		}
	}
	if in.kind != "sqlite" {
		return
	}
	path := filepath.Join(in.dir, "out", "nurture.csv")
	st, err := os.Stat(path)
	if err != nil {
		in.t.Fatalf("%s: %v", when, err)
	}
	if st.Mode().Perm() != 0o600 {
		in.t.Errorf("%s: nurture.csv mode %v, want 0600", when, st.Mode().Perm())
	}
	file := readCSV(in.t, path)
	if len(file) != len(rows)+1 {
		in.t.Errorf("%s: nurture.csv has %d lines, the table %d rows", when, len(file), len(rows))
		return
	}
	col := map[string]int{}
	for i, h := range file[0] {
		col[h] = i
	}
	for _, line := range file[1:] {
		r := got[line[col["email"]]]
		if r == nil || line[col["status"]] != r["status"] || line[col["do_not_contact"]] != r["do_not_contact"] || line[col["score"]] != r["score"] {
			in.t.Errorf("%s: nurture.csv line %v does not match the table", when, line)
		}
	}
}

// pushes returns the ledger rows, each with its lead's email added.
func (in *install) pushes() []api.Row {
	in.t.Helper()
	emails := map[string]string{}
	for e, id := range in.leadIDs() {
		emails[id] = e
	}
	rows := in.rows(model.TablePushes)
	for _, r := range rows {
		r["email"] = emails[r["lead_id"]]
	}
	return rows
}

// outcome returns a lead's Outcomes row.
func (in *install) outcome(email string) api.Row {
	in.t.Helper()
	id := in.leadIDs()[email]
	for _, r := range in.rows(model.TableOutcomes) {
		if r["lead_id"] == id {
			return r
		}
	}
	return api.Row{}
}
