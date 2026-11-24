package actionlint

import (
	"strings"
	"testing"
)

func parseExprForTest(t *testing.T, src string) ExprNode {
	t.Helper()
	expr, err := NewExprParser().Parse(NewExprLexer(strings.TrimSpace(src) + "}}"))
	if err != nil {
		t.Fatalf("could not parse %q: %v", src, err)
	}
	return expr
}

func TestCollectStepsAccesses(t *testing.T) {
	tests := []struct {
		what   string
		src    string
		id     string
		props  []string
		output bool
	}{
		{
			what:   "simple dereference chain",
			src:    "steps.foo.outputs.bar",
			id:     "foo",
			props:  []string{"foo", "outputs", "bar"},
			output: true,
		},
		{
			what:   "index access with literal",
			src:    "steps['foo'].outputs.bar",
			id:     "foo",
			props:  []string{"foo", "outputs", "bar"},
			output: true,
		},
		{
			what:   "outcome read",
			src:    "steps.foo.outcome",
			id:     "foo",
			props:  []string{"foo", "outcome"},
			output: false,
		},
		{
			what:   "conclusion read",
			src:    "steps.foo.conclusion",
			id:     "foo",
			props:  []string{"foo", "conclusion"},
			output: false,
		},
		{
			what:   "case is normalized",
			src:    "steps.FOO.outputs.BAR",
			id:     "foo",
			props:  []string{"foo", "outputs", "bar"},
			output: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.what, func(t *testing.T) {
			accesses := collectStepsAccesses(parseExprForTest(t, tc.src))
			if len(accesses) != 1 {
				t.Fatalf("wanted 1 access but got %d: %#v", len(accesses), accesses)
			}
			a := accesses[0]
			if a.stepID != tc.id {
				t.Errorf("wanted step ID %q but got %q", tc.id, a.stepID)
			}
			if len(a.segments) != len(tc.props) {
				t.Fatalf("wanted %d segments but got %d: %#v", len(tc.props), len(a.segments), a.segments)
			}
			for i, p := range tc.props {
				if a.segments[i].property != p || a.segments[i].dynamic {
					t.Errorf("segment %d: wanted property %q but got %#v", i, p, a.segments[i])
				}
			}
			if a.reachesOutputs() != tc.output {
				t.Errorf("wanted reachesOutputs=%v but got %v", tc.output, a.reachesOutputs())
			}
		})
	}
}

func TestCollectStepsAccessesDynamicIndex(t *testing.T) {
	// A non-literal index cannot be resolved to a step ID.
	accesses := collectStepsAccesses(parseExprForTest(t, "steps[github.run_id].outputs.x"))
	if len(accesses) != 1 {
		t.Fatalf("wanted 1 access but got %d", len(accesses))
	}
	if accesses[0].stepID != "" || !accesses[0].segments[0].dynamic {
		t.Fatalf("wanted an unresolvable dynamic first segment but got %#v", accesses[0])
	}
}

func TestHasSuccessGuard(t *testing.T) {
	tests := []struct {
		what string
		cond string
		coe  bool
		want bool
	}{
		{"outcome success", "steps.a.outcome == 'success'", false, true},
		{"conclusion success without coe", "steps.a.conclusion == 'success'", false, true},
		{"conclusion success with coe", "steps.a.conclusion == 'success'", true, false},
		{"outcome success with coe", "steps.a.outcome == 'success'", true, true},
		{"different step", "steps.b.outcome == 'success'", false, false},
		{"inequality is not a guard", "steps.a.outcome != 'failure'", false, false},
		{"name only in negation", "steps.a.conclusion != 'skipped'", false, false},
		{"always function", "always()", false, false},
		{"failure function", "failure()", false, false},
		{"bare success function", "success()", false, false},
		{"cancelled function", "cancelled()", false, false},
		{"guard inside conjunction", "github.ref == 'x' && steps.a.outcome == 'success'", false, true},
		{"guard inside disjunction is not sufficient", "steps.a.outcome == 'success' || always()", false, false},
		{"equality against non-success", "steps.a.outcome == 'failure'", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.what, func(t *testing.T) {
			got := hasSuccessGuard(parseExprForTest(t, tc.cond), "a", tc.coe)
			if got != tc.want {
				t.Fatalf("wanted %v but got %v for %q (coe=%v)", tc.want, got, tc.cond, tc.coe)
			}
		})
	}
}

func TestExplicitNullFallback(t *testing.T) {
	target := stepsAccess{
		stepID: "a",
		segments: []stepsAccessSegment{
			{property: "a", node: &ObjectDerefNode{}},
			{property: "outputs", node: &ObjectDerefNode{}},
			{property: "x", node: &ObjectDerefNode{}},
		},
	}

	withFallback := parseExprForTest(t, "steps.a.outputs.x || null")
	if !hasExplicitNullFallback(withFallback, target) {
		t.Error("null fallback should be recognized")
	}

	withoutFallback := parseExprForTest(t, "steps.a.outputs.x")
	if hasExplicitNullFallback(withoutFallback, target) {
		t.Error("plain read should not be treated as having a fallback")
	}

	nonNullFallback := parseExprForTest(t, "steps.a.outputs.x || 'default'")
	if hasExplicitNullFallback(nonNullFallback, target) {
		t.Error("a non-null fallback is not an explicit null fallback")
	}

	otherAccess := parseExprForTest(t, "steps.b.outputs.y || null")
	if hasExplicitNullFallback(otherAccess, target) {
		t.Error("fallback for a different access must not match")
	}
}

func TestStepAlwaysRuns(t *testing.T) {
	tests := []struct {
		what string
		cond string
		want bool
	}{
		{"no condition", "", true},
		{"constant true", "${{ true }}", true},
		{"constant false", "${{ false }}", false},
		{"constant true expression without placeholder", "true", true},
		{"constant compound", "${{ true && !false }}", true},
		{"dynamic condition", "${{ github.event_name == 'push' }}", false},
	}
	for _, tc := range tests {
		t.Run(tc.what, func(t *testing.T) {
			var str *String
			if tc.cond != "" {
				str = &String{Value: tc.cond, Pos: &Pos{Line: 1, Col: 1}}
			}
			if got := stepAlwaysRuns(str); got != tc.want {
				t.Fatalf("wanted %v but got %v for %q", tc.want, got, tc.cond)
			}
		})
	}
}

func TestCanonicalExprEquality(t *testing.T) {
	a := parseExprForTest(t, "github.event_name == 'push'")
	b := parseExprForTest(t, "github.event_name == 'push'")
	c := parseExprForTest(t, "github.event_name == 'pull_request'")

	if canonicalExpr(a) != canonicalExpr(b) {
		t.Error("identical expressions must have identical canonical forms")
	}
	if canonicalExpr(a) == canonicalExpr(c) {
		t.Error("different expressions must not share a canonical form")
	}
}
