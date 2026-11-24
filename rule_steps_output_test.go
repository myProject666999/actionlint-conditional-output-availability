package actionlint

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintStepsOutputWorkflow lints the shared workflow with the optional "steps-output" rule either
// enabled or disabled and returns the collected errors as their one-line representations.
func lintStepsOutputWorkflow(t *testing.T, enabled bool) []*Error {
	t.Helper()

	dir := filepath.Join("testdata", "steps_output")
	path := filepath.Join(dir, "steps_output.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	l, err := NewLinter(io.Discard, &LinterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l.defaultConfig = &Config{CheckStepOutput: enabled}

	errs, err := l.Lint("test.yaml", b, &Project{root: dir})
	if err != nil {
		t.Fatal(err)
	}
	return errs
}

func TestRuleStepsOutputEnabled(t *testing.T) {
	errs := lintStepsOutputWorkflow(t, true)
	checkErrors(t, filepath.Join("testdata", "steps_output", "steps_output.out"), errs)
}

func TestRuleStepsOutputDisabledByDefault(t *testing.T) {
	errs := lintStepsOutputWorkflow(t, false)

	for _, err := range errs {
		if err.Kind == "steps-output" {
			t.Fatalf("steps-output diagnostic must not be reported while disabled, got %q", err.Error())
		}
	}

	// Forward and self references must still be reported by the default "expression" rule with
	// the original error category, since the optional check does not relax existing checks.
	var kinds []string
	for _, err := range errs {
		kinds = append(kinds, err.Error())
	}
	joined := strings.Join(kinds, "\n")
	if !strings.Contains(joined, `property "later" is not defined`) {
		t.Errorf("forward reference error was lost while the rule is disabled:\n%s", joined)
	}
	if !strings.Contains(joined, `property "selfy" is not defined`) {
		t.Errorf("self reference error was lost while the rule is disabled:\n%s", joined)
	}
}

// TestRuleStepsOutputDiagnosticCount pins the number of findings so an accidental change in the
// guard recognition logic is caught together with the golden file.
func TestRuleStepsOutputDiagnosticCount(t *testing.T) {
	errs := lintStepsOutputWorkflow(t, true)

	var maybeEmpty, forward, self int
	for _, err := range errs {
		switch {
		case strings.Contains(err.Message, "may be empty"):
			maybeEmpty++
		case strings.Contains(err.Message, "references itself"):
			self++
		case err.Kind == "expression" && strings.Contains(err.Message, `property "later" is not defined`):
			// Forward references keep the original "expression" error category and message;
			// the availability rule only adds the definition location instead of duplicating
			// an equivalent finding.
			forward++
		}
	}

	// Counted from the workflow: unguarded conditional reads, name-only/always/failure/success
	// guards, fallback without same condition, same condition without fallback, case variant, and
	// the continue-on-error conclusion case.
	wantMaybeEmpty := 9
	if maybeEmpty != wantMaybeEmpty {
		t.Errorf("wanted %d may-be-empty diagnostics but got %d: %v", wantMaybeEmpty, maybeEmpty, errs)
	}
	if forward != 1 {
		t.Errorf("wanted 1 forward reference but got %d: %v", forward, errs)
	}
	if self != 1 {
		t.Errorf("wanted 1 self reference but got %d: %v", self, errs)
	}
}

// TestRuleStepsOutputIgnoreMechanism ensures the existing -ignore/config filtering still applies
// to the new rule's diagnostics.
func TestRuleStepsOutputIgnoreMechanism(t *testing.T) {
	dir := filepath.Join("testdata", "steps_output")
	b, err := os.ReadFile(filepath.Join(dir, "steps_output.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	l, err := NewLinter(io.Discard, &LinterOptions{
		IgnorePatterns: []string{`may be empty`},
	})
	if err != nil {
		t.Fatal(err)
	}
	l.defaultConfig = &Config{CheckStepOutput: true}

	errs, err := l.Lint("test.yaml", b, &Project{root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range errs {
		if strings.Contains(e.Message, "may be empty") {
			t.Fatalf("ignored diagnostic was still reported: %q", e.Error())
		}
	}
}
