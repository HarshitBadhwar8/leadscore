package check

import (
	"context"

	"github.com/HarshitBadhwar8/leadscore/internal/rules"
)

func init() { Register(rubric{}) }

// rubric fails when the rubric does not compile (section 10). S2 owns this
// compile part; S10a adds the field part (Fields() against the loaded columns).
type rubric struct{}

func (rubric) Name() string { return "rubric" }
func (rubric) InRun() bool  { return true }

func (rubric) Run(_ context.Context, env Env) []Problem {
	if env.Config == nil {
		return nil
	}
	where := env.Config.RubricPath
	if env.Config.Bundle {
		where = "the rubric in " + env.Config.Path
	}
	text, err := env.Config.Rubric()
	if err == nil {
		_, err = rules.Compile(text)
	}
	if err != nil {
		return []Problem{{
			Key:     "rubric_invalid:compile",
			Message: where + ": " + err.Error(),
			Fix:     "fix the file (leadscore rules check <file> lists every error)",
		}}
	}
	return nil
}
