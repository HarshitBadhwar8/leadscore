package engine

import (
	"fmt"
	"math"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
)

// cursorKey is the State key of a source's cursor.
func cursorKey(sourceID string) string { return "cursor:" + sourceID }

// fetched is one source's output this run.
type fetched struct {
	src       config.Source
	rows      []merge.Normalized
	next      api.Cursor
	hasEvents bool
}

// ingest is step 3's source part: every source is fetched, its rows
// normalized, and up to chunk rows (whole row groups, across sources in
// config order) merged. Source events are never chunked; their keys are
// normalized once here and handed to Intake in Run.SourceEvents. A source
// whose rows were all taken this run, and whose events reached Intake, gets
// its cursor saved in phase 1 (saveCursors); rows
// left over are the backlog, which blocks pushing (an unread opt-out or an
// unmerged duplicate may sit in it). A dry run merges every row.
func (x *exec) ingest(chunk int) {
	r := x.run
	aliases := r.Rubric.Aliases()
	seenHeader := map[string]bool{}
	var all []fetched
	for _, src := range x.cfg.Sources {
		rows, events, next, err := x.fetch(src)
		if err != nil {
			x.sourceFailed(src, err)
			continue
		}
		f := fetched{src: src, next: next, hasEvents: len(events) > 0}
		for _, row := range rows {
			row.SourceID = src.ID // a row always counts for the source that returned it
			for _, h := range row.Headers {
				if !seenHeader[h] {
					seenHeader[h] = true
					x.columns = append(x.columns, h)
				}
			}
			f.rows = append(f.rows, merge.Normalize(row, aliases))
		}
		for _, e := range events {
			// Always the source's own id: a source must never pose as the
			// receiver, polling or a lookup, whose events carry opt-outs.
			e.Origin = src.ID
			r.SourceEvents = append(r.SourceEvents, merge.NormalizeEventKeys(e))
		}
		all = append(all, f)
	}

	budget := chunk
	if r.DryRun {
		budget = math.MaxInt // a dry run shows every row
	}
	var take []merge.Normalized
	taking := true
	for _, f := range all {
		complete := true
		for _, g := range merge.Pending(r.Model, f.rows) {
			// A row group is never split: the first group is taken whole even
			// when it alone is over the budget.
			if taking && (x.merged == 0 || x.merged+len(g.Rows) <= budget) {
				take = append(take, g.Rows...)
				x.merged += len(g.Rows)
				continue
			}
			taking, complete = false, false
			x.backlog += len(g.Rows)
		}
		if complete {
			x.cursors = append(x.cursors, cursorSet{source: f.src.ID, next: f.next, hasEvents: f.hasEvents})
		}
	}
	merge.Apply(r.Model, take, merge.ApplyCtx{Now: r.Now(), RunID: r.ID, Sources: x.cfg.Sources, Aliases: aliases})

	if x.backlog > 0 {
		x.noPush(fmt.Sprintf("%d input row(s) are not merged yet", x.backlog))
		x.problem("ingest_backlog", fmt.Sprintf("%d input row(s) wait for later runs (%d per run); pushing waits until all are merged",
			x.backlog, chunk), "nothing to do: the backlog clears over the next runs; raise ingest_chunk_rows to clear it faster", true)
	}
}

// fetch builds a source and reads it from its saved cursor.
func (x *exec) fetch(src config.Source) ([]api.InputRow, []api.Event, api.Cursor, error) {
	f, ok := api.SourceFactory(src.Type)
	if !ok {
		return nil, nil, "", fmt.Errorf("source type %q is not registered in this build", src.Type)
	}
	s, err := f(src.Block)
	if err != nil {
		return nil, nil, "", err
	}
	cursor := api.Cursor(x.run.Model.StateValue(cursorKey(src.ID)))
	return s.Fetch(x.run.Ctx, cursor)
}

// sourceFailed handles a source that could not be read (the run's rule
// for hook errors): its rows and events are skipped this run and its
// cursor is kept, the run is unhealthy and goes on, and pushing waits, since
// the skipped rows may hold an unmerged duplicate and the skipped events an
// opt-out.
func (x *exec) sourceFailed(src config.Source, err error) {
	x.problem("source_failed:"+src.ID,
		fmt.Sprintf("source %s could not be read, so its rows and events were skipped this run and its cursor kept: %v", src.ID, err),
		"fix the source as the message says; the next run reads it again", false)
	x.noPush("source " + src.ID + " could not be read")
	x.log("error", "source_failed", "", fmt.Sprintf("source %s could not be read: %v", src.ID, err))
}

// saveCursors puts in the model the cursors of sources whose rows were all
// taken: those with no events (withEvents false), or, once Intake took the
// events without error, those with events (withEvents true). A source whose
// events never reached Intake keeps its cursor, so the next run reads them.
func (x *exec) saveCursors(withEvents bool) {
	m := x.run.Model
	for _, c := range x.cursors {
		if c.hasEvents == withEvents && m.StateValue(cursorKey(c.source)) != string(c.next) {
			m.SetState(cursorKey(c.source), string(c.next))
		}
	}
}
