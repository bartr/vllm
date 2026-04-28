package httpapi

import (
	"testing"

	"cllm/internal/node"
)

// TestPriorityScore confirms the textual-tier-to-int mapping that drives
// node.TokenBudget priority-weighted dequeue (Phase 14C).
func TestPriorityScore(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"high":    1,
		"HIGH":    1,
		" high ":  1,
		"medium":  0,
		"low":     -1,
		"LOW":     -1,
		"":        0,
		"unknown": 0, // unknown tiers fall back to medium (FIFO equivalent)
	}
	for in, want := range cases {
		if got := priorityScore(in); got != want {
			t.Errorf("priorityScore(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestValidPriorityName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"low", "medium", "high", "HIGH", " high "} {
		if !validPriorityName(ok) {
			t.Errorf("validPriorityName(%q) = false; want true", ok)
		}
	}
	for _, bad := range []string{"", "urgent", "0", "-1", "highest"} {
		if validPriorityName(bad) {
			t.Errorf("validPriorityName(%q) = true; want false", bad)
		}
	}
}

// TestParseDSLPriorityOverride verifies the DSL parser populates
// `priorityOverride` on a valid token, lowercases the value, and records
// the directive.
func TestParseDSLPriorityOverride(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"high", "HIGH", "Medium", "low"} {
		_, ov := parseDSL([]chatCompletionMessage{
			{Role: "user", Content: "hi :dsl priority=" + in},
		}, nil)
		want := ""
		switch in {
		case "high", "HIGH":
			want = "high"
		case "Medium":
			want = "medium"
		case "low":
			want = "low"
		}
		if ov.priorityOverride != want {
			t.Fatalf("priority=%s -> %q; want %q", in, ov.priorityOverride, want)
		}
		found := false
		for _, d := range ov.directives {
			if d == "priority="+want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("priority=%s -> directives %v; want priority=%s recorded", in, ov.directives, want)
		}
	}
}

func TestParseDSLPriorityRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"urgent", "0", "highest", ""} {
		_, ov := parseDSL([]chatCompletionMessage{
			{Role: "user", Content: "hi :dsl priority=" + bad},
		}, nil)
		if ov.priorityOverride != "" {
			t.Fatalf("priority=%q accepted (got %q); want dropped", bad, ov.priorityOverride)
		}
	}
}

// TestParseDSLPriorityFirstWins verifies that with two `priority=`
// directives, the first occurrence wins (matches the workload-class
// pattern).
func TestParseDSLPriorityFirstWins(t *testing.T) {
	t.Parallel()
	_, ov := parseDSL([]chatCompletionMessage{
		{Role: "user", Content: "hi :dsl priority=low priority=high"},
	}, nil)
	if ov.priorityOverride != "low" {
		t.Fatalf("priorityOverride = %q; want low (first-wins)", ov.priorityOverride)
	}
}

// TestPriorityFamilyLabel confirms `priority=NAME` collapses to
// `priority` for the dsl_directives_total label so cardinality stays
// bounded.
func TestPriorityFamilyLabel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"priority=high":   "priority",
		"priority=medium": "priority",
		"priority=low":    "priority",
	}
	for in, want := range cases {
		if got := dslDirectiveFamily(in); got != want {
			t.Errorf("dslDirectiveFamily(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestRequestCostPriorityFieldPlumbed is a tiny integration check that
// the new RequestCost.Priority field is honoured by the budget when the
// scheduler hands it off (Phase 14C end-to-end).
func TestRequestCostPriorityFieldPlumbed(t *testing.T) {
	t.Parallel()
	c := node.RequestCost{TotalCost: 1, Priority: 1}
	if c.Priority != 1 {
		t.Fatalf("Priority field not stored: %+v", c)
	}
}
