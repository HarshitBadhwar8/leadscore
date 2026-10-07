package e2e

// The suite's expectations, derived by hand from examples/rubric.yml,
// examples/leads.csv and the fixture events before the suite first ran (the
// working is in testdata/expected.md). Never regenerate these from output:
// when a run disagrees, find out which side is wrong.

// The people of examples/leads.csv, plus Nora, who is in no source at first.
const (
	anna  = "anna.weber@kranlogistik.example"
	jonas = "jonas.brandt@kranlogistik.example"
	lea   = "lea.devries@vanderhaven.example"
	tom   = "tom.hale@ironbridge.example"
	marie = "marie.laurent@routeclair.example"
	sam   = "sam.ortiz@bigbox.example"
	ines  = "ines.ruiz@solmar.example"
	pia   = "pia.schulz@kranlogistik.example"
	nora  = "nora.klein@kranlogistik.example"
)

// noraRow is Nora's line, added to the leads file before run 2.
const noraRow = "nora.klein@kranlogistik.example,Nora Klein,Procurement Manager,Kran Logistik,kranlogistik.example,420,Germany,Series B,,,,,"

// ranked is one expected Ranked row; lead_id, reasons and rubric_version are
// checked separately.
type ranked struct {
	email, linkedin, name, domain string
	fit, tier, priority, hot      string
	account, contact, score       string
	status, lane                  string
}

// Company halves (expected.md, "Company facts"): Kran 65 (tier 1 = 40,
// Germany 10, three leads 10, two ops contacts 5); Van der Haven 35 (tier 2 =
// 25, Netherlands 10); Solmar 40 (tier 1); Ironbridge and BigBox 0 (tier 4);
// Route Clair 0 in C (tier null), 10 then 25 in V (tier 3, then 2 once
// pricing_visits fires).

// rankedV1 is Ranked after run 1 of the vendor variant. Statuses are the
// step-5 fold; the lane is the lane pushed to, else the export lane listed on.
var rankedV1 = []ranked{
	// Kran, tier 1 A. Head 15 + hot 20 (demo visit T1-2d) + 2 sources 10 = 45. Highest cold lane: hot-visitors.
	{anna, "linkedin.com/in/fake-example-0001", "Anna Weber", "kranlogistik.example", "yes", "1", "A", "yes", "65", "45", "110", "new", "hot-visitors"},
	// Kran. Warm path 10 = 10. fleet-ops.
	{jonas, "", "Jonas Brandt", "kranlogistik.example", "yes", "1", "A", "", "65", "10", "75", "new", "fleet-ops"},
	// Kran. No contact points. fleet-ops.
	{pia, "", "Pia Schulz", "kranlogistik.example", "yes", "1", "A", "", "65", "0", "65", "new", "fleet-ops"},
	// Van der Haven, tier 2 B. Director 15 + 2 sources 10 (demo visit T1-10d: not hot). fleet-ops.
	{lea, "", "Lea de Vries", "vanderhaven.example", "yes", "2", "B", "", "35", "25", "60", "new", "fleet-ops"},
	// Ironbridge, tier 4 C. 2 sources (his reply) 10. replied_positive -> demo-followup.
	{tom, "", "Tom Hale", "ironbridge.example", "yes", "4", "C", "", "0", "10", "10", "replied_positive", "demo-followup"},
	// Route Clair: enriched 220 employees, no fit, one pricing visit in 14d -> tier 3 C, account 10. VP 15. nurture only.
	{marie, "", "Marie Laurent", "routeclair.example", "", "3", "C", "", "10", "15", "25", "new", "nurture"},
	// BigBox, 8000 employees -> tier 4 C. Nothing.
	{sam, "", "Sam Ortiz", "bigbox.example", "yes", "4", "C", "", "0", "0", "0", "new", ""},
	// Solmar, target industry, fleet 25 -> tier 1, Spain -> B. fleet-ops.
	{ines, "", "Ines Ruiz", "solmar.example", "yes", "1", "B", "", "40", "0", "40", "new", "fleet-ops"},
}

// rankedV2 is Ranked after run 2 of the vendor variant.
var rankedV2 = []ranked{
	// Pushed in run 1 -> contacted; no push this run, so the lane shown is nurture.
	{anna, "linkedin.com/in/fake-example-0001", "Anna Weber", "kranlogistik.example", "yes", "1", "A", "yes", "65", "45", "110", "contacted", "nurture"},
	{jonas, "", "Jonas Brandt", "kranlogistik.example", "yes", "1", "A", "", "65", "10", "75", "contacted", "nurture"},
	{pia, "", "Pia Schulz", "kranlogistik.example", "yes", "1", "A", "", "65", "0", "65", "contacted", "nurture"},
	{lea, "", "Lea de Vries", "vanderhaven.example", "yes", "2", "B", "", "35", "25", "60", "contacted", "nurture"},
	// His deal step was called -> deal (rule 4 beats his reply); demo-followup no longer matches.
	{tom, "", "Tom Hale", "ironbridge.example", "yes", "4", "C", "", "0", "10", "10", "deal", ""},
	// pricing_visits fires (e4 and e5 in 14d) -> tier 2 B, account 25.
	{marie, "", "Marie Laurent", "routeclair.example", "", "2", "B", "", "25", "15", "40", "new", "nurture"},
	{sam, "", "Sam Ortiz", "bigbox.example", "yes", "4", "C", "", "0", "0", "0", "new", ""},
	{ines, "", "Ines Ruiz", "solmar.example", "yes", "1", "B", "", "40", "0", "40", "contacted", "nurture"},
}

// rankedC is Ranked after run 1 and after run 2 of the CSV-only variant: one
// source per lead (visits from an events source add none), no enrichment.
var rankedC = []ranked{
	// Head 15 + hot 20 = 35.
	{anna, "linkedin.com/in/fake-example-0001", "Anna Weber", "kranlogistik.example", "yes", "1", "A", "yes", "65", "35", "100", "new", "nurture"},
	{jonas, "", "Jonas Brandt", "kranlogistik.example", "yes", "1", "A", "", "65", "10", "75", "new", "nurture"},
	{pia, "", "Pia Schulz", "kranlogistik.example", "yes", "1", "A", "", "65", "0", "65", "new", "nurture"},
	// Director 15.
	{lea, "", "Lea de Vries", "vanderhaven.example", "yes", "2", "B", "", "35", "15", "50", "new", "nurture"},
	{tom, "", "Tom Hale", "ironbridge.example", "yes", "4", "C", "", "0", "0", "0", "new", ""},
	// Employees missing -> tier null, so priority falls to C and nurture (tier <= 3) does not match.
	{marie, "", "Marie Laurent", "routeclair.example", "", "", "C", "", "0", "15", "15", "new", ""},
	{sam, "", "Sam Ortiz", "bigbox.example", "yes", "4", "C", "", "0", "0", "0", "new", ""},
	{ines, "", "Ines Ruiz", "solmar.example", "yes", "1", "B", "", "40", "0", "40", "new", "nurture"},
}

// exported is one expected row of the nurture export.
type exported struct {
	email, score, status, dnc string
}

// exportV is the nurture export after run 2 of the vendor variant. Score is
// the score when listed (run 1); do_not_contact is yes for a cold lane match
// or a contacted lead.
var exportV = []exported{
	{anna, "110", "contacted", "yes"},
	{jonas, "75", "contacted", "yes"},
	{pia, "65", "contacted", "yes"},
	{lea, "60", "contacted", "yes"},
	{ines, "40", "contacted", "yes"},
	{marie, "25", "new", "no"}, // listed at tier 3 in run 1; no cold lane matches her
}

// exportC is the nurture export of the CSV-only variant (no cold lanes).
var exportC = []exported{
	{anna, "100", "new", "no"},
	{jonas, "75", "new", "no"},
	{pia, "65", "new", "no"},
	{lea, "50", "new", "no"},
	{ines, "40", "new", "no"},
}

// pushed is one expected ledger row; every one is done.
type pushed struct {
	email, lane, step, kind, dest string
}

// pushesV is the ledger after run 1 of the vendor variant; run 2 adds none.
var pushesV = []pushed{
	{tom, "demo-followup", "contact", "non-cold", "deals"},
	{tom, "demo-followup", "deal", "non-cold", "deals"},
	{anna, "hot-visitors", "contact", "cold", "sequence/Demo visitors"},
	{anna, "hot-visitors", "enroll", "cold", "sequence/Demo visitors"},
	{jonas, "fleet-ops", "contact", "cold", "sequence/Fleet ops intro"},
	{jonas, "fleet-ops", "enroll", "cold", "sequence/Fleet ops intro"},
	{pia, "fleet-ops", "contact", "cold", "sequence/Fleet ops intro"},
	{pia, "fleet-ops", "enroll", "cold", "sequence/Fleet ops intro"},
	{lea, "fleet-ops", "contact", "cold", "sequence/Fleet ops intro"},
	{lea, "fleet-ops", "enroll", "cold", "sequence/Fleet ops intro"},
	{ines, "fleet-ops", "contact", "cold", "sequence/Fleet ops intro"},
	{ines, "fleet-ops", "enroll", "cold", "sequence/Fleet ops intro"},
}

// sequencesV is where each Apollo contact is enrolled after run 1.
var sequencesV = map[string]string{
	anna:  "Demo visitors",
	jonas: "Fleet ops intro",
	pia:   "Fleet ops intro",
	lea:   "Fleet ops intro",
	ines:  "Fleet ops intro",
}

// tomContactV is what Tom's HubSpot contact is created with in run 1.
var tomContactV = map[string]string{
	"leadscore_lane": "demo-followup", "leadscore_tier": "4", "leadscore_priority": "C", "leadscore_score": "10",
}

// Opt-outs, vendor variant: Anna, Jonas and Pia are pushed to fleet-ops in
// run 1 (no visits here, so Anna is not hot); nobody in run 2. Lea (Overrides),
// Ines (webhook) and Nora (webhook before her row exists) never are.
var (
	optOutPushesV = map[string]string{anna: "fleet-ops", jonas: "fleet-ops", pia: "fleet-ops"}
	optOutOrigins = map[string]string{lea: "manual", ines: "event", nora: "event"}
	optOutExportV = []exported{
		{anna, "", "contacted", "yes"},
		{jonas, "", "contacted", "yes"},
		{pia, "", "contacted", "yes"},
		{marie, "", "new", "no"}, // tier 3 after enrichment, no cold lane
	}
	// The CSV-only variant: Lea and Nora are kept off the list by Overrides.
	optOutExportC = []exported{
		{anna, "", "new", "no"},
		{jonas, "", "new", "no"},
		{pia, "", "new", "no"},
		{ines, "", "new", "no"},
	}
)

// Polling (vendor variant, replies: polling): Tom's polled willing_to_meet
// puts him on demo-followup; Ines's polled unsubscribe blocks her; Sam's
// receiver replied_positive is ignored in polling mode.
var (
	pollingPushes   = map[string]string{tom: "demo-followup", anna: "fleet-ops", jonas: "fleet-ops", pia: "fleet-ops", lea: "fleet-ops"}
	pollingStatuses = map[string]string{tom: "replied_positive", ines: "unsubscribed", sam: "new"}
)

// otto is in no source: only an identified receiver visit reports him.
const otto = "otto.berg@kranlogistik.example"

// Safety case (a): Jonas becomes hot in run 2 but already holds his cold
// push; Ines opts out after contact (expected.md, "Safety cases").
var (
	// Kran 65; warm path 10 + hot 20 + 2 sources 10 = 40; contacted in run 1; not pushed, listed on nurture.
	jonasHotV2 = ranked{jonas, "", "Jonas Brandt", "kranlogistik.example", "yes", "1", "A", "yes", "65", "40", "105", "contacted", "nurture"}
	// The five fleet-ops pushes of run 1, and nothing more after runs 2 and 3.
	laneChangePushes = map[string]string{anna: "fleet-ops", jonas: "fleet-ops", pia: "fleet-ops", lea: "fleet-ops", ines: "fleet-ops"}
	inesExportV2     = exported{ines, "40", "contacted", "yes"}
	inesExportV3     = exported{ines, "40", "unsubscribed", "yes"}
)

// Safety case (b): Otto is receiver-only. Kran 65; hot 20; one source; no
// cold lane holds; listed on nurture with do_not_contact no.
var (
	ottoRanked         = ranked{otto, "", "", "kranlogistik.example", "yes", "1", "A", "yes", "65", "20", "85", "new", "nurture"}
	ottoExport         = exported{otto, "85", "new", "no"}
	receiverOnlyPushes = laneChangePushes
)

// Safety case (c): Anna and Jonas reply positive; one Kran deal; Pia's cold
// push is blocked by it.
var (
	dealPushes = map[string]string{anna: "demo-followup", jonas: "demo-followup", lea: "fleet-ops", ines: "fleet-ops"}
	// Created on the run-1 verdict: Anna 65 + head 15 + 2 sources 10 = 90; Jonas 65 + warm 10 + 2 sources 10 = 85.
	dealContacts = map[string]map[string]string{
		anna:  {"leadscore_lane": "demo-followup", "leadscore_tier": "1", "leadscore_priority": "A", "leadscore_score": "90"},
		jonas: {"leadscore_lane": "demo-followup", "leadscore_tier": "1", "leadscore_priority": "A", "leadscore_score": "85"},
	}
	dealApolloContacts = []string{lea, ines}
	dealStatusesRun2   = map[string]string{anna: "deal", jonas: "deal", pia: "deal", lea: "contacted", ines: "contacted"}
)
