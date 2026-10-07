package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/merge"
	"github.com/HarshitBadhwar8/leadscore/internal/model"
	"github.com/HarshitBadhwar8/leadscore/internal/store/codec"
)

// now is the clock the Overrides writers stamp requests with.
var now = func() time.Time { return time.Now().UTC() }

// editOverrides loads the store, lets edit change the model's Overrides, and
// commits only that table. It takes no lease: Overrides is people-owned, the
// run never writes it, and the CLI writes it the same way on every store.
func editOverrides(inv *invocation, edit func(m *model.Model) (string, error)) int {
	ctx := context.Background()
	c, err := config.Load(inv.configOptions())
	if err != nil {
		return inv.fail(err)
	}
	open, ok := api.BackendFactory(c.Store.Type)
	if !ok {
		return inv.fail(fmt.Errorf("store type %q is not registered", c.Store.Type))
	}
	b, _, err := open(c.Store.Block)
	if err != nil {
		return inv.fail(fmt.Errorf("opening the store: %w", err))
	}
	if cl, ok := b.(io.Closer); ok {
		defer cl.Close()
	}
	m, err := codec.Load(ctx, b)
	if err != nil {
		return inv.fail(fmt.Errorf("loading the store: %w", err))
	}
	done, err := edit(m)
	if err != nil {
		return inv.fail(err)
	}
	writes := codec.Encode(m, model.TableOverrides)
	if len(writes) == 0 {
		fmt.Fprintln(inv.stdout, "Overrides already says this; nothing to change")
		return exitOK
	}
	if err := b.Commit(ctx, writes); err != nil {
		return inv.fail(fmt.Errorf("writing Overrides: %w", err))
	}
	fmt.Fprintln(inv.stdout, done)
	return exitOK
}

// runSetStatus: set-status <person> <status|none|resubscribe>.
func runSetStatus(inv *invocation) int {
	return editOverrides(inv, func(m *model.Model) (string, error) {
		known := isKnown(m, inv.args[0])
		key, err := merge.SetStatus(m, inv.args[0], inv.args[1], now())
		if err != nil {
			return "", err
		}
		return "Overrides: status " + inv.args[1] + " for " + key + waits(known), nil
	})
}

// runMerge: merge <person> <person> appends a same_as row.
func runMerge(inv *invocation) int {
	return editOverrides(inv, func(m *model.Model) (string, error) {
		a, b, err := merge.AddPair(m, merge.ActionSameAs, inv.args[0], inv.args[1])
		if err != nil {
			return "", err
		}
		return "Overrides: " + a + " same_as " + b + "; the next run merges them for good", nil
	})
}

// runMarkDistinct: mark-distinct <person> <person> appends a distinct row.
func runMarkDistinct(inv *invocation) int {
	return editOverrides(inv, func(m *model.Model) (string, error) {
		known := isKnown(m, inv.args[0]) && isKnown(m, inv.args[1])
		a, b, err := merge.AddPair(m, merge.ActionDistinct, inv.args[0], inv.args[1])
		if err != nil {
			return "", err
		}
		return "Overrides: " + a + " distinct from " + b + waits(known), nil
	})
}

// runRetry: retry [--lane X] [<person>] appends a retry row.
func runRetry(inv *invocation) int {
	return editOverrides(inv, func(m *model.Model) (string, error) {
		person := ""
		if len(inv.args) > 0 {
			person = inv.args[0]
		}
		known := person == "" || isKnown(m, person)
		key, err := merge.AddRetry(m, person, inv.flags["lane"], now())
		if err != nil {
			return "", err
		}
		lane := inv.flags["lane"]
		if lane == "" {
			lane = "every lane"
		}
		who := key
		if key == "*" {
			who = "every lead"
		}
		return "Overrides: retry " + lane + " for " + who + "; the next run resets its failed steps" + waits(known), nil
	})
}

func isKnown(m *model.Model, person string) bool {
	_, ok := merge.Resolve(m, person)
	return ok
}

func waits(known bool) string {
	if known {
		return ""
	}
	return " (no lead matches yet: the row waits until the person appears)"
}
