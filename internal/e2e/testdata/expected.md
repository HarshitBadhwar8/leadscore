# End-to-end expected results, derived by hand

Everything below was worked out by hand from `examples/rubric.yml`,
`examples/leads.csv` and the fixture events listed here, **before the suite
was first run**. It is never regenerated from output. When a run disagrees,
find out which side is wrong; fix the code only for a clear bug.

`expected_test.go` holds the same numbers as Go tables, with a one-line
derivation per row. This file holds the working.

## Clock and events

| Name | Time (UTC) | Used for |
|---|---|---|
| T0 | 2026-10-05 08:00 | receiver posts (or CSV rows) before run 1 |
| T1 | 2026-10-05 09:00 | run 1 |
| T1+30m | 2026-10-05 09:30 | the second pricing visit, posted between runs |
| T2 | 2026-10-05 10:00 | run 2 |

Fixture events for the **main scenario** (both variants):

| # | Kind | Person | `visited_at` | When stored |
|---|---|---|---|---|
| e1 | `visit_demo` | anna.weber@kranlogistik.example | T1 − 2d | T0 |
| e2 | `visit_demo` | lea.devries@vanderhaven.example | T1 − 10d | T0 |
| e3 | `visit_pricing` | company only, routeclair.example | T1 − 20d | T0 |
| e4 | `visit_pricing` | company only, routeclair.example | T1 − 2d | T0 |
| e5 | `visit_pricing` | company only, routeclair.example | T1 + 30m | T1 + 30m |
| e6 | `replied_positive` | tom.hale@ironbridge.example | — | T0 (vendor variant only) |

The vendor variant posts these to the in-process receiver; the CSV-only
variant reads e1–e5 from a visits CSV source (`events: true`), with e5 added
to the file before run 2.

## The two install variants

- **V (vendor):** the example rubric unchanged; `sinks.apollo` and
  `sinks.hubspot` on the fakes; `replies: receiver`; `pushes_enabled: true`;
  `enrich: { type: apollo }` on the Apollo fake, which knows only
  `routeclair.example` (employees 220, country France). Every other domain
  answers not found, so its CSV facts stand.
- **C (CSV-only):** `testdata/rubric_csv_only.yml`, the example rubric with
  only the `nurture` export lane. The example's cold lanes require
  `receiver_only: false`, and a cold lane claims its leads for the
  export (`do_not_contact: yes`) even with no sink set up, so a CSV-only team
  running the example unchanged would see every fit lead marked do-not-contact.
  The variant drops the three push lanes; everything above `lanes:` is the
  example's text exactly (a test checks this). No enrichment, no receiver.

## Company facts (from leads.csv)

| Company | employees | region | legacy_tool_seen | largest_fleet | ops_contacts | main_industry | leads_seen |
|---|---|---|---|---|---|---|---|
| kranlogistik.example | 420 | Germany | true (Anna: Spreadsheets) | 60 (Jonas) | 2 (Anna "Head of Operations", Pia "Operations Lead") | Freight forwarding | 3 |
| vanderhaven.example | 1200 | Netherlands | true (Excel) | 12 | 1 | Cold chain | 1 |
| ironbridge.example | 30 | United Kingdom | true (Paper) | 8 | 0 | Haulage | 1 |
| routeclair.example | missing (V: 220 from enrichment) | France | false (no software) | none | 0 | Retail | 1 |
| bigbox.example | 8000 | United States | true (Spreadsheets) | 300 | 1 ("Operations Analyst") | Freight forwarding | 1 |
| solmar.example | 150 | Spain | false (Manhattan WMS) | 25 | 0 | Food distribution | 1 |

Derived per company (rules top to bottom, first match wins):

- **Kran:** fit_signal `yes` (legacy tool). tier: employees present, 420 in
  50..5000, fit yes and largest_fleet 60 ≥ 20 → **1**. priority: tier 1 and
  Germany in home_countries → **A**. Account: tier 1 = 40, home region = 10,
  leads_seen 3 → band {2:5, 3:10} = 10, ops_contacts 2 ≥ 2 = 5 → **65**.
- **Van der Haven:** fit `yes` (Excel). tier: fleet 12 < 20, so not 1; fit yes
  → **2**. priority: not (tier 1 and home); tier ≤ 2 → **B**. Account: tier 2
  = 25, Netherlands = 10, leads_seen 1 = 0, ops 1 = 0 → **35**.
- **Ironbridge:** fit `yes` (Paper). tier: 30 < 50 → **4**. priority **C**.
  Account: 0 (no tier rule pays 4, UK not home, one lead, no ops).
- **BigBox:** fit `yes`. tier: 8000 > 5000 → **4**. priority **C**. Account **0**.
- **Solmar:** legacy false, but main_industry "Food distribution" is a target
  → fit `yes`. tier: 150 in range, fit yes and fleet 25 ≥ 20 → **1**.
  priority: tier 1 but Spain is not home; tier ≤ 2 → **B**. Account: tier 1 =
  40, rest 0 → **40**.
- **Route Clair, C:** legacy false, Retail not a target → fit no value.
  tier: employees missing → `then: null` → no value. priority: both tier
  comparisons on an absent value are false → **C**. Account **0**.
- **Route Clair, V:** employees 220 from enrichment (run 1, step 4, before
  scoring). tier: present, in range, fit not yes (twice), then
  `pricing_visits`:
  - run 1 at T1: window (T1 − 14d, T1] holds e4 only (e3 is 20 days old) →
    1 < min 2 → not fired → **else 3**. priority **C**. Account: tier 3 = 10
    → **10**.
  - run 2 at T2: e4 and e5 → 2 → **fires** → tier **2**, priority **B**,
    account tier 2 = 25 → **25**. Run 2 logs `tier_change` and
    `priority_change` for Marie only (no other lead's tier or priority moves).

## Detector firings

| Detector | Run 1 (T1) | Run 2 (T2) | Why |
|---|---|---|---|
| `demo_visit` (lead, first `visit_demo` within 7d) | Anna only | Anna only | Anna's first demo visit is T1 − 2d; Lea's is T1 − 10d, outside 7 days |
| `pricing_visits` (company, ≥ 2 `visit_pricing` in 14d) | none | routeclair.example | see Route Clair above |

Seen in `Ranked` through `hot` (Anna `yes`, Lea empty) and, in V, Marie's
tier 3 → 2. In C, Route Clair's tier is null before the detector rule is
reached, so the firing is not visible in `Ranked`; the suite checks the three
`visit_pricing` rows in `Window events` there instead.

## Contact half, per lead

Rules: title contains head/director/vp = 15; `hot` = 20; `warm_path`
present = 10; sources_seen band {2: 10}.

`sources_seen` counts channels that reported the lead through input rows or
identities. In V, an identified receiver request adds a `receiver` input row
(see docs/reference.md, "Receiver rows"), so Anna and Lea (identified
visits) and Tom (his reply) count 2. Company-only visits make no row, so Marie stays 1. In C,
visits come from an events source, which yields events, not input rows, so
every lead counts 1.

| Lead | Title | V contact | C contact |
|---|---|---|---|
| Anna | Head of Operations | 15 + 20 (hot) + 10 (2 sources) = **45** | 15 + 20 = **35** |
| Jonas | Warehouse Manager | warm path 10 = **10** | **10** |
| Lea | Director of Operations | 15 + 10 (2 sources) = **25** | **15** |
| Tom | CFO | 10 (2 sources) = **10** | **0** |
| Marie | VP Supply Chain | 15 ("vp", case-insensitive) = **15** | **15** |
| Sam | Operations Analyst | **0** | **0** |
| Ines | Logistics Coordinator | **0** | **0** |
| Pia | Operations Lead | **0** | **0** |

## Lanes and pushes (V)

Lanes whose `when` holds, highest priority first:

- Anna: hot-visitors (hot, not receiver-only), fleet-ops (tier 1, fit yes,
  A), nurture (tier ≤ 3).
- Jonas, Pia: fleet-ops, nurture. Lea: fleet-ops (tier 2, fit yes, B),
  nurture. Ines: fleet-ops (tier 1, fit yes, B), nurture.
- Tom: demo-followup in run 1 (status replied_positive); nothing in run 2
  (status is `deal`, which is not replied_positive; tier 4 fails the rest).
- Marie: nurture only (no fit, not hot). Sam: none (tier 4).

**Run 1 pushes** (6 pushes, 12 ledger rows, every row `done`):

| Lead | Lane | Kind | Dest | Steps |
|---|---|---|---|---|
| Tom | demo-followup | non-cold | deals | contact, deal |
| Anna | hot-visitors | cold | sequence/Demo visitors | contact, enroll |
| Jonas | fleet-ops | cold | sequence/Fleet ops intro | contact, enroll |
| Pia | fleet-ops | cold | sequence/Fleet ops intro | contact, enroll |
| Lea | fleet-ops | cold | sequence/Fleet ops intro | contact, enroll |
| Ines | fleet-ops | cold | sequence/Fleet ops intro | contact, enroll |

Anna goes to hot-visitors, not fleet-ops: a lead goes to its
highest-priority matching cold lane (25 > 20). HubSpot gets one contact (Tom,
created with `leadscore_lane` demo-followup, tier 4, priority C, score 10)
and one deal named `ironbridge.example` with `leadscore_company_domain`
`ironbridge.example`, associated with Tom's contact. Apollo gets five
contacts: Anna enrolled in "Demo visitors", the other four in "Fleet ops
intro".

**Run 2 pushes:** none. Every cold lead already holds its one cold push;
Tom no longer matches demo-followup.

## Statuses

- Run 1 `Ranked` status is the step-5 fold (the lane plan comes after
  PrePush): everyone `new`, Tom `replied_positive`.
- Run 2: the five cold leads `contacted` (a completed cold push sets
  `contacted_at`, rule 6). Tom `deal` (rule 4: his deal step at Ironbridge
  has `called_at`, and the run-2 lookup reads the deal open; rule 4 beats
  his reply, rule 5). Marie and Sam `new`.

## The `lane` column

Once PrePush has planned lanes, `lane` is the highest-priority lane the lead
is pushed to this run, or an export lane it is listed on (passes the lane's
built-in checks); empty when none. So in V run 1 a pushed lead shows its push
lane; in run 2, where nobody is pushed, a lead with a nurture match shows
`nurture`.

## Ranked, V (main scenario)

Columns compared: email, linkedin_url, full_name, company_domain, the four
derived names, the three scores, status, lane. `rubric_version` must equal the
compiled rubric's version. `reasons` is the renderer's text and is not
compared word for word. `linkedin_url` is the canonical identity form:
`linkedin.com/in/fake-example-0001`.

Run 1:

| Lead | fit | tier | prio | hot | acct | contact | score | status | lane |
|---|---|---|---|---|---|---|---|---|---|
| Anna | yes | 1 | A | yes | 65 | 45 | 110 | new | hot-visitors |
| Jonas | yes | 1 | A | | 65 | 10 | 75 | new | fleet-ops |
| Pia | yes | 1 | A | | 65 | 0 | 65 | new | fleet-ops |
| Lea | yes | 2 | B | | 35 | 25 | 60 | new | fleet-ops |
| Tom | yes | 4 | C | | 0 | 10 | 10 | replied_positive | demo-followup |
| Marie | | 3 | C | | 10 | 15 | 25 | new | nurture |
| Sam | yes | 4 | C | | 0 | 0 | 0 | new | |
| Ines | yes | 1 | B | | 40 | 0 | 40 | new | fleet-ops |

Run 2 (changes from run 1 in bold):

| Lead | fit | tier | prio | hot | acct | contact | score | status | lane |
|---|---|---|---|---|---|---|---|---|---|
| Anna | yes | 1 | A | yes | 65 | 45 | 110 | **contacted** | **nurture** |
| Jonas | yes | 1 | A | | 65 | 10 | 75 | **contacted** | **nurture** |
| Pia | yes | 1 | A | | 65 | 0 | 65 | **contacted** | **nurture** |
| Lea | yes | 2 | B | | 35 | 25 | 60 | **contacted** | **nurture** |
| Tom | yes | 4 | C | | 0 | 10 | 10 | **deal** | **(none)** |
| Marie | | **2** | **B** | | **25** | 15 | **40** | new | nurture |
| Sam | yes | 4 | C | | 0 | 0 | 0 | new | |
| Ines | yes | 1 | B | | 40 | 0 | 40 | **contacted** | **nurture** |

## Ranked, C (main scenario; the same after run 1 and run 2)

| Lead | fit | tier | prio | hot | acct | contact | score | status | lane |
|---|---|---|---|---|---|---|---|---|---|
| Anna | yes | 1 | A | yes | 65 | 35 | 100 | new | nurture |
| Jonas | yes | 1 | A | | 65 | 10 | 75 | new | nurture |
| Pia | yes | 1 | A | | 65 | 0 | 65 | new | nurture |
| Lea | yes | 2 | B | | 35 | 15 | 50 | new | nurture |
| Tom | yes | 4 | C | | 0 | 0 | 0 | new | |
| Marie | | | C | | 0 | 15 | 15 | new | |
| Sam | yes | 4 | C | | 0 | 0 | 0 | new | |
| Ines | yes | 1 | B | | 40 | 0 | 40 | new | nurture |

## The nurture export (both stores: a tab on Sheets, a table and `nurture.csv` on SQLite)

A lead is listed the first run it matches the lane and is not blocked on
every lane. `score` is the score when listed (only `status` and
`do_not_contact` are refreshed). `first_listed_at` is T1 for everyone here.

V after run 2: `do_not_contact` is `yes` for any lead a cold lane's `when`
holds for, or who was contacted.

| Lead | score | status | do_not_contact | why |
|---|---|---|---|---|
| Anna | 110 | contacted | yes | matches hot-visitors and fleet-ops; contacted |
| Jonas | 75 | contacted | yes | matches fleet-ops; contacted |
| Pia | 65 | contacted | yes | same |
| Lea | 60 | contacted | yes | same |
| Ines | 40 | contacted | yes | same |
| Marie | 25 | new | no | no cold lane matches; listed in run 1 at tier 3 with score 25, and the score is not refreshed |

Not listed: Tom, Sam (tier 4).

C after run 2: Anna 100, Jonas 75, Pia 65, Lea 50, Ines 40, all `new`,
`do_not_contact` `no` (no cold lane exists). Not listed: Tom, Sam (tier 4),
Marie (tier null).

## Opt-outs

**V (both stores).** Same install and data, no visits, no reply. Before
run 1: an Apollo `email_unsubscribed` webhook for Ines; one for
nora.klein@kranlogistik.example, who is in no source yet; an Overrides row
`status unsubscribed` for Lea (written as `set-status` writes it). Before
run 2, Nora's row is added to the leads file:
`nora.klein@kranlogistik.example, Nora Klein, Procurement Manager, Kran Logistik, kranlogistik.example, 420, Germany, Series B`.

- Run 1: Anna (not hot: no demo visit), Jonas and Pia match fleet-ops and are
  pushed (3 pushes). Lea (manual unsubscribed) and Ines (webhook) are blocked
  on every lane. Nora's lead is created from the event, receiver-only, so no
  cold lane matches, and she is unsubscribed anyway.
- Run 2: Nora's CSV row merges into her receiver lead by email; she is no
  longer receiver-only, so fleet-ops' `when` holds (Kran is tier 1, A), but
  she is unsubscribed: **no push**. Run 2 pushes nothing.
- Outcomes: Ines and Nora `unsubscribed`, origin `event`; Lea `unsubscribed`,
  origin `manual`.
- Ranked: Lea, Ines, Nora have status `unsubscribed` and an empty lane.
- nurture: Anna, Jonas, Pia (`do_not_contact` yes) and Marie (tier 3 after
  enrichment, no). Lea, Ines and Nora are never listed (blocked on every lane
  the first time they matched).
- No Apollo contact is ever created for Lea, Ines or Nora.

**C (both stores).** Overrides `status unsubscribed` for Lea, and for Nora
before her row exists (the row waits; run 1 raises `override_unmatched`).
Run 2 adds Nora's row. Neither is ever listed; Nora's run-2 status is
`unsubscribed` (the waiting override applies once she appears). nurture:
Anna, Jonas, Pia, Ines.

## Polling (V on SQLite, `replies: polling`)

Fake Apollo replies: Tom, label `willing_to_meet` (→ `replied_positive`);
Ines, label `unsubscribe` (→ `unsubscribed`). The receiver also stores an
`email_replied_positive` webhook for Sam, which polling mode must ignore (it
would otherwise put Sam on demo-followup).

- Run 1 pushes: Tom to demo-followup (one HubSpot contact and one deal);
  Anna, Jonas, Pia, Lea to fleet-ops (no visits here, so Anna is not hot).
  Ines and Sam are not pushed.
- Outcomes: Tom `replied_positive`; Ines `unsubscribed`; Sam `new` with no
  reply status.

## Notes added after review

- **Change log lines.** The run logs a tier or priority change as
  `<name> <old> -> <new>` (the format is `internal/engine/score.go`,
  `buildRanked`; the docs fix only the kinds `tier_change` and
  `priority_change`). So Marie's run-2 lines end `-> 2` and `-> B`.
- **Blocked leads' reasons.** A lead blocked by an opt-out keeps its
  matching lanes in the evaluator's result, and each lane it is skipped on
  is explained in `reasons`; the reason names the status, so the text
  contains `unsubscribed`.
- **Not covered:** proving `pricing_visits` fires in the CSV-only variant
  (Route Clair's tier is null there before the detector rule is reached).

## Safety cases (vendor variant, both stores)

All three start like the opt-out case: no visits and no replies unless
listed, enrichment on (Marie tier 3). Without visits Anna is not hot, so run 1
pushes Anna, Jonas, Pia, Lea and Ines to fleet-ops unless a case says
otherwise.

### (a) Never cold-contacted twice after a lane change, then an opt-out after contact

- Run 1 (T1): fleet-ops pushes Anna, Jonas, Pia, Lea, Ines: 5 pushes, 10
  ledger rows, 5 Apollo contacts, 5 enrollments, all "Fleet ops intro".
- T1+30m: an identified `visit_demo` for Jonas, visited at T1+30m.
- Run 2 (T2): Jonas's first demo visit is inside 7 days, so `demo_visit`
  fires and he is hot; hot-visitors' `when` now holds for him. He already
  holds his one cold push (fleet-ops, done), so **nothing is pushed**: still
  10 ledger rows, 5 contacts, 5 enrollments, and Jonas is only in "Fleet ops
  intro". His Ranked row: Kran tier 1 A, account 65; contact: warm path 10 +
  hot 20 + 2 sources (his visit adds a receiver row) 10 = 40; score 105;
  status `contacted`; lane `nurture` (not pushed this run; listed there).
  Ines's export row: status `contacted`, `do_not_contact` yes.
- T2+30m: an Apollo `unsubscribed` webhook for Ines.
- Run 3 (T3 = T2 + 1h): Ines is `unsubscribed`. Her export row flips to
  status `unsubscribed`, `do_not_contact` yes, score still 40. Her Ranked
  row: status `unsubscribed`, empty lane, reasons contain `unsubscribed`.
  Nothing is pushed.

### (b) A receiver-only lead is never cold-pushed

- T0: an identified `visit_demo` for otto.berg@kranlogistik.example (in no
  source), visited at T1 − 1d, account domain kranlogistik.example. The
  visit creates his lead under the receiver; he is receiver-only.
- Run 1: Kran still tier 1 A, account 65 (leads_seen 4 pays the same 10 as
  3; Otto has no title, so ops_contacts stays 2). Otto: hot (demo visit
  within 7 days), contact 20 (no title, no warm path, one source), score 85.
  Neither cold lane's `when` holds (both need receiver_only false), so no
  ledger row, no Apollo contact, and no `receiver_only_push` problem.
  nurture (tier 1 ≤ 3) lists him: status `new`, `do_not_contact` no (no
  cold lane matches, nothing blocks him). Pushed: Anna, Jonas, Pia, Lea,
  Ines (5).
- Otto's Ranked row: email otto.berg@kranlogistik.example, no LinkedIn, no
  name, domain kranlogistik.example, fit yes, tier 1, priority A, hot yes,
  65 / 20 / 85, `new`, lane `nurture`.

### (c) One deal per company; a colleague's cold push blocked

- T0: `replied_positive` webhooks for Anna and Jonas (Kran).
- Run 1: both are `replied_positive`, which blocks cold lanes, and match
  demo-followup. Within Kran the non-cold steps run first; the first deal
  step opens Kran's deal and the second reuses it. The check before Pia's
  cold push then sees a deal step called at Kran (rule 4), so **Pia is not
  pushed** (no called ledger row for her, no Apollo contact). Lea and Ines go
  to fleet-ops. Pushed: Anna, Jonas, Lea, Ines (4).
- HubSpot: 2 contacts, 1 deal named `kranlogistik.example`, associated with
  both. Anna's contact: lane demo-followup, tier 1, priority A, score 90
  (account 65; contact: head 15 + 2 sources 10, since her reply adds a
  receiver row; not hot). Jonas's: score 85 (65 + warm path 10 + 2 sources
  10).
- Apollo: 2 contacts (Lea, Ines).
- Run 2: Anna, Jonas and Pia are `deal` (rule 4). Nothing is pushed; Pia
  still has no Apollo contact.
