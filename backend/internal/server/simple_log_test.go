package server

import "testing"

// The command's output and the server's progress lines must not compete for the
// same bytes. A deployment script that prints an image load or a build log can
// reach the cap on its own, and if that also silenced the server's lines the
// stored log would end mid-output with no "exit=" line and no record of whether
// replication and the application deployment ran.
func TestLogBudgetKeepsAReserveForServerLines(t *testing.T) {
	budget := newLogBudget()
	allowed, exhausted := budget.take("stdout", maxSimpleRunLogBytes)
	if allowed != maxSimpleRunLogBytes || !exhausted {
		t.Fatalf("the command must be able to use its whole budget, got %d exhausted=%v", allowed, exhausted)
	}
	if allowed, _ := budget.take("stderr", 16); allowed != 0 {
		t.Fatalf("command output past the cap must be dropped, got %d", allowed)
	}
	if allowed, _ := budget.take(streamSystem, 64); allowed != 64 {
		t.Fatalf("a server line must still be stored after the command filled the cap, got %d", allowed)
	}
}

// The notice that later output is dropped is written once, on the payload that
// uses up the last of the command budget, so a long run does not repeat it on
// every line it discards afterwards.
func TestLogBudgetReportsExhaustionOnce(t *testing.T) {
	budget := logBudget{command: 10, system: 10}
	allowed, exhausted := budget.take("stdout", 4)
	if allowed != 4 || exhausted {
		t.Fatalf("a payload within the budget must not report exhaustion, got %d %v", allowed, exhausted)
	}
	// The payload that crosses the cap is stored up to the cap and reports it.
	allowed, exhausted = budget.take("stdout", 9)
	if allowed != 6 || !exhausted {
		t.Fatalf("the crossing payload must be truncated and report exhaustion, got %d %v", allowed, exhausted)
	}
	if allowed, exhausted = budget.take("stdout", 3); allowed != 0 || exhausted {
		t.Fatalf("later output must be dropped silently, got %d %v", allowed, exhausted)
	}
}

// The reserve is a bound too: a stage that logged pathologically much cannot
// grow the run log without limit, and filling it never claims the command cap
// was reached.
func TestLogBudgetBoundsTheReserveWithoutReportingExhaustion(t *testing.T) {
	budget := logBudget{command: 10, system: 5}
	allowed, exhausted := budget.take(streamSystem, 12)
	if allowed != 5 || exhausted {
		t.Fatalf("a server line must be truncated to the reserve, got %d %v", allowed, exhausted)
	}
	if allowed, _ := budget.take(streamSystem, 1); allowed != 0 {
		t.Fatalf("the reserve must not be exceeded, got %d", allowed)
	}
	if allowed, _ := budget.take("stdout", 10); allowed != 10 {
		t.Fatal("a full reserve must not consume the command budget")
	}
}

// An empty payload is not a log row, and must not be mistaken for the one that
// exhausts the budget.
func TestLogBudgetIgnoresEmptyPayloads(t *testing.T) {
	budget := logBudget{command: 0, system: 4}
	if allowed, exhausted := budget.take("stdout", 0); allowed != 0 || exhausted {
		t.Fatalf("an empty payload must be a no-op, got %d %v", allowed, exhausted)
	}
	if budget.system != 4 {
		t.Fatalf("an empty payload must not charge a budget, system = %d", budget.system)
	}
}
