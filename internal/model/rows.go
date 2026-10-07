package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Row is one typed row of a store table. Every table has its own type;
// Model.Put takes any of them. Known columns are typed fields; Extra holds the
// columns this version does not know, so a write keeps them.
type Row interface {
	table() string   // the TableDef name this type belongs to
	encode() api.Row // the stored form (the store's cell formats)
}

// Override is one `Overrides` row (people-owned; held as an ordered list).
type Override struct {
	Person, Action, Value, Note string
	Extra                       map[string]string
}

// AppliedOverride records a one-shot Overrides row already applied.
type AppliedOverride struct {
	RowHash   string
	AppliedAt time.Time
	RunID     string
	Extra     map[string]string
}

// Person is one `People` row.
type Person struct {
	LeadID       api.LeadID
	CreatedAt    time.Time
	ApolloHeldAt time.Time  // never cleared
	MergedInto   api.LeadID // the surviving lead after a same_as merge; permanent
	FirstSeen    map[string]time.Time
	Fields       map[string]Field
	Conflicts    map[string][]Conflict
	Extra        map[string]string
}

// Field is one resolved field in People.fields.
type Field struct {
	Value    string
	SourceID string
	At       time.Time
	Derived  bool // a company domain derived from a work email
}

// Conflict is one value that disagreed with the kept value (People.conflicts).
type Conflict struct {
	Value    string `json:"value"`
	SourceID string `json:"source_id"`
}

// Identity is one `Identities` row: an email or LinkedIn key and its lead.
type Identity struct {
	Key         string // lowercased email or canonical LinkedIn URL
	Kind        string // email or linkedin
	LeadID      api.LeadID
	SourceID    string
	FirstSeenAt time.Time
	Extra       map[string]string
}

// CompanyFact is one `Company facts` row.
type CompanyFact struct {
	Domain     string
	Facts      map[string]Fact // the resolved winner per fact
	Previous   map[string]Fact // the value a change replaced
	Rollups    map[string]any  // JSON values; numbers are float64, exact only up to 2^53
	FirstSeen  map[string]time.Time
	EnrichedAt time.Time
	NotFoundAt time.Time
	// EnrichFailedAt is when the last lookup failed (no answer at all); the
	// domain waits a day before it is tried again.
	EnrichFailedAt time.Time
	Extra          map[string]string
}

// Fact is one company fact: its value, origin and when it was set.
type Fact struct {
	Value  string
	Origin string // companies_tab, enrichment, input
	At     time.Time
}

// WindowEvent is one parsed event kept for detector windows.
type WindowEvent struct {
	EventKey string
	Subject  string // lead or company
	LeadID   api.LeadID
	Domain   string
	Kind     string
	At       time.Time
	Attrs    map[string]string
	Extra    map[string]string
}

// AppliedRow records an input row already applied (LeadID empty for a reject).
type AppliedRow struct {
	SourceID       string
	RowID          string
	RowHash        string
	LeadID         api.LeadID
	FirstAppliedAt time.Time
	KeyConflictAt  time.Time // when the row first carried a key another lead holds; it counts once
	Extra          map[string]string
}

// SeenEvent is one de-duplication key.
type SeenEvent struct {
	EventKey        string
	FirstReceivedAt time.Time
	RunID           string
	Extra           map[string]string
}

// Outcome holds a lead's status facts, kept for good.
type Outcome struct {
	LeadID             api.LeadID
	Status             string
	StatusAt           time.Time
	UnsubscribedAt     time.Time
	UnsubscribedOrigin string // event, lookup, manual
	ReplyStatus        string
	ReplyAt            time.Time
	ContactedAt        time.Time
	DealID             string
	DealStage          string
	DealCheckedAt      time.Time
	Extra              map[string]string
}

// RankedRow is one `Ranked` row. Derived holds one column per derived name,
// already formatted; any column that is not fixed is a derived one.
type RankedRow struct {
	LeadID        api.LeadID
	Email         string
	LinkedInURL   string
	FullName      string
	CompanyDomain string
	Derived       map[string]string
	AccountScore  float64
	ContactScore  float64
	Score         float64
	Status        string
	Lane          string
	Reasons       string
	RubricVersion string
}

// Push is one push-ledger row.
type Push struct {
	LeadID         api.LeadID
	LaneID         string
	Step           string
	LaneKind       string
	Dest           string
	State          string // pending, done, failed, cancelled
	VendorID       string
	Attempts       int
	CalledAt       time.Time
	IntentRun      string
	LastError      string
	FirstStartedAt time.Time
	UpdatedAt      time.Time
	Extra          map[string]string
}

// LogEntry is one `Log` row. Log is only appended and trimmed; runs do not load it.
type LogEntry struct {
	At            time.Time
	RunID         string
	Level         string
	LeadID        api.LeadID
	Email         string
	Kind          string
	Message       string
	RubricVersion string
	Extra         map[string]string
}

// HealthRow is one `Health` row: a result or an open problem.
type HealthRow struct {
	Kind        string // result or problem
	Key         string
	Value       string
	FirstSeenAt time.Time
	UpdatedAt   time.Time
	Extra       map[string]string
}

// StateRow is one `State` key and value.
type StateRow struct {
	Key, Value string
	Extra      map[string]string
}

// ExportRow is one row of an `Export <lane id>` table.
type ExportRow struct {
	LeadID        api.LeadID
	Email         string
	LinkedInURL   string
	FullName      string
	CompanyDomain string
	Score         float64
	Reasons       string
	FirstListedAt time.Time
	Status        string
	DoNotContact  bool // stored as yes or no
	UpdatedAt     time.Time
	Extra         map[string]string
}

func (Override) table() string        { return TableOverrides }
func (AppliedOverride) table() string { return TableAppliedOverrides }
func (Person) table() string          { return TablePeople }
func (Identity) table() string        { return TableIdentities }
func (CompanyFact) table() string     { return TableCompanyFacts }
func (WindowEvent) table() string     { return TableWindowEvents }
func (AppliedRow) table() string      { return TableAppliedRows }
func (SeenEvent) table() string       { return TableSeenEvents }
func (Outcome) table() string         { return TableOutcomes }
func (RankedRow) table() string       { return TableRanked }
func (Push) table() string            { return TablePushes }
func (LogEntry) table() string        { return TableLog }
func (HealthRow) table() string       { return TableHealth }
func (StateRow) table() string        { return TableState }
func (ExportRow) table() string       { return ExportPrefix }

// Encoding. Each encode writes every known column, then the Extra columns that
// do not collide with a known one.

func (r Override) encode() api.Row {
	return withExtra(api.Row{"person": r.Person, "action": r.Action, "value": r.Value, "note": r.Note}, r.Extra)
}

func (r AppliedOverride) encode() api.Row {
	return withExtra(api.Row{"row_hash": r.RowHash, "applied_at": FormatTime(r.AppliedAt), "run_id": r.RunID}, r.Extra)
}

func (r Person) encode() api.Row {
	return withExtra(api.Row{
		"lead_id":        string(r.LeadID),
		"created_at":     FormatTime(r.CreatedAt),
		"apollo_held_at": FormatTime(r.ApolloHeldAt),
		"merged_into":    string(r.MergedInto),
		"first_seen":     timesJSON(r.FirstSeen),
		"fields":         fieldsJSON(r.Fields),
		"conflicts":      objectJSON(r.Conflicts),
	}, r.Extra)
}

func (r Identity) encode() api.Row {
	return withExtra(api.Row{
		"key": r.Key, "kind": r.Kind, "lead_id": string(r.LeadID), "source_id": r.SourceID,
		"first_seen_at": FormatTime(r.FirstSeenAt),
	}, r.Extra)
}

func (r CompanyFact) encode() api.Row {
	return withExtra(api.Row{
		"domain":           r.Domain,
		"facts":            factsJSON(r.Facts),
		"previous":         factsJSON(r.Previous),
		"rollups":          objectJSON(r.Rollups),
		"first_seen":       timesJSON(r.FirstSeen),
		"enriched_at":      FormatTime(r.EnrichedAt),
		"not_found_at":     FormatTime(r.NotFoundAt),
		"enrich_failed_at": FormatTime(r.EnrichFailedAt),
	}, r.Extra)
}

func (r WindowEvent) encode() api.Row {
	return withExtra(api.Row{
		"event_key": r.EventKey, "subject": r.Subject, "lead_id": string(r.LeadID), "domain": r.Domain,
		"kind": r.Kind, "at": FormatTime(r.At), "attrs": objectJSON(r.Attrs),
	}, r.Extra)
}

func (r AppliedRow) encode() api.Row {
	return withExtra(api.Row{
		"source_id": r.SourceID, "row_id": r.RowID, "row_hash": r.RowHash, "lead_id": string(r.LeadID),
		"first_applied_at": FormatTime(r.FirstAppliedAt), "key_conflict_at": FormatTime(r.KeyConflictAt),
	}, r.Extra)
}

func (r SeenEvent) encode() api.Row {
	return withExtra(api.Row{
		"event_key": r.EventKey, "first_received_at": FormatTime(r.FirstReceivedAt), "run_id": r.RunID,
	}, r.Extra)
}

func (r Outcome) encode() api.Row {
	return withExtra(api.Row{
		"lead_id":             string(r.LeadID),
		"status":              r.Status,
		"status_at":           FormatTime(r.StatusAt),
		"unsubscribed_at":     FormatTime(r.UnsubscribedAt),
		"unsubscribed_origin": r.UnsubscribedOrigin,
		"reply_status":        r.ReplyStatus,
		"reply_at":            FormatTime(r.ReplyAt),
		"contacted_at":        FormatTime(r.ContactedAt),
		"deal_id":             r.DealID,
		"deal_stage":          r.DealStage,
		"deal_checked_at":     FormatTime(r.DealCheckedAt),
	}, r.Extra)
}

func (r RankedRow) encode() api.Row {
	return withExtra(api.Row{
		"lead_id": string(r.LeadID), "email": r.Email, "linkedin_url": r.LinkedInURL, "full_name": r.FullName,
		"company_domain": r.CompanyDomain,
		"account_score":  FormatFloat(r.AccountScore),
		"contact_score":  FormatFloat(r.ContactScore),
		"score":          FormatFloat(r.Score),
		"status":         r.Status, "lane": r.Lane, "reasons": r.Reasons, "rubric_version": r.RubricVersion,
	}, r.Derived)
}

func (r Push) encode() api.Row {
	return withExtra(api.Row{
		"lead_id": string(r.LeadID), "lane_id": r.LaneID, "step": r.Step, "lane_kind": r.LaneKind,
		"dest": r.Dest, "state": r.State, "vendor_id": r.VendorID, "attempts": strconv.Itoa(r.Attempts),
		"called_at": FormatTime(r.CalledAt), "intent_run": r.IntentRun, "last_error": r.LastError,
		"first_started_at": FormatTime(r.FirstStartedAt), "updated_at": FormatTime(r.UpdatedAt),
	}, r.Extra)
}

func (r LogEntry) encode() api.Row {
	return withExtra(api.Row{
		"at": FormatTime(r.At), "run_id": r.RunID, "level": r.Level, "lead_id": string(r.LeadID),
		"email": r.Email, "kind": r.Kind, "message": r.Message, "rubric_version": r.RubricVersion,
	}, r.Extra)
}

func (r HealthRow) encode() api.Row {
	return withExtra(api.Row{
		"kind": r.Kind, "key": r.Key, "value": r.Value,
		"first_seen_at": FormatTime(r.FirstSeenAt), "updated_at": FormatTime(r.UpdatedAt),
	}, r.Extra)
}

func (r StateRow) encode() api.Row {
	return withExtra(api.Row{"key": r.Key, "value": r.Value}, r.Extra)
}

func (r ExportRow) encode() api.Row {
	dnc := "no"
	if r.DoNotContact {
		dnc = "yes"
	}
	return withExtra(api.Row{
		"lead_id": string(r.LeadID), "email": r.Email, "linkedin_url": r.LinkedInURL, "full_name": r.FullName,
		"company_domain": r.CompanyDomain, "score": FormatFloat(r.Score), "reasons": r.Reasons,
		"first_listed_at": FormatTime(r.FirstListedAt), "status": r.Status, "do_not_contact": dnc,
		"updated_at": FormatTime(r.UpdatedAt),
	}, r.Extra)
}

// decode turns a stored row into its typed row. def names the table.
func decode(def TableDef, r api.Row) (Row, error) {
	d := decoder{r: r}
	var out Row
	switch def.Name {
	case TableOverrides:
		out = Override{Person: d.s("person"), Action: d.s("action"), Value: d.s("value"), Note: d.s("note"),
			Extra: extra(def, r)}
	case TableAppliedOverrides:
		out = AppliedOverride{RowHash: d.s("row_hash"), AppliedAt: d.t("applied_at"), RunID: d.s("run_id"),
			Extra: extra(def, r)}
	case TablePeople:
		p := Person{LeadID: api.LeadID(d.s("lead_id")), CreatedAt: d.t("created_at"),
			ApolloHeldAt: d.t("apollo_held_at"), MergedInto: api.LeadID(d.s("merged_into")),
			FirstSeen: d.times("first_seen"), Fields: d.fields("fields"), Extra: extra(def, r)}
		d.json("conflicts", &p.Conflicts)
		out = p
	case TableIdentities:
		out = Identity{Key: d.s("key"), Kind: d.s("kind"), LeadID: api.LeadID(d.s("lead_id")),
			SourceID: d.s("source_id"), FirstSeenAt: d.t("first_seen_at"), Extra: extra(def, r)}
	case TableCompanyFacts:
		c := CompanyFact{Domain: d.s("domain"), Facts: d.facts("facts"), Previous: d.facts("previous"),
			FirstSeen: d.times("first_seen"), EnrichedAt: d.t("enriched_at"), NotFoundAt: d.t("not_found_at"),
			EnrichFailedAt: d.t("enrich_failed_at"), Extra: extra(def, r)}
		d.json("rollups", &c.Rollups)
		out = c
	case TableWindowEvents:
		w := WindowEvent{EventKey: d.s("event_key"), Subject: d.s("subject"), LeadID: api.LeadID(d.s("lead_id")),
			Domain: d.s("domain"), Kind: d.s("kind"), At: d.t("at"), Extra: extra(def, r)}
		d.json("attrs", &w.Attrs)
		out = w
	case TableAppliedRows:
		out = AppliedRow{SourceID: d.s("source_id"), RowID: d.s("row_id"), RowHash: d.s("row_hash"),
			LeadID: api.LeadID(d.s("lead_id")), FirstAppliedAt: d.t("first_applied_at"),
			KeyConflictAt: d.t("key_conflict_at"), Extra: extra(def, r)}
	case TableSeenEvents:
		out = SeenEvent{EventKey: d.s("event_key"), FirstReceivedAt: d.t("first_received_at"),
			RunID: d.s("run_id"), Extra: extra(def, r)}
	case TableOutcomes:
		out = Outcome{LeadID: api.LeadID(d.s("lead_id")), Status: d.s("status"), StatusAt: d.t("status_at"),
			UnsubscribedAt: d.t("unsubscribed_at"), UnsubscribedOrigin: d.s("unsubscribed_origin"),
			ReplyStatus: d.s("reply_status"), ReplyAt: d.t("reply_at"), ContactedAt: d.t("contacted_at"),
			DealID: d.s("deal_id"), DealStage: d.s("deal_stage"), DealCheckedAt: d.t("deal_checked_at"),
			Extra: extra(def, r)}
	case TableRanked:
		out = RankedRow{LeadID: api.LeadID(d.s("lead_id")), Email: d.s("email"), LinkedInURL: d.s("linkedin_url"),
			FullName: d.s("full_name"), CompanyDomain: d.s("company_domain"), Derived: extra(def, r),
			AccountScore: d.f("account_score"), ContactScore: d.f("contact_score"), Score: d.f("score"),
			Status: d.s("status"), Lane: d.s("lane"), Reasons: d.s("reasons"), RubricVersion: d.s("rubric_version")}
	case TablePushes:
		out = Push{LeadID: api.LeadID(d.s("lead_id")), LaneID: d.s("lane_id"), Step: d.s("step"),
			LaneKind: d.s("lane_kind"), Dest: d.s("dest"), State: d.s("state"), VendorID: d.s("vendor_id"),
			Attempts: d.i("attempts"), CalledAt: d.t("called_at"), IntentRun: d.s("intent_run"),
			LastError: d.s("last_error"), FirstStartedAt: d.t("first_started_at"), UpdatedAt: d.t("updated_at"),
			Extra: extra(def, r)}
	case TableLog:
		out = LogEntry{At: d.t("at"), RunID: d.s("run_id"), Level: d.s("level"), LeadID: api.LeadID(d.s("lead_id")),
			Email: d.s("email"), Kind: d.s("kind"), Message: d.s("message"), RubricVersion: d.s("rubric_version"),
			Extra: extra(def, r)}
	case TableHealth:
		out = HealthRow{Kind: d.s("kind"), Key: d.s("key"), Value: d.s("value"), FirstSeenAt: d.t("first_seen_at"),
			UpdatedAt: d.t("updated_at"), Extra: extra(def, r)}
	case TableState:
		out = StateRow{Key: d.s("key"), Value: d.s("value"), Extra: extra(def, r)}
	case ExportPrefix:
		dnc := d.s("do_not_contact")
		if dnc != "" && dnc != "yes" && dnc != "no" && d.err == nil {
			d.err = errors.New("do_not_contact: not yes or no")
		}
		out = ExportRow{LeadID: api.LeadID(d.s("lead_id")), Email: d.s("email"), LinkedInURL: d.s("linkedin_url"),
			FullName: d.s("full_name"), CompanyDomain: d.s("company_domain"), Score: d.f("score"),
			Reasons: d.s("reasons"), FirstListedAt: d.t("first_listed_at"), Status: d.s("status"),
			DoNotContact: dnc == "yes", UpdatedAt: d.t("updated_at"), Extra: extra(def, r)}
	default:
		return nil, fmt.Errorf("table %q has no row type", def.Name)
	}
	if d.err != nil {
		return nil, d.err
	}
	return out, nil
}

// FormatTime writes a time in TimeFormat (UTC); the zero time is empty.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeFormat)
}

// ParseTime reads TimeFormat, also accepting RFC 3339; empty is the zero time.
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(TimeFormat, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// The value stays out of the error: a cell may hold an email.
		return time.Time{}, fmt.Errorf("not a time in the form %s", TimeFormat)
	}
	return t.UTC(), nil
}

// FormatFloat writes a number as plain decimal.
func FormatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

type decoder struct {
	r   api.Row
	err error
}

func (d *decoder) s(col string) string { return d.r[col] }

func (d *decoder) fail(col string, err error) {
	if d.err == nil {
		d.err = fmt.Errorf("%s: %w", col, err)
	}
}

func (d *decoder) t(col string) time.Time {
	t, err := ParseTime(d.r[col])
	if err != nil {
		d.fail(col, err)
	}
	return t
}

func (d *decoder) f(col string) float64 {
	if d.r[col] == "" {
		return 0
	}
	f, err := strconv.ParseFloat(d.r[col], 64)
	if err != nil {
		d.fail(col, errors.New("not a number"))
	}
	return f
}

func (d *decoder) i(col string) int {
	if d.r[col] == "" {
		return 0
	}
	n, err := strconv.Atoi(d.r[col])
	if err != nil {
		d.fail(col, errors.New("not a whole number"))
	}
	return n
}

// json reads a JSON object column into v; empty and "{}" leave v nil.
func (d *decoder) json(col string, v any) {
	s := d.r[col]
	if s == "" || s == "{}" {
		return
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		d.fail(col, errors.New("not a JSON object of the expected shape"))
	}
}

func (d *decoder) times(col string) map[string]time.Time {
	var raw map[string]string
	d.json(col, &raw)
	if raw == nil {
		return nil
	}
	out := make(map[string]time.Time, len(raw))
	for k, v := range raw {
		t, err := ParseTime(v)
		if err != nil {
			d.fail(col, err)
		}
		out[k] = t
	}
	return out
}

type wireField struct {
	Value    string `json:"value"`
	SourceID string `json:"source_id"`
	At       string `json:"at"`
	Derived  bool   `json:"derived,omitempty"`
}

func (d *decoder) fields(col string) map[string]Field {
	var raw map[string]wireField
	d.json(col, &raw)
	if raw == nil {
		return nil
	}
	out := make(map[string]Field, len(raw))
	for k, w := range raw {
		t, err := ParseTime(w.At)
		if err != nil {
			d.fail(col, err)
		}
		out[k] = Field{Value: w.Value, SourceID: w.SourceID, At: t, Derived: w.Derived}
	}
	return out
}

type wireFact struct {
	Value  string `json:"value"`
	Origin string `json:"origin"`
	At     string `json:"at"`
}

func (d *decoder) facts(col string) map[string]Fact {
	var raw map[string]wireFact
	d.json(col, &raw)
	if raw == nil {
		return nil
	}
	out := make(map[string]Fact, len(raw))
	for k, w := range raw {
		t, err := ParseTime(w.At)
		if err != nil {
			d.fail(col, err)
		}
		out[k] = Fact{Value: w.Value, Origin: w.Origin, At: t}
	}
	return out
}

// objectJSON writes a map as one JSON object; an empty map is "{}". Keys are
// sorted (encoding/json does), so equal maps encode equally.
func objectJSON[V any](m map[string]V) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("model: encoding a JSON column: %v", err)) // only plain values reach here
	}
	return string(b)
}

func timesJSON(m map[string]time.Time) string {
	out := make(map[string]string, len(m))
	for k, t := range m {
		out[k] = FormatTime(t)
	}
	return objectJSON(out)
}

func fieldsJSON(m map[string]Field) string {
	out := make(map[string]wireField, len(m))
	for k, f := range m {
		out[k] = wireField{Value: f.Value, SourceID: f.SourceID, At: FormatTime(f.At), Derived: f.Derived}
	}
	return objectJSON(out)
}

func factsJSON(m map[string]Fact) string {
	out := make(map[string]wireFact, len(m))
	for k, f := range m {
		out[k] = wireFact{Value: f.Value, Origin: f.Origin, At: FormatTime(f.At)}
	}
	return objectJSON(out)
}

// extra returns the non-empty columns of r that def does not name; nil when
// there are none. An empty cell and a missing column mean the same thing.
func extra(def TableDef, r api.Row) map[string]string {
	var out map[string]string
	for k, v := range r {
		if v != "" && !hasColumn(def, k) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func hasColumn(def TableDef, col string) bool {
	for _, c := range def.Columns {
		if c == col {
			return true
		}
	}
	return false
}

func withExtra(r api.Row, extra map[string]string) api.Row {
	for k, v := range extra {
		if _, known := r[k]; !known {
			r[k] = v
		}
	}
	return r
}
