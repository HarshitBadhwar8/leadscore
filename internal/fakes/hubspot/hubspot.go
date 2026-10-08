// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package hubspot is a fake HubSpot portal for tests: the CRM calls the
// HubSpot adapter makes (contacts, deals, companies, v4 associations,
// pipelines, properties, the token-info call), backed by in-memory records.
//
// Its answers follow the provisional fixtures in testdata/vendors/hubspot
// (taken from HubSpot's public API documentation and another client,
// not recorded calls; real captures will replace or confirm them). Error
// answers are the fixtures' bodies, served as recorded; a test checks every
// fixture against the fake.
//
// Search can lag, as HubSpot's search index does (SetLag): a record created
// while lag is on is missing from search, but not from reads by id or from
// association reads, until Index. (That reads by id and association reads
// do not lag is itself unconfirmed; the fake assumes it.) A contact can
// be merged into another (Merge): reads of the old id answer with the
// survivor. A read by email also matches `hs_additional_emails` (a
// provisional assumption, unconfirmed either way; the adapter copes with
// both). Calls can be made to fail before the
// portal acts (Fail, FailNext) or after it created a record (FailAfter).
package hubspot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/HarshitBadhwar8/leadscore/sinktest"
)

// Token is the bearer token the fake accepts.
const Token = "test-token"

// Stage ids of the fake's one pipeline, "Sales Pipeline" (id "default").
const (
	PipelineID    = "default"
	PipelineLabel = "Sales Pipeline"
	StageOpen     = "appointmentscheduled" // "Appointment scheduled"
	StageLater    = "qualifiedtobuy"       // "Qualified to buy", open
	StageWon      = "closedwon"
	StageLost     = "closedlost"
	StageOpenName = "Appointment scheduled"
)

// AllScopes is every scope the token-info call reports by default.
var AllScopes = []string{
	"crm.objects.companies.read", "crm.objects.contacts.read", "crm.objects.contacts.write",
	"crm.objects.deals.read", "crm.objects.deals.write", "crm.schemas.contacts.read",
	"crm.schemas.contacts.write", "crm.schemas.deals.read", "crm.schemas.deals.write",
}

type record struct {
	id    string
	props map[string]string
}

// Property is a property as the portal lists it.
type Property struct {
	Name           string `json:"name"`
	Label          string `json:"label"`
	Type           string `json:"type"`
	FieldType      string `json:"fieldType"`
	GroupName      string `json:"groupName"`
	HasUniqueValue bool   `json:"hasUniqueValue"`
}

type fault struct {
	match  func(method, path string) bool
	status int
	body   []byte
	after  bool // create the record, then answer with the fault
	times  int  // -1: always
}

// Server is the fake portal. Create it with New or NewBare.
type Server struct {
	mu        sync.Mutex
	next      int
	objects   map[string]map[string]*record // type -> id -> record
	assoc     map[string]map[string]bool    // "type/id>type" -> ids
	props     map[string]map[string]Property
	groups    map[string]bool
	scopes    []string
	lag       bool
	hidden    map[string]bool   // ids missing from search
	merged    map[string]string // a merged-away contact id -> the survivor
	faults    []*fault
	created   map[string]int // "contact" or "deal" -> records created
	requests  []string
	pageSize  int
	fixtures  map[string][]byte // "<call>/<case>" -> response_body
	statusFor map[string]int
}

// New returns a portal with the leadscore custom properties already set up
// (prefix leadscore_).
func New() *Server {
	s := NewBare()
	for _, p := range []Property{
		{Name: "leadscore_lead_id", Type: "string", FieldType: "text", HasUniqueValue: true},
		{Name: "leadscore_lane", Type: "string", FieldType: "text"},
		{Name: "leadscore_tier", Type: "string", FieldType: "text"},
		{Name: "leadscore_priority", Type: "string", FieldType: "text"},
		{Name: "leadscore_reasons", Type: "string", FieldType: "text"},
		{Name: "leadscore_score", Type: "number", FieldType: "number"},
	} {
		p.GroupName, p.Label = "leadscore", p.Name
		s.props["contacts"][p.Name] = p
	}
	for _, n := range []string{"leadscore_company_domain", "leadscore_lane"} {
		s.props["deals"][n] = Property{Name: n, Label: n, Type: "string", FieldType: "text", GroupName: "leadscore"}
	}
	s.groups["contacts"], s.groups["deals"] = true, true
	return s
}

// NewBare returns a portal with only HubSpot's own properties, as before
// `leadscore setup hubspot`.
func NewBare() *Server {
	s := &Server{
		next:    100,
		objects: map[string]map[string]*record{"contacts": {}, "deals": {}, "companies": {}},
		assoc:   map[string]map[string]bool{},
		props:   map[string]map[string]Property{"contacts": {}, "deals": {}, "companies": {}},
		groups:  map[string]bool{},
		scopes:  append([]string(nil), AllScopes...),
		hidden:  map[string]bool{},
		merged:  map[string]string{},
		created: map[string]int{},
	}
	builtin := map[string][]string{
		"contacts":  {"email", "firstname", "lastname", "jobtitle", "company", "hs_email_optout", "hs_additional_emails", "hs_object_id"},
		"deals":     {"dealname", "pipeline", "dealstage", "amount", "hs_object_id"},
		"companies": {"domain", "name", "hs_object_id"},
	}
	for obj, names := range builtin {
		for _, n := range names {
			s.props[obj][n] = Property{Name: n, Label: n, Type: "string", FieldType: "text", GroupName: obj[:len(obj)-1] + "information"}
		}
	}
	s.loadFixtures()
	return s
}

// Serve starts the fake on an httptest server, closed when the test ends.
func (s *Server) Serve(t interface{ Cleanup(func()) }) *httptest.Server {
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// fixtureDir is testdata/vendors/hubspot at the module root.
func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "vendors", "hubspot")
}

// Fixture is one recorded (or, until one is recorded, provisional) call.
type Fixture struct {
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	Query          string          `json:"query"`
	RequestHeaders []string        `json:"request_headers"`
	RequestBody    json.RawMessage `json:"request_body"`
	Status         int             `json:"status"`
	ResponseBody   json.RawMessage `json:"response_body"`
	Provisional    bool            `json:"provisional"`
}

// Fixtures reads every fixture, keyed "<call>/<case>".
func Fixtures() (map[string]Fixture, error) {
	out := map[string]Fixture{}
	dir := fixtureDir()
	calls, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		if !c.IsDir() {
			continue
		}
		cases, err := os.ReadDir(filepath.Join(dir, c.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range cases {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, c.Name(), f.Name()))
			if err != nil {
				return nil, err
			}
			var fx Fixture
			if err := json.Unmarshal(b, &fx); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", c.Name(), f.Name(), err)
			}
			out[c.Name()+"/"+strings.TrimSuffix(f.Name(), ".json")] = fx
		}
	}
	return out, nil
}

func (s *Server) loadFixtures() {
	fx, err := Fixtures()
	if err != nil {
		panic("fake hubspot: reading the fixtures: " + err.Error())
	}
	s.fixtures, s.statusFor = map[string][]byte{}, map[string]int{}
	for k, f := range fx {
		s.fixtures[k] = f.ResponseBody
		s.statusFor[k] = f.Status
	}
}

// errorBody is a fixture's body and status.
func (s *Server) errorBody(key string) (int, []byte) {
	b, ok := s.fixtures[key]
	if !ok {
		panic("fake hubspot: no fixture " + key)
	}
	return s.statusFor[key], b
}

// --- state helpers -------------------------------------------------------

func (s *Server) newID() string {
	s.next++
	return strconv.Itoa(s.next)
}

func (s *Server) put(typ string, props map[string]string) string {
	id := s.newID()
	p := map[string]string{"hs_object_id": id}
	for k, v := range props {
		p[k] = v
	}
	s.objects[typ][id] = &record{id: id, props: p}
	if s.lag {
		s.hidden[id] = true
	}
	return id
}

// AddContact adds a contact with an email and other properties.
func (s *Server) AddContact(email string, props map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := map[string]string{"email": strings.ToLower(email)}
	for k, v := range props {
		p[k] = v
	}
	return s.put("contacts", p)
}

// AddCompany adds a company with a domain.
func (s *Server) AddCompany(domain string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.put("companies", map[string]string{"domain": domain, "name": domain})
}

// AddDeal adds a deal in the fake's pipeline at the stage, with other
// properties (a salesperson's deal has no leadscore properties).
func (s *Server) AddDeal(stage string, props map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := map[string]string{"pipeline": PipelineID, "dealstage": stage, "dealname": "deal"}
	for k, v := range props {
		p[k] = v
	}
	return s.put("deals", p)
}

// Associate links two records both ways.
func (s *Server) Associate(fromType, fromID, toType, toID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link(fromType, fromID, toType, toID)
}

func (s *Server) link(fromType, fromID, toType, toID string) {
	add := func(a, aid, b, bid string) {
		k := a + "/" + aid + ">" + b
		if s.assoc[k] == nil {
			s.assoc[k] = map[string]bool{}
		}
		s.assoc[k][bid] = true
	}
	add(fromType, fromID, toType, toID)
	add(toType, toID, fromType, fromID)
}

// SetStage moves a deal to a stage.
func (s *Server) SetStage(dealID, stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects["deals"][dealID].props["dealstage"] = stage
}

// SetProp sets a record's property.
func (s *Server) SetProp(typ, id, name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[typ][id].props[name] = value
}

// Merge merges contact from into contact into, as a salesperson merging
// duplicates does: from is gone, and a read of its id answers with into.
func (s *Server) Merge(from, into string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects["contacts"], from)
	s.merged[from] = into
}

// byID finds a record by id, following a contact merge.
func (s *Server) byID(typ, id string) *record {
	if r := s.objects[typ][id]; r != nil {
		return r
	}
	if typ == "contacts" && s.merged[id] != "" {
		return s.objects[typ][s.merged[id]]
	}
	return nil
}

// Delete removes a record.
func (s *Server) Delete(typ, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects[typ], id)
}

// Prop returns a record's property.
func (s *Server) Prop(typ, id, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.objects[typ][id]; r != nil {
		return r.props[name]
	}
	return ""
}

// IDs returns the ids of every record of a type, oldest first.
func (s *Server) IDs(typ string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.objects[typ] {
		out = append(out, id)
	}
	sortIDs(out)
	return out
}

// Associated returns the ids of toType records linked to a record.
func (s *Server) Associated(fromType, fromID, toType string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.linked(fromType, fromID, toType)
}

func (s *Server) linked(fromType, fromID, toType string) []string {
	var out []string
	for id := range s.assoc[fromType+"/"+fromID+">"+toType] {
		if s.objects[toType][id] != nil {
			out = append(out, id)
		}
	}
	sortIDs(out)
	return out
}

// SetLag turns search lag on or off. Records created while it is on stay
// out of search until Index.
func (s *Server) SetLag(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lag = on
}

// Index makes every record visible to search.
func (s *Server) Index() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden = map[string]bool{}
}

// SetPageSize caps search pages (0: as asked), so a test can make a search
// span several pages.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// SetScopes sets the scopes the token-info call reports.
func (s *Server) SetScopes(scopes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = scopes
}

// AddProperty adds or replaces a property, as someone editing the portal.
func (s *Server) AddProperty(object string, p Property) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.props[object][p.Name] = p
}

// Properties returns an object's properties by name.
func (s *Server) Properties(object string) map[string]Property {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Property{}
	for k, v := range s.props[object] {
		out[k] = v
	}
	return out
}

// Requests returns every request as "METHOD path", in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// --- faults --------------------------------------------------------------

// stepMatch is the calls a step makes: the contact step works on contacts;
// the deal step on deals (pipelines, deal search, reads, associations).
func stepMatch(step string) func(method, path string) bool {
	return func(_, path string) bool {
		switch step {
		case "contact":
			return strings.HasPrefix(path, "/crm/v3/objects/contacts")
		case "deal":
			return strings.Contains(path, "deals")
		}
		return false
	}
}

func isCreate(method, path string) bool {
	return method == http.MethodPost && (path == "/crm/v3/objects/contacts" || path == "/crm/v3/objects/deals")
}

func (s *Server) kindBody(kind sinktest.FailKind) (int, []byte) {
	switch kind {
	case sinktest.RateLimited:
		return s.errorBody("errors/rate_limited")
	case sinktest.Transient:
		return s.errorBody("errors/server_error")
	case sinktest.Refused:
		return s.errorBody("contacts_create/invalid_email")
	}
	return s.errorBody("errors/property_missing")
}

// Fail makes the next call of the step fail this way before the portal acts
// (sinktest.Vendor). A refusal is the one HubSpot refusal the adapter maps
// (400 INVALID_EMAIL), served whatever the call.
func (s *Server) Fail(step string, kind sinktest.FailKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, b := s.kindBody(kind)
	s.faults = append(s.faults, &fault{match: stepMatch(step), status: st, body: b, times: 1})
}

// FailAfter makes the step's next create succeed, then answer this way: a
// timeout after HubSpot acted.
func (s *Server) FailAfter(step string, kind sinktest.FailKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, b := s.kindBody(kind)
	m := stepMatch(step)
	s.faults = append(s.faults, &fault{match: func(method, path string) bool { return m(method, path) && isCreate(method, path) },
		status: st, body: b, after: true, times: 1})
}

// FailNext makes the next n calls ("METHOD path" starting with prefix; n -1
// for every call) answer with the fixture's status and body, for example
// FailNext("POST /crm/v3/objects/deals/search", "errors/server_error", 1).
func (s *Server) FailNext(prefix, fixture string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, b := s.errorBody(fixture)
	s.faults = append(s.faults, &fault{match: func(method, path string) bool {
		return strings.HasPrefix(method+" "+path, prefix)
	}, status: st, body: b, times: n})
}

// Count is the number of records created for a step (sinktest.Vendor):
// contacts for "contact", deals for "deal".
func (s *Server) Count(step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created[step]
}

func (s *Server) takeFault(method, path string) *fault {
	for i, f := range s.faults {
		if f.times != 0 && f.match(method, path) {
			if f.times > 0 {
				f.times--
			}
			if f.times == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			return f
		}
	}
	return nil
}

// --- HTTP ----------------------------------------------------------------

var (
	objectPath = regexp.MustCompile(`^/crm/v3/objects/(contacts|deals|companies)(/search|/batch/read)?$`)
	objectGet  = regexp.MustCompile(`^/crm/v3/objects/(contacts|deals|companies)/(\d+)$`)
	assocRead  = regexp.MustCompile(`^/crm/v4/associations/(contacts|deals|companies)/(contacts|deals|companies)/batch/read$`)
	assocPut   = regexp.MustCompile(`^/crm/v4/objects/(contacts|deals|companies)/(\d+)/associations/default/(contacts|deals|companies)/(\d+)$`)
	propsPath  = regexp.MustCompile(`^/crm/v3/properties/(contacts|deals)(/groups)?$`)
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	s.requests = append(s.requests, r.Method+" "+path)
	if r.Header.Get("Authorization") != "Bearer "+Token {
		st, b := s.errorBody("errors/unauthorized")
		writeRaw(w, st, b)
		return
	}
	f := s.takeFault(r.Method, path)
	if f != nil && !f.after {
		writeRaw(w, f.status, f.body)
		return
	}
	rec := &recorder{header: http.Header{}}
	s.route(rec, r.Method, path, r.URL.Query().Get("properties"), body)
	if f != nil {
		writeRaw(w, f.status, f.body)
		return
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	writeRaw(w, rec.status, rec.body)
}

// recorder holds an answer, so a FailAfter fault can drop it after the
// portal acted.
type recorder struct {
	header http.Header
	status int
	body   []byte
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (rec *recorder) json(status int, v any) {
	b, _ := json.Marshal(v)
	rec.status, rec.body = status, b
}

func (rec *recorder) raw(status int, b []byte) { rec.status, rec.body = status, b }

func (s *Server) route(rec *recorder, method, path, query string, body []byte) {
	switch {
	case method == http.MethodPost && path == "/oauth/v2/private-apps/get/access-token-info":
		rec.json(200, map[string]any{"userId": 1, "hubId": 1000, "appId": 2000, "scopes": s.scopes})
	case method == http.MethodGet && path == "/crm/v3/pipelines/deals":
		rec.json(200, pipelinesAnswer())
	case objectPath.MatchString(path) && method == http.MethodPost:
		m := objectPath.FindStringSubmatch(path)
		switch m[2] {
		case "/search":
			s.search(rec, m[1], body)
		case "/batch/read":
			s.batchRead(rec, m[1], body)
		default:
			s.create(rec, m[1], body)
		}
	case objectGet.MatchString(path) && method == http.MethodGet:
		m := objectGet.FindStringSubmatch(path)
		r := s.byID(m[1], m[2])
		if r == nil {
			rec.raw(s.errorBody("errors/not_found"))
			return
		}
		rec.json(200, answer(r, strings.Split(query, ",")))
	case assocRead.MatchString(path) && method == http.MethodPost:
		m := assocRead.FindStringSubmatch(path)
		s.assocBatch(rec, m[1], m[2], body)
	case assocPut.MatchString(path) && method == http.MethodPut:
		m := assocPut.FindStringSubmatch(path)
		if s.objects[m[1]][m[2]] == nil || s.objects[m[3]][m[4]] == nil {
			rec.raw(s.errorBody("errors/not_found"))
			return
		}
		s.link(m[1], m[2], m[3], m[4])
		rec.json(200, map[string]any{"status": "COMPLETE", "results": []any{map[string]any{
			"fromObjectTypeId": typeID(m[1]), "fromObjectId": num(m[2]), "toObjectTypeId": typeID(m[3]), "toObjectId": num(m[4]),
			"labels": []string{}}}})
	case propsPath.MatchString(path):
		m := propsPath.FindStringSubmatch(path)
		s.properties(rec, method, m[1], m[2] != "", body)
	default:
		rec.raw(s.errorBody("errors/not_found"))
	}
}

func typeID(t string) string {
	return map[string]string{"contacts": "0-1", "companies": "0-2", "deals": "0-3"}[t]
}

func num(id string) int64 { n, _ := strconv.ParseInt(id, 10, 64); return n }

func pipelinesAnswer() map[string]any {
	stage := func(id, label string, order int, closed bool, p string) map[string]any {
		return map[string]any{"id": id, "label": label, "displayOrder": order, "archived": false,
			"metadata": map[string]string{"isClosed": strconv.FormatBool(closed), "probability": p}}
	}
	return map[string]any{"results": []any{map[string]any{
		"id": PipelineID, "label": PipelineLabel, "displayOrder": 0, "archived": false,
		"stages": []any{
			stage(StageOpen, StageOpenName, 0, false, "0.2"),
			stage(StageLater, "Qualified to buy", 1, false, "0.4"),
			stage(StageWon, "Closed won", 2, true, "1.0"),
			stage(StageLost, "Closed lost", 3, true, "0.0"),
		},
	}}}
}

func (s *Server) stageKnown(pipeline, stage string) bool {
	if pipeline != PipelineID {
		return false
	}
	switch stage {
	case StageOpen, StageLater, StageWon, StageLost:
		return true
	}
	return false
}

// answer is a record as the API returns it: the asked properties.
func answer(r *record, asked []string) map[string]any {
	props := map[string]any{"hs_object_id": r.id}
	for _, n := range asked {
		if v, ok := r.props[n]; ok {
			props[n] = v
		} else {
			props[n] = nil
		}
	}
	return map[string]any{"id": r.id, "properties": props, "createdAt": "2026-01-01T00:00:00.000Z",
		"updatedAt": "2026-01-01T00:00:00.000Z", "archived": false}
}

type searchBody struct {
	FilterGroups []struct {
		Filters []struct {
			PropertyName string   `json:"propertyName"`
			Operator     string   `json:"operator"`
			Value        string   `json:"value"`
			Values       []string `json:"values"`
		} `json:"filters"`
	} `json:"filterGroups"`
	Properties []string `json:"properties"`
	Limit      int      `json:"limit"`
	After      string   `json:"after"`
}

func (s *Server) search(rec *recorder, typ string, body []byte) {
	var in searchBody
	if err := json.Unmarshal(body, &in); err != nil {
		rec.raw(s.errorBody("errors/property_missing"))
		return
	}
	if len(in.FilterGroups) > 5 {
		rec.raw(s.errorBody("errors/property_missing"))
		return
	}
	for _, g := range in.FilterGroups {
		for _, f := range g.Filters {
			if _, ok := s.props[typ][f.PropertyName]; !ok {
				rec.raw(s.errorBody("errors/property_missing"))
				return
			}
			if f.Operator == "IN" && len(f.Values) > 100 {
				rec.raw(s.errorBody("errors/property_missing"))
				return
			}
		}
	}
	var hits []*record
	for _, r := range s.objects[typ] {
		if s.hidden[r.id] {
			continue
		}
		for _, g := range in.FilterGroups {
			all := true
			for _, f := range g.Filters {
				v := strings.ToLower(r.props[f.PropertyName])
				switch f.Operator {
				case "EQ":
					all = all && v == strings.ToLower(f.Value)
				case "IN":
					in := false
					for _, x := range f.Values {
						in = in || v == strings.ToLower(x)
					}
					all = all && in
				default:
					all = false
				}
			}
			if all {
				hits = append(hits, r)
				break
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return lessID(hits[i].id, hits[j].id) })
	start, _ := strconv.Atoi(in.After)
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 10
	}
	if s.pageSize > 0 && s.pageSize < limit {
		limit = s.pageSize
	}
	end := min(start+limit, len(hits))
	if start > len(hits) {
		start = len(hits)
	}
	results := []any{}
	for _, r := range hits[start:end] {
		results = append(results, answer(r, in.Properties))
	}
	out := map[string]any{"total": len(hits), "results": results}
	if end < len(hits) {
		out["paging"] = map[string]any{"next": map[string]string{"after": strconv.Itoa(end), "link": "?after=" + strconv.Itoa(end)}}
	}
	rec.json(200, out)
}

func (s *Server) batchRead(rec *recorder, typ string, body []byte) {
	var in struct {
		Inputs     []struct{ ID string } `json:"inputs"`
		IDProperty string                `json:"idProperty"`
		Properties []string              `json:"properties"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Inputs) > 100 {
		rec.raw(s.errorBody("errors/property_missing"))
		return
	}
	results := []any{}
	var missing []string
	for _, input := range in.Inputs {
		var hit *record
		if in.IDProperty == "" {
			hit = s.byID(typ, input.ID)
		} else {
			var ids []string
			for id, r := range s.objects[typ] {
				if strings.EqualFold(r.props[in.IDProperty], input.ID) || in.IDProperty == "email" && hasAdditional(r, input.ID) {
					ids = append(ids, id)
				}
			}
			sortIDs(ids)
			if len(ids) > 0 {
				hit = s.objects[typ][ids[0]]
			}
		}
		if hit == nil {
			missing = append(missing, input.ID)
			continue
		}
		results = append(results, answer(hit, in.Properties))
	}
	out := map[string]any{"status": "COMPLETE", "results": results,
		"startedAt": "2026-01-01T00:00:00.000Z", "completedAt": "2026-01-01T00:00:00.100Z"}
	if len(missing) == 0 {
		rec.json(200, out)
		return
	}
	out["numErrors"] = 1
	out["errors"] = []any{map[string]any{"status": "error", "category": "OBJECT_NOT_FOUND",
		"message": "Could not get some " + strings.ToUpper(strings.TrimSuffix(typ, "s")) + " objects, they may be deleted or not exist. Check that ids are valid.",
		"context": map[string][]string{"ids": missing}}}
	rec.json(207, out)
}

func (s *Server) create(rec *recorder, typ string, body []byte) {
	var in struct {
		Properties   map[string]string `json:"properties"`
		Associations []struct {
			To struct {
				ID string `json:"id"`
			} `json:"to"`
			Types []struct {
				Category string `json:"associationCategory"`
				TypeID   int    `json:"associationTypeId"`
			} `json:"types"`
		} `json:"associations"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		rec.raw(s.errorBody("errors/property_missing"))
		return
	}
	for name := range in.Properties {
		if _, ok := s.props[typ][name]; !ok {
			rec.raw(s.errorBody("errors/property_missing"))
			return
		}
	}
	step := ""
	switch typ {
	case "contacts":
		step = "contact"
		email := strings.ToLower(in.Properties["email"])
		if email != "" && !strings.Contains(email, "@") {
			rec.raw(s.errorBody("contacts_create/invalid_email"))
			return
		}
		for _, r := range s.objects["contacts"] {
			if email != "" && strings.EqualFold(r.props["email"], email) {
				st, b := s.errorBody("contacts_create/conflict")
				rec.raw(st, []byte(strings.ReplaceAll(string(b), "Existing ID: 151", "Existing ID: "+r.id)))
				return
			}
		}
		for name, p := range s.props["contacts"] {
			if v := in.Properties[name]; p.HasUniqueValue && v != "" {
				for _, r := range s.objects["contacts"] {
					if r.props[name] == v {
						rec.raw(s.errorBody("errors/property_missing"))
						return
					}
				}
			}
		}
	case "deals":
		step = "deal"
		if !s.stageKnown(in.Properties["pipeline"], in.Properties["dealstage"]) {
			rec.raw(s.errorBody("errors/property_missing"))
			return
		}
	}
	// Associations are checked before anything is created: a create either
	// makes the record with its associations or makes nothing.
	for _, a := range in.Associations {
		if typ != "deals" || len(a.Types) != 1 || a.Types[0].TypeID != 3 || s.objects["contacts"][a.To.ID] == nil {
			rec.raw(s.errorBody("errors/property_missing"))
			return
		}
	}
	id := s.put(typ, in.Properties)
	for _, a := range in.Associations {
		s.link("deals", id, "contacts", a.To.ID)
	}
	if step != "" {
		s.created[step]++
	}
	rec.json(201, answer(s.objects[typ][id], keys(in.Properties)))
}

func hasAdditional(r *record, email string) bool {
	for _, e := range strings.Split(r.props["hs_additional_emails"], ";") {
		if e != "" && strings.EqualFold(strings.TrimSpace(e), email) {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) assocBatch(rec *recorder, from, to string, body []byte) {
	var in struct {
		Inputs []struct{ ID string } `json:"inputs"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Inputs) > 1000 {
		rec.raw(s.errorBody("errors/property_missing"))
		return
	}
	results := []any{}
	var errs []any
	for _, input := range in.Inputs {
		ids := s.linked(from, input.ID, to)
		if s.objects[from][input.ID] == nil || len(ids) == 0 {
			errs = append(errs, map[string]any{"status": "error", "category": "NO_ASSOCIATIONS_FOUND",
				"subCategory": "crm.associations.NO_ASSOCIATIONS_FOUND",
				"message":     "No " + strings.TrimSuffix(to, "s") + " is associated with " + strings.TrimSuffix(from, "s") + " " + input.ID + ".",
				"context":     map[string][]string{"fromObjectId": {input.ID}}})
			continue
		}
		var tos []any
		for _, id := range ids {
			tos = append(tos, map[string]any{"toObjectId": num(id),
				"associationTypes": []any{map[string]any{"category": "HUBSPOT_DEFINED", "typeId": 3, "label": nil}}})
		}
		results = append(results, map[string]any{"from": map[string]string{"id": input.ID}, "to": tos})
	}
	out := map[string]any{"status": "COMPLETE", "results": results,
		"startedAt": "2026-01-01T00:00:00.000Z", "completedAt": "2026-01-01T00:00:00.100Z"}
	if len(errs) == 0 {
		rec.json(200, out)
		return
	}
	out["numErrors"], out["errors"] = len(errs), errs
	rec.json(207, out)
}

func (s *Server) properties(rec *recorder, method, object string, groups bool, body []byte) {
	switch {
	case groups && method == http.MethodPost:
		var in struct{ Name string }
		_ = json.Unmarshal(body, &in)
		if s.groups[object] {
			rec.raw(s.errorBody("property_groups_create/exists"))
			return
		}
		s.groups[object] = true
		rec.json(201, map[string]any{"name": in.Name, "label": in.Name, "displayOrder": -1, "archived": false})
	case !groups && method == http.MethodGet:
		var list []Property
		for _, p := range s.props[object] {
			list = append(list, p)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		rec.json(200, map[string]any{"results": list})
	case !groups && method == http.MethodPost:
		var p Property
		if err := json.Unmarshal(body, &p); err != nil || p.Name == "" || !s.groups[object] {
			rec.raw(s.errorBody("errors/property_missing"))
			return
		}
		if _, ok := s.props[object][p.Name]; ok {
			rec.raw(s.errorBody("properties_create/exists"))
			return
		}
		s.props[object][p.Name] = p
		rec.json(201, p)
	default:
		rec.raw(s.errorBody("errors/not_found"))
	}
}

func lessID(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func sortIDs(ids []string) { sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) }) }
