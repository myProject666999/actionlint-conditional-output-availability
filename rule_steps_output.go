package actionlint

import (
	"strconv"
	"strings"
)

// RuleStepsOutput is an optional rule which checks availability of values read through
// `steps.<step_id>.outputs.*` expressions. Unlike the "expression" rule, which only knows the
// static type of already visited steps, this rule collects every step ID and its source
// position before traversal and uses that index only to explain diagnostics. It never adds
// later steps to the `steps` type, so illegal forward references stay illegal.
//
// The rule is opt-in (Config.CheckStepOutput, disabled by default) because its "may be empty"
// diagnostics are conservative. When disabled, no diagnostic is emitted and the default
// "expression" rule behavior is unchanged.
//
// Limits (see docs/checks.md):
//   - General condition implication is not solved. Only a small, explicitly supported set of
//     guards is recognized.
//   - Values are not propagated across jobs and matrix combinations are not enumerated.
//   - Whether a step's shell/action actually writes a declared output at runtime is out of
//     scope. The guard only narrows the conditions under which the source step executed.
//   - When any step ID is dynamic (contains an expression), the analysis stays silent because a
//     complete index cannot be built.
type RuleStepsOutput struct {
	RuleBase
	// steps maps lower-cased static step IDs to their index entries within the current job.
	steps map[string]*stepIndexEntry
	// current is the step currently being visited. It is nil while visiting job/workflow level
	// sections, where steps output availability is not analyzed.
	current *Step
	// dynamic is true when a step ID in the current job contains an expression. In that case no
	// conclusion is drawn from the incomplete index.
	dynamic bool
	// reported maps an expression base position to the source positions at which this rule has
	// emitted diagnostics for the expression analyzed at that base. RuleExpression consults it to
	// avoid duplicating the generic undefined-property error.
	reported map[Pos]map[Pos]struct{}
	// analysisBase is the base position of the expression currently being analyzed.
	analysisBase Pos
}

// stepIndexEntry is the information collected for one statically identified step.
type stepIndexEntry struct {
	id  string
	pos *Pos
	// ordinal is the zero-based index of the step within the job's steps sequence.
	ordinal int
	ifExpr  ExprNode
	// alwaysRuns is true when the step has no "if" condition or its condition is a constant
	// expression which statically evaluates to true.
	alwaysRuns bool
	// coe is true when the step (or its job) sets "continue-on-error" to a statically true value.
	// Dynamic values are handled conservatively as if they were true.
	coe bool
}

// stepsAccessSegment is one element of a `steps....` access chain.
type stepsAccessSegment struct {
	// property is the accessed property name in lower case. Empty when dynamic is true.
	property string
	// dynamic means the element is an index access with a non-literal index (e.g. steps[x.y]).
	dynamic bool
	// node is the expression node performing this access, used for source positions.
	node ExprNode
}

// stepsAccess represents one `steps.<id>...` access found in an expression.
type stepsAccess struct {
	// stepID is the ID right after `steps`, in lower case. Empty when the first segment is a
	// dynamic index access.
	stepID string
	// segments is the chain after the `steps` variable. segments[0] is the step ID access.
	segments []stepsAccessSegment
	// node is the node performing the step ID access (dereference or index), used for positions.
	node ExprNode
}

// NewRuleStepsOutput creates new RuleStepsOutput instance.
func NewRuleStepsOutput() *RuleStepsOutput {
	return &RuleStepsOutput{
		RuleBase: RuleBase{
			name: "steps-output",
			desc: "Checks availability of steps.<step_id>.outputs values (optional, disabled by default)",
		},
	}
}

// VisitJobPre builds the step index for the job. The index is used only to explain diagnostics.
// It does not mutate the `steps` type used by the "expression" rule.
func (rule *RuleStepsOutput) VisitJobPre(job *Job) error {
	rule.steps = map[string]*stepIndexEntry{}
	rule.current = nil
	rule.dynamic = false
	rule.reported = map[Pos]map[Pos]struct{}{}

	for ordinal, s := range job.Steps {
		if s.ID == nil {
			continue
		}
		if s.ID.ContainsExpression() {
			// A dynamic ID may resolve to any name, so a complete index cannot be built.
			rule.dynamic = true
			continue
		}
		id := strings.ToLower(s.ID.Value) // Step ID is case insensitive
		if _, ok := rule.steps[id]; ok {
			// Duplicate IDs are already reported by the "id" rule. Do not make conclusions
			// about which entry a reference resolves to.
			rule.dynamic = true
			continue
		}
		entry := &stepIndexEntry{
			id:         id,
			pos:        s.ID.Pos,
			ordinal:    ordinal,
			ifExpr:     parseStepCondition(s.If),
			alwaysRuns: stepAlwaysRuns(s.If),
			coe:        stepContinueOnError(s, job),
		}
		rule.steps[id] = entry
	}

	return nil
}

// VisitJobPost resets the per-job state.
func (rule *RuleStepsOutput) VisitJobPost(_ *Job) error {
	rule.steps = nil
	rule.current = nil
	rule.dynamic = false
	rule.reported = nil
	return nil
}

// VisitStep satisfies the Pass interface. The "current step" context is driven by
// RuleExpression while it checks the step's expressions; this callback does not change state so
// the rule remains correct regardless of the order passes are registered in.
func (rule *RuleStepsOutput) VisitStep(_ *Step) error {
	return nil
}

// analyzeExpression inspects one parsed expression for steps output availability. It is called
// from RuleExpression for every expression checked while a step is being visited. baseLine and
// baseCol are the source position of the first token of the expression.
func (rule *RuleStepsOutput) analyzeExpression(expr ExprNode, baseLine, baseCol int) {
	rule.analysisBase = Pos{Line: baseLine, Col: baseCol}
	defer func() { rule.analysisBase = Pos{} }()

	if rule.current == nil || rule.dynamic || len(rule.steps) == 0 {
		return
	}

	currentID := ""
	if rule.current.ID != nil && !rule.current.ID.ContainsExpression() {
		currentID = strings.ToLower(rule.current.ID.Value)
	}
	currentEntry := rule.steps[currentID] // nil when the current step has no static ID

	for _, access := range collectStepsAccesses(expr) {
		// A dynamic first index (steps[expr]) cannot be resolved, stay conservative.
		if access.stepID == "" {
			continue
		}

		entry, known := rule.steps[access.stepID]

		switch {
		case currentEntry != nil && access.stepID == currentID:
			// Reading the step from within itself. The finding keeps the "expression" category
			// and is produced by RuleExpression via enrichment (see enrichUndefinedStepsError),
			// so nothing is emitted here to avoid duplicates.
		case !known:
			// Referenced step exists nowhere in this job. The "expression" rule already reports
			// an undefined property error, do not duplicate it.
		case currentEntry == nil:
			// The current step has no static ID so its ordinal is unknown. References cannot be
			// self-references. Run only the conditional-output analysis, whose index was built
			// before traversal and does not depend on the current step's position.
			rule.checkMaybeEmpty(access, entry, expr, baseLine, baseCol)
		case entry.ordinal >= currentEntry.ordinal:
			// The referenced step is the current one (handled above) or defined later in the
			// job, so it has not run yet. The finding keeps the "expression" category and is
			// produced via enrichment; nothing is emitted here.
		default:
			rule.checkMaybeEmpty(access, entry, expr, baseLine, baseCol)
		}
	}
}

// reportAt emits a diagnostic at the given node's position and records the position so
// RuleExpression can suppress an equivalent generic undefined-property error at the same spot.
func (rule *RuleStepsOutput) reportAt(node ExprNode, baseLine, baseCol int, format string, args ...interface{}) {
	pos := exprPos(node, baseLine, baseCol)
	rule.Errorf(pos, format, args...)
	if rule.reported == nil {
		rule.reported = map[Pos]map[Pos]struct{}{}
	}
	set, ok := rule.reported[rule.analysisBase]
	if !ok {
		set = map[Pos]struct{}{}
		rule.reported[rule.analysisBase] = set
	}
	set[*pos] = struct{}{}
}

// reportedAt reports whether this rule emitted a diagnostic at pos for the expression whose base
// position is base. RuleExpression uses it to decide whether a generic error is already covered.
func (rule *RuleStepsOutput) reportedAt(base, pos Pos) bool {
	if rule.reported == nil {
		return false
	}
	set, ok := rule.reported[base]
	if !ok {
		return false
	}
	_, ok = set[pos]
	return ok
}

// enrichUndefinedStepsError adds the referenced step's definition location to a generic
// "property is not defined" expression error caused by a steps reference. It is used for forward
// and self references, which keep the original "expression" error category but gain an
// explanatory definition position. The boolean result is false when no static index entry exists
// (for example a truly unknown step ID), in which case the original message is emitted unchanged.
func (rule *RuleStepsOutput) enrichUndefinedStepsError(err *ExprError, root ExprNode, line, col int) (string, bool) {
	if rule.dynamic || len(rule.steps) == 0 {
		return "", false
	}
	for _, access := range collectStepsAccesses(root) {
		if access.stepID == "" {
			continue
		}
		entry, ok := rule.steps[access.stepID]
		if !ok {
			continue
		}
		pos := exprPos(access.node, line, col)
		if pos.Line != line+err.Line-1 || pos.Col != col+err.Column-1 {
			continue
		}
		isSelf := rule.current != nil && rule.current.ID != nil &&
			!rule.current.ID.ContainsExpression() &&
			strings.ToLower(rule.current.ID.Value) == access.stepID
		if isSelf {
			return err.Message + ". note: step " + access.stepID +
				" references itself so its outputs are not available until the step completed" +
				"; its ID is defined at " + entry.pos.String(), true
		}
		return err.Message + ". note: step " + access.stepID +
			" is referenced before it runs so its outputs are not available yet" +
			"; its ID is defined at " + entry.pos.String(), true
	}
	return "", false
}

// checkMaybeEmpty reports an independent "may be empty" diagnostic at the concrete outputs access
// when the source step may be skipped and the reference site is not protected by a supported guard.
func (rule *RuleStepsOutput) checkMaybeEmpty(access stepsAccess, entry *stepIndexEntry, root ExprNode, baseLine, baseCol int) {
	if entry.alwaysRuns {
		return
	}
	if !access.reachesOutputs() {
		// Reading steps.<id>.outcome or steps.<id>.conclusion itself is always available.
		return
	}

	guard := rule.findGuard(access, entry, root)
	if guard != guardNone {
		return
	}

	outNode := access.outputsNode()
	rule.reportAt(
		outNode, baseLine, baseCol,
		"outputs of step %q may be empty because the step may be skipped by its \"if\" condition. the step ID is defined at %s",
		access.stepID,
		entry.pos.String(),
	)
}

// guardKind describes whether and how a steps output read is protected.
type guardKind int

const (
	guardNone guardKind = iota
	// guardSuccess means the referring "if" condition explicitly requires the source step's
	// outcome (or conclusion, when allowed) to be "success".
	guardSuccess
	// guardSameConditionFallback means the reference uses the same condition as the source step
	// and an explicit null fallback is present at the read.
	guardSameConditionFallback
)

// findGuard determines whether a supported guard protects the given access. It recognizes only
// the finite, explicitly supported guard forms and is otherwise conservative (guardNone).
func (rule *RuleStepsOutput) findGuard(access stepsAccess, entry *stepIndexEntry, root ExprNode) guardKind {
	// Guard form 1: explicit success judgment of the same step's outcome or conclusion in the
	// referring step's "if" condition.
	if rule.current.If != nil {
		if cond := parseStepCondition(rule.current.If); cond != nil && hasSuccessGuard(cond, entry.id, entry.coe) {
			return guardSuccess
		}
	}

	// Guard form 2: same condition as the source step plus an explicit null fallback at the read.
	if entry.ifExpr != nil && rule.current.If != nil {
		if cond := parseStepCondition(rule.current.If); cond != nil &&
			canonicalExpr(cond) == canonicalExpr(entry.ifExpr) &&
			hasExplicitNullFallback(root, access) {
			return guardSameConditionFallback
		}
	}

	return guardNone
}

// hasSuccessGuard reports whether expr contains, in a conjunction ("and") position, an explicit
// equality check that the given step's outcome or conclusion is the string "success".
//
// Mere mentions of the step name, bare state functions (always(), failure(), cancelled(),
// success()), and disjunctions are not accepted. When the source step may run with
// continue-on-error, a "conclusion" success check cannot prove the step's outputs were written,
// so only an "outcome" check is accepted in that case (platform semantics: conclusion reflects
// the post-continue-on-error result while outcome reflects the pre-continue-on-error result).
func hasSuccessGuard(expr ExprNode, stepID string, coe bool) bool {
	switch e := expr.(type) {
	case *LogicalOpNode:
		if e.Kind == LogicalOpNodeKindAnd {
			// Either conjunct may carry the guard.
			return hasSuccessGuard(e.Left, stepID, coe) || hasSuccessGuard(e.Right, stepID, coe)
		}
		// An "or" does not establish that the success check held when the outputs are read.
		return false
	case *CompareOpNode:
		if e.Kind != CompareOpNodeKindEq {
			return false
		}
		lhs, lok := e.Left.(*ObjectDerefNode)
		rhs, rok := e.Right.(*StringNode)
		if !lok || !rok || strings.ToLower(rhs.Value) != "success" {
			return false
		}
		prop, id, ok := derefStepProp(lhs)
		if !ok || id != stepID {
			return false
		}
		switch prop {
		case "outcome":
			return true
		case "conclusion":
			// Under continue-on-error a "success" conclusion does not guarantee the step's
			// action ran successfully and wrote its outputs.
			return !coe
		default:
			return false
		}
	default:
		return false
	}
}

// hasExplicitNullFallback reports whether the access appears as the left operand of an "or"
// expression whose right operand is an explicit null literal, e.g.
//
//	${{ steps.x.outputs.y || null }}
func hasExplicitNullFallback(root ExprNode, target stepsAccess) bool {
	found := false
	VisitExprNode(root, func(node, parent ExprNode, entering bool) {
		if found || entering {
			return
		}
		or, ok := node.(*LogicalOpNode)
		if !ok || or.Kind != LogicalOpNodeKindOr {
			return
		}
		if _, ok := or.Right.(*NullNode); !ok {
			return
		}
		if exprReadsAccess(or.Left, target) {
			found = true
		}
	})
	return found
}

// exprReadsAccess reports whether expr contains the exact steps access (same step ID and the same
// chain of properties). Dynamic index segments never match.
func exprReadsAccess(expr ExprNode, target stepsAccess) bool {
	for _, a := range collectStepsAccesses(expr) {
		if a.stepID != target.stepID || len(a.segments) != len(target.segments) {
			continue
		}
		same := true
		for i := range a.segments {
			x, y := a.segments[i], target.segments[i]
			if x.dynamic != y.dynamic || x.property != y.property {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// reachesOutputs reports whether the chain goes through an "outputs" property. Reads of
// steps.<id>.outcome and steps.<id>.conclusion themselves never trigger the "may be empty" check.
func (a stepsAccess) reachesOutputs() bool {
	for _, seg := range a.segments {
		if !seg.dynamic && seg.property == "outputs" {
			return true
		}
	}
	return false
}

// outputsNode returns the node at the "outputs" property access, used as the diagnostic position.
// It falls back to the step ID access node when the chain is shorter than expected.
func (a stepsAccess) outputsNode() ExprNode {
	// segments[0] is the step ID access held by a.node. Each further segment has its own node,
	// but a.node already points at the first dereference; the "outputs" dereference token position
	// is computed relative to the same expression base. Walk the chain nodes stored in segments.
	for _, seg := range a.segments {
		if !seg.dynamic && seg.property == "outputs" && seg.node != nil {
			return seg.node
		}
	}
	return a.node
}

// collectStepsAccesses finds every `steps.<...>` access chain in an expression.
func collectStepsAccesses(expr ExprNode) []stepsAccess {
	var ret []stepsAccess
	var walk func(n ExprNode)
	walk = func(n ExprNode) {
		switch e := n.(type) {
		case *VariableNode:
			if strings.ToLower(e.Name) == "steps" {
				ret = append(ret, stepsAccess{node: e})
			}
		case *ObjectDerefNode:
			walk(e.Receiver)
			// A bare `steps` variable is recorded as an access with no segments. Attach this
			// dereference to it when the receiver chain ends at the `steps` variable.
			if idx, ok := accessForReceiver(&ret, e.Receiver); ok {
				ret[idx].segments = append(ret[idx].segments, stepsAccessSegment{
					property: strings.ToLower(e.Property),
					node:     e,
				})
			}
		case *IndexAccessNode:
			walk(e.Operand)
			walk(e.Index)
			if idx, ok := accessForReceiver(&ret, e.Operand); ok {
				if lit, ok := e.Index.(*StringNode); ok {
					ret[idx].segments = append(ret[idx].segments, stepsAccessSegment{
						property: strings.ToLower(lit.Value),
						node:     e,
					})
				} else {
					ret[idx].segments = append(ret[idx].segments, stepsAccessSegment{
						dynamic: true,
						node:    e,
					})
				}
			}
		case *ArrayDerefNode:
			walk(e.Receiver)
		case *NotOpNode:
			walk(e.Operand)
		case *CompareOpNode:
			walk(e.Left)
			walk(e.Right)
		case *LogicalOpNode:
			walk(e.Left)
			walk(e.Right)
		case *FuncCallNode:
			for _, a := range e.Args {
				walk(a)
			}
		}
	}
	walk(expr)

	// Resolve the step ID from the first segment and skip malformed/empty chains.
	out := ret[:0]
	for _, access := range ret {
		if len(access.segments) == 0 {
			continue // bare `steps` variable with no property access
		}
		first := access.segments[0]
		if !first.dynamic {
			access.stepID = first.property
			access.node = first.node
		}
		out = append(out, access)
	}
	return out
}

// accessForReceiver returns the index in accs of the (unique) steps access whose chain ends at
// receiver. Receiver identity is matched by node pointer.
func accessForReceiver(accs *[]stepsAccess, receiver ExprNode) (int, bool) {
	found := -1
	for i, a := range *accs {
		if a.endsAt(receiver) {
			if found >= 0 {
				// Shared receiver (e.g. through an array dereference) is ambiguous; skip both.
				return -1, false
			}
			found = i
		}
	}
	return found, found >= 0
}

// endsAt reports whether the outermost node of the collected chain is node.
func (a stepsAccess) endsAt(node ExprNode) bool {
	if len(a.segments) == 0 {
		// No segments: the chain is the bare `steps` variable itself.
		v, ok := node.(*VariableNode)
		return ok && strings.ToLower(v.Name) == "steps"
	}
	return a.segments[len(a.segments)-1].node == node
}

// derefStepProp decomposes a `steps.<id>.<prop>` (or index equivalent) dereference, returning the
// final property name, the lower-cased step ID, and whether the shape matched.
func derefStepProp(n ExprNode) (property, stepID string, ok bool) {
	deref, ok := n.(*ObjectDerefNode)
	if !ok {
		return "", "", false
	}
	id, ok := stepIDOfReceiver(deref.Receiver)
	if !ok {
		return "", "", false
	}
	return strings.ToLower(deref.Property), id, true
}

// stepIDOfReceiver extracts the lower-cased step ID from the receiver of a property access,
// supporting `steps.id` (dereference) and `steps['id']` (string literal index).
func stepIDOfReceiver(expr ExprNode) (string, bool) {
	switch r := expr.(type) {
	case *ObjectDerefNode:
		if v, ok := r.Receiver.(*VariableNode); ok && strings.ToLower(v.Name) == "steps" {
			return strings.ToLower(r.Property), true
		}
	case *IndexAccessNode:
		if v, ok := r.Operand.(*VariableNode); ok && strings.ToLower(v.Name) == "steps" {
			if lit, ok := r.Index.(*StringNode); ok {
				return strings.ToLower(lit.Value), true
			}
		}
	}
	return "", false
}

// parseStepCondition parses a step's "if" condition into an expression AST, handling both the
// bare expression form and the ${{ }} placeholder form. It returns nil when the condition cannot
// be parsed, in which case callers stay conservative.
func parseStepCondition(str *String) ExprNode {
	if str == nil {
		return nil
	}
	var src string
	if str.ContainsExpression() {
		if !str.IsExpressionAssigned() {
			// Mixed content such as text around ${{ }} cannot be treated as one boolean
			// expression. Stay conservative.
			return nil
		}
		inner := str.Value
		start := strings.Index(inner, "${{")
		end := strings.LastIndex(inner, "}}")
		if start < 0 || end < 0 || end <= start {
			return nil
		}
		src = inner[start+3 : end]
	} else {
		src = str.Value
	}
	expr, err := NewExprParser().Parse(NewExprLexer(strings.TrimSpace(src) + "}}"))
	if err != nil {
		return nil
	}
	return expr
}

// stepAlwaysRuns reports whether a step unconditionally runs: no "if" condition, or a condition
// that is a constant expression statically evaluating to true.
func stepAlwaysRuns(str *String) bool {
	if str == nil {
		return true
	}
	expr := parseStepCondition(str)
	if expr == nil {
		return false
	}
	sema := NewExprSemanticsChecker(false, nil)
	if !sema.IsConstant(expr) {
		return false
	}
	v, ok := evalConstBool(expr)
	return ok && v
}

// stepContinueOnError reports whether the source step should be treated as having
// continue-on-error enabled. Static true and dynamic/unknown values are treated as enabled
// conservatively; only static false is treated as disabled. A job level setting applies too.
func stepContinueOnError(step *Step, job *Job) bool {
	if b := step.ContinueOnError; b != nil {
		if b.Expression != nil {
			return true // Unknown at analysis time
		}
		return b.Value
	}
	if b := job.ContinueOnError; b != nil {
		if b.Expression != nil {
			return true
		}
		return b.Value
	}
	return false
}

// evalConstBool statically evaluates a constant expression to its boolean truth value, following
// GitHub Actions expression coercion rules used by "if" conditions. It returns ok=false when any
// node cannot be evaluated statically.
func evalConstBool(expr ExprNode) (bool, bool) {
	switch e := expr.(type) {
	case *NullNode:
		return false, true
	case *BoolNode:
		return e.Value, true
	case *StringNode:
		return e.Value != "", true
	case *IntNode:
		return e.Value != 0, true
	case *FloatNode:
		return e.Value != 0, true
	case *NotOpNode:
		v, ok := evalConstBool(e.Operand)
		return !v, ok
	case *LogicalOpNode:
		l, lok := evalConstBool(e.Left)
		r, rok := evalConstBool(e.Right)
		if !lok || !rok {
			return false, false
		}
		if e.Kind == LogicalOpNodeKindAnd {
			return l && r, true
		}
		return l || r, true
	case *CompareOpNode:
		return evalConstCompare(e)
	default:
		return false, false
	}
}

// evalConstCompare evaluates a constant comparison expression. Only equality/inequality and
// ordering over constant operands supported by the type system are handled.
func evalConstCompare(e *CompareOpNode) (bool, bool) {
	lv, lok := evalConstScalar(e.Left)
	rv, rok := evalConstScalar(e.Right)
	if !lok || !rok {
		return false, false
	}
	c := compareScalar(lv, rv)
	switch e.Kind {
	case CompareOpNodeKindEq:
		return c == 0, true
	case CompareOpNodeKindNotEq:
		return c != 0, true
	case CompareOpNodeKindLess:
		return c < 0, true
	case CompareOpNodeKindLessEq:
		return c <= 0, true
	case CompareOpNodeKindGreater:
		return c > 0, true
	case CompareOpNodeKindGreaterEq:
		return c >= 0, true
	default:
		return false, false
	}
}

// constScalar is a normalized constant value used only for static comparison.
type constScalar struct {
	num float64
	str string
	// kind is 0 for number, 1 for string, 2 for null/bool collapsed to comparable forms.
	kind int
}

func evalConstScalar(expr ExprNode) (constScalar, bool) {
	switch e := expr.(type) {
	case *NullNode:
		return constScalar{kind: 2}, true
	case *BoolNode:
		if e.Value {
			return constScalar{kind: 2, num: 1}, true
		}
		return constScalar{kind: 2}, true
	case *StringNode:
		return constScalar{kind: 1, str: e.Value}, true
	case *IntNode:
		return constScalar{kind: 0, num: float64(e.Value)}, true
	case *FloatNode:
		return constScalar{kind: 0, num: e.Value}, true
	default:
		return constScalar{}, false
	}
}

func compareScalar(a, b constScalar) int {
	if a.kind != b.kind {
		// GitHub coerces mismatched types loosely; for constant conditions treat different kinds
		// as non-equal and order numbers before strings deterministically.
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	switch a.kind {
	case 0:
		switch {
		case a.num < b.num:
			return -1
		case a.num > b.num:
			return 1
		default:
			return 0
		}
	case 1:
		return strings.Compare(a.str, b.str)
	default:
		if a.num == b.num {
			return 0
		}
		if a.num < b.num {
			return -1
		}
		return 1
	}
}

// canonicalExpr renders an expression AST into a stable structural string, used only to decide
// whether two "if" conditions are syntactically identical. Identical source expressions parse to
// identical canonical forms.
func canonicalExpr(expr ExprNode) string {
	var b strings.Builder
	writeCanonical(&b, expr)
	return b.String()
}

func writeCanonical(b *strings.Builder, expr ExprNode) {
	switch e := expr.(type) {
	case *NullNode:
		b.WriteString("null")
	case *BoolNode:
		if e.Value {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case *IntNode:
		b.WriteString("int:")
		b.WriteString(strconv.Itoa(e.Value))
	case *FloatNode:
		b.WriteString("float:")
		b.WriteString(strconv.FormatFloat(e.Value, 'g', -1, 64))
	case *StringNode:
		b.WriteString("str:")
		b.WriteString(e.Value)
	case *VariableNode:
		b.WriteString("var:")
		b.WriteString(strings.ToLower(e.Name))
	case *ObjectDerefNode:
		b.WriteString("deref(")
		writeCanonical(b, e.Receiver)
		b.WriteString(",")
		b.WriteString(strings.ToLower(e.Property))
		b.WriteString(")")
	case *ArrayDerefNode:
		b.WriteString("arrayderef(")
		writeCanonical(b, e.Receiver)
		b.WriteString(")")
	case *IndexAccessNode:
		b.WriteString("index(")
		writeCanonical(b, e.Operand)
		b.WriteString(",")
		writeCanonical(b, e.Index)
		b.WriteString(")")
	case *NotOpNode:
		b.WriteString("!(")
		writeCanonical(b, e.Operand)
		b.WriteString(")")
	case *CompareOpNode:
		b.WriteString("cmp(")
		b.WriteString(e.Kind.String())
		b.WriteString(",")
		writeCanonical(b, e.Left)
		b.WriteString(",")
		writeCanonical(b, e.Right)
		b.WriteString(")")
	case *LogicalOpNode:
		b.WriteString("logic(")
		b.WriteString(e.Kind.String())
		b.WriteString(",")
		writeCanonical(b, e.Left)
		b.WriteString(",")
		writeCanonical(b, e.Right)
		b.WriteString(")")
	case *FuncCallNode:
		b.WriteString("call(")
		b.WriteString(strings.ToLower(e.Callee))
		for _, a := range e.Args {
			b.WriteString(",")
			writeCanonical(b, a)
		}
		b.WriteString(")")
	default:
		// Unknown node kind: make the form unique-ish so different shapes never compare equal.
		b.WriteString("unknown")
	}
}

// exprPos converts an expression node's token position into a workflow source position. It uses
// the same conversion as RuleExpression.exprError so diagnostics line up with expression
// semantics errors at the same source location.
func exprPos(node ExprNode, baseLine, baseCol int) *Pos {
	tok := node.Token()
	return convertExprLineColToPos(tok.Line, tok.Column, baseLine, baseCol)
}
