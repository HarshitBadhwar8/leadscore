package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// The CEL environments (contracts section 2, "CEL variables"). A lead-level
// condition sees every variable; a company-level one (company derive blocks
// and account score rules) sees no `lead` and no `status`, so a raw
// expression reading them fails to compile instead of reading nothing.
//
// The helper functions do the text matching rules in one place: textEq, textIn
// and textContains compare trimmed and lowercased; ordEq, ordIn and ordRank also
// ignore spaces, hyphens and underscores, and ordRank gives -1 to a value not in
// the list, so unknown values sort below all.
func newEnv(company bool) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("company", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("detector", cel.MapType(cel.StringType, cel.BoolType)),
		cel.Variable("settings", cel.MapType(cel.StringType, cel.DynType)),
		cel.CrossTypeNumericComparisons(true),
		textFn("textEq", cel.StringType, func(v, s string) bool { return normText(v) == normText(s) }),
		textFn("textContains", cel.StringType, func(v, s string) bool { return strings.Contains(normText(v), normText(s)) }),
		textFn("ordEq", cel.StringType, func(v, s string) bool { return normOrdered(v) == normOrdered(s) }),
		listFn("textIn", func(v string, l []string) ref.Val {
			for _, x := range l {
				if normText(v) == normText(x) {
					return types.True
				}
			}
			return types.False
		}, cel.BoolType),
		listFn("ordIn", func(v string, l []string) ref.Val {
			for _, x := range l {
				if normOrdered(v) == normOrdered(x) {
					return types.True
				}
			}
			return types.False
		}, cel.BoolType),
		listFn("ordRank", func(v string, l []string) ref.Val {
			return types.Int(rank(v, l))
		}, cel.IntType),
	}
	if !company {
		opts = append(opts,
			cel.Variable("lead", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("status", cel.StringType),
		)
	}
	return cel.NewEnv(opts...)
}

// rank is v's position in an ordered list, or -1 when v is not in it.
func rank(v string, l []string) int {
	n := normOrdered(v)
	for i, x := range l {
		if normOrdered(x) == n {
			return i
		}
	}
	return -1
}

// textOf reads a CEL value as text: a string as is, anything else formatted.
func textOf(v ref.Val) string {
	switch x := v.Value().(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}

func textFn(name string, arg *cel.Type, f func(v, s string) bool) cel.EnvOption {
	return cel.Function(name, cel.Overload(name+"_dyn_string", []*cel.Type{cel.DynType, arg}, cel.BoolType,
		cel.BinaryBinding(func(v, s ref.Val) ref.Val {
			return types.Bool(f(textOf(v), textOf(s)))
		})))
}

func listFn(name string, f func(v string, l []string) ref.Val, result *cel.Type) cel.EnvOption {
	return cel.Function(name, cel.Overload(name+"_dyn_list", []*cel.Type{cel.DynType, cel.ListType(cel.StringType)}, result,
		cel.BinaryBinding(func(v, l ref.Val) ref.Val {
			lister, ok := l.(traits.Lister)
			if !ok {
				return types.NewErr("%s: not a list", name)
			}
			n := int(lister.Size().(types.Int))
			items := make([]string, 0, n)
			for i := 0; i < n; i++ {
				items = append(items, textOf(lister.Get(types.Int(i))))
			}
			return f(textOf(v), items)
		})))
}

// celString, celNumber, celDate and celList write CEL literals.
func celString(s string) string { return strconv.Quote(s) }

func celNumber(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

func celDate(t time.Time) string {
	return "timestamp(" + celString(t.UTC().Format(time.RFC3339Nano)) + ")"
}

func celLiteral(v any) string {
	switch x := v.(type) {
	case float64:
		return celNumber(x)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return celDate(x)
	}
	return celString(fmt.Sprint(v))
}

func celList(vs []any) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = celLiteral(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// costLimit bounds what one condition may cost for one lead, in CEL's cost
// units (about one per operation). Compile refuses a condition whose estimated
// worst case is over it, and evaluation stops one that reaches it, so a rubric
// can never stall a run (contracts section 2).
const costLimit = 1_000_000

// runCostLimit is the limit compiled into each program: costLimit, lowered
// only by a test that makes the run-time limit fire.
var runCostLimit uint64 = costLimit

// Largest sizes a run can see: a lead or company map of 1,000 entries, and a
// value in one of at most 50,000 characters (a Sheets cell).
const (
	maxEntries = 1_000
	maxText    = 50_000
)

// sizeEstimator gives CEL's cost estimate real sizes: lead and company values
// at the largest a run can see, settings at their sizes in this rubric, and
// items of a list written in the expression at its longest literal string.
type sizeEstimator struct {
	settings   map[string]setting
	literalMax uint64
}

func (e sizeEstimator) EstimateSize(n checker.AstNode) *checker.SizeEstimate {
	path := n.Path()
	if len(path) == 0 {
		return nil
	}
	size := func(max uint64) *checker.SizeEstimate { return &checker.SizeEstimate{Min: 0, Max: max} }
	switch path[0] {
	case "lead", "company", "detector":
		if len(path) == 1 {
			return size(maxEntries)
		}
		return size(maxText)
	case "settings":
		if len(path) == 1 {
			return size(uint64(len(e.settings)))
		}
		var longestList, longestText uint64
		for _, s := range e.settings {
			longestList = max(longestList, uint64(len(s.list)))
			longestText = max(longestText, uint64(len(s.scalar.text)))
			for _, item := range s.list {
				longestText = max(longestText, uint64(len(item.text)))
			}
		}
		if len(path) == 2 {
			if s, ok := e.settings[path[1]]; ok {
				if s.list != nil {
					return size(uint64(len(s.list)))
				}
				return size(uint64(len(s.scalar.text)))
			}
			return size(max(longestList, longestText))
		}
		return size(longestText)
	}
	// An item of a list written in the expression.
	return size(e.literalMax)
}

func (sizeEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// longestLiteral is the length of the longest string literal in an expression.
func longestLiteral(e celast.Expr) uint64 {
	var n uint64
	celast.PreOrderVisit(e, celast.NewExprVisitor(func(x celast.Expr) {
		if x.Kind() == celast.LiteralKind {
			if s, ok := x.AsLiteral().Value().(string); ok {
				n = max(n, uint64(len(s)))
			}
		}
	}))
	return n
}
