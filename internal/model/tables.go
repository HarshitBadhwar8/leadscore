package model

import "strings"

// SchemaVersion is the store layout this binary writes, as `major.minor`
// (contracts section 4). A release that only adds tables or columns bumps the
// minor; the store's State.schema_version is never lowered.
const SchemaVersion = "1.0"

// TimeFormat is the one stored time form: UTC, never trimmed, so text order is
// time order (contracts, "Formats used everywhere").
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Table names (section 4), exactly as TableWrite.Table carries them.
const (
	TableOverrides        = "Overrides"
	TableAppliedOverrides = "Applied overrides"
	TablePeople           = "People"
	TableIdentities       = "Identities"
	TableCompanyFacts     = "Company facts"
	TableEvents           = "Events" // SQLite's name; Sheets uses one "Events YYYY-MM" tab per month
	TableWindowEvents     = "Window events"
	TableAppliedRows      = "Applied rows"
	TableSeenEvents       = "Seen events"
	TableOutcomes         = "Outcomes"
	TableRanked           = "Ranked"
	TablePushes           = "Pushes"
	TableLog              = "Log"
	TableHealth           = "Health"
	TableState            = "State"
	TableCompanies        = "Companies" // people-owned, any columns; read only

	// ExportPrefix starts every export table's name: "Export <lane id>".
	ExportPrefix = "Export "
	// EventsPrefix starts every monthly Sheets events tab: "Events YYYY-MM".
	EventsPrefix = "Events "
)

// TableDef is one section 4 table with fixed columns.
type TableDef struct {
	Name         string   // section 4 name; for a pattern, the prefix ("Events ", "Export ")
	Key          []string // primary key columns in order; nil for a keyless table
	Columns      []string // fixed columns in order
	Pattern      bool     // Events YYYY-MM and Export <lane id>
	DynamicAfter string   // Ranked: derived-name columns go after this column
}

// Tables is every section 4 table with fixed columns, in section 4 order:
// `storetest.Schema` is built from it. `Leads` and `Companies` take any columns
// and are not listed. On SQLite the events table is plain `Events`.
var Tables = []TableDef{
	{Name: TableOverrides, Columns: []string{"person", "action", "value", "note"}},
	{Name: TableAppliedOverrides, Key: []string{"row_hash"}, Columns: []string{"row_hash", "applied_at", "run_id"}},
	{Name: TablePeople, Key: []string{"lead_id"}, Columns: []string{
		"lead_id", "created_at", "apollo_held_at", "merged_into", "first_seen", "fields", "conflicts"}},
	{Name: TableIdentities, Key: []string{"key"}, Columns: []string{"key", "kind", "lead_id", "source_id", "first_seen_at"}},
	{Name: TableCompanyFacts, Key: []string{"domain"}, Columns: []string{
		"domain", "facts", "previous", "rollups", "first_seen", "enriched_at", "not_found_at"}},
	{Name: EventsPrefix, Key: []string{"seq"}, Columns: []string{"seq", "received_at", "kind", "body"}, Pattern: true},
	{Name: TableWindowEvents, Key: []string{"event_key"}, Columns: []string{
		"event_key", "subject", "lead_id", "domain", "kind", "at", "attrs"}},
	{Name: TableAppliedRows, Key: []string{"source_id", "row_id"}, Columns: []string{
		"source_id", "row_id", "row_hash", "lead_id", "first_applied_at", "key_conflict_at"}},
	{Name: TableSeenEvents, Key: []string{"event_key"}, Columns: []string{"event_key", "first_received_at", "run_id"}},
	{Name: TableOutcomes, Key: []string{"lead_id"}, Columns: []string{
		"lead_id", "status", "status_at", "unsubscribed_at", "unsubscribed_origin", "reply_status", "reply_at",
		"contacted_at", "deal_id", "deal_stage", "deal_checked_at"}},
	{Name: TableRanked, Key: []string{"lead_id"}, DynamicAfter: "company_domain", Columns: []string{
		"lead_id", "email", "linkedin_url", "full_name", "company_domain",
		"account_score", "contact_score", "score", "status", "lane", "reasons", "rubric_version"}},
	{Name: TablePushes, Key: []string{"lead_id", "lane_id", "step"}, Columns: []string{
		"lead_id", "lane_id", "step", "lane_kind", "dest", "state", "vendor_id", "attempts", "called_at",
		"intent_run", "last_error", "first_started_at", "updated_at"}},
	{Name: TableLog, Columns: []string{"at", "run_id", "level", "lead_id", "email", "kind", "message", "rubric_version"}},
	{Name: TableHealth, Key: []string{"kind", "key"}, Columns: []string{"kind", "key", "value", "first_seen_at", "updated_at"}},
	{Name: TableState, Key: []string{"key"}, Columns: []string{"key", "value"}},
	{Name: ExportPrefix, Key: []string{"lead_id"}, Pattern: true, Columns: []string{
		"lead_id", "email", "linkedin_url", "full_name", "company_domain", "score", "reasons",
		"first_listed_at", "status", "do_not_contact", "updated_at"}},
}

// Def returns the definition for a table name, matching a pattern table
// ("Export warm", "Events 2026-10") by its prefix. SQLite's plain "Events"
// matches the events definition too.
func Def(name string) (TableDef, bool) {
	for _, d := range Tables {
		if (!d.Pattern && d.Name == name) || (d.Pattern && strings.HasPrefix(name, d.Name) && len(name) > len(d.Name)) {
			return d, true
		}
	}
	if name == TableEvents {
		return Def(EventsPrefix + "*")
	}
	return TableDef{}, false
}

// ExportTable is the export table name for a lane id.
func ExportTable(laneID string) string { return ExportPrefix + laneID }

// appendNew names the keyed tables whose new rows are written with OpAppend
// (they only grow); every other keyed table upserts (contracts section 12.2).
var appendNew = map[string]bool{
	TableSeenEvents:   true,
	TableWindowEvents: true,
	TableIdentities:   true,
}
