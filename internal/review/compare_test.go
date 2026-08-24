package review

import (
	"testing"
)

func TestParseModelSpec(t *testing.T) {
	tests := []struct {
		spec     string
		provider string
		model    string
		wantErr  bool
	}{
		{"anthropic:claude-3-5-sonnet", "anthropic", "claude-3-5-sonnet", false},
		{"openai:gpt-4", "openai", "gpt-4", false},
		{"gemini:gemini-pro", "gemini", "gemini-pro", false},
		{"invalid", "", "", true},
		{":model", "", "", true},
		{"provider:", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		p, m, err := parseModelSpec(tt.spec)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseModelSpec(%q) expected error", tt.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseModelSpec(%q) unexpected error: %v", tt.spec, err)
			continue
		}
		if p != tt.provider || m != tt.model {
			t.Errorf("parseModelSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, p, m, tt.provider, tt.model)
		}
	}
}

func TestFuzzyMatch_SameFile_OverlappingLines_SameCategory_SharedWords(t *testing.T) {
	a := Finding{
		Category: CategoryBug,
		Title:    "Null pointer dereference",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 10, End: 15}},
		},
	}
	b := Finding{
		Category: CategoryBug,
		Title:    "Potential null pointer issue",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 12, End: 18}},
		},
	}
	if !fuzzyMatch(a, b) {
		t.Error("Expected fuzzy match: same file, overlapping lines, same category, shared word 'pointer'")
	}
}

func TestFuzzyMatch_SameCategory_NoSharedWords(t *testing.T) {
	a := Finding{
		Category: CategoryBug,
		Title:    "Memory leak detected",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 10, End: 15}},
		},
	}
	b := Finding{
		Category: CategoryBug,
		Title:    "Unhandled error return",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 12, End: 18}},
		},
	}
	if fuzzyMatch(a, b) {
		t.Error("Should not match: same category but no shared title words")
	}
}

func TestFuzzyMatch_DifferentFiles(t *testing.T) {
	a := Finding{
		Category: CategoryBug,
		Title:    "Bug A",
		Locations: []Location{
			{Path: "a.go", Lines: LineRange{Start: 10, End: 15}},
		},
	}
	b := Finding{
		Category: CategoryBug,
		Title:    "Bug A",
		Locations: []Location{
			{Path: "b.go", Lines: LineRange{Start: 10, End: 15}},
		},
	}
	if fuzzyMatch(a, b) {
		t.Error("Should not match findings from different files")
	}
}

func TestFuzzyMatch_NonOverlappingLines(t *testing.T) {
	a := Finding{
		Category: CategoryBug,
		Title:    "Bug",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 1, End: 5}},
		},
	}
	b := Finding{
		Category: CategoryBug,
		Title:    "Bug",
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 50, End: 55}},
		},
	}
	if fuzzyMatch(a, b) {
		t.Error("Should not match findings with non-overlapping lines")
	}
}

func TestFuzzyMatch_SimilarTitles(t *testing.T) {
	a := Finding{
		Category: CategorySecurity,
		Title:    "SQL injection vulnerability",
		Locations: []Location{
			{Path: "db.go", Lines: LineRange{Start: 20, End: 25}},
		},
	}
	b := Finding{
		Category: CategoryBug, // different category
		Title:    "SQL injection risk",
		Locations: []Location{
			{Path: "db.go", Lines: LineRange{Start: 22, End: 28}},
		},
	}
	if !fuzzyMatch(a, b) {
		t.Error("Expected fuzzy match: same file, overlapping lines, similar titles")
	}
}

func TestTitleSimilar(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"Null pointer", "Null pointer", true},
		{"null pointer", "Null Pointer", true},
		{"SQL injection vulnerability", "SQL injection risk", true},
		{"Missing error handling", "Error handling is absent", true},
		{"Bug in auth", "Performance issue in database", false},
		{"", "", true},    // both empty, exact match
		{"foo", "", true}, // empty is substring of anything
	}
	for _, tt := range tests {
		got := titleSimilar(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("titleSimilar(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLinesOverlap(t *testing.T) {
	tests := []struct {
		a, b Finding
		want bool
	}{
		{
			Finding{Locations: []Location{{Lines: LineRange{10, 20}}}},
			Finding{Locations: []Location{{Lines: LineRange{15, 25}}}},
			true,
		},
		{
			Finding{Locations: []Location{{Lines: LineRange{10, 20}}}},
			Finding{Locations: []Location{{Lines: LineRange{20, 25}}}},
			true, // touching at boundary
		},
		{
			Finding{Locations: []Location{{Lines: LineRange{10, 20}}}},
			Finding{Locations: []Location{{Lines: LineRange{21, 25}}}},
			false,
		},
		{
			Finding{Locations: []Location{{Lines: LineRange{10, 20}}}},
			Finding{Locations: []Location{{Lines: LineRange{5, 12}}}},
			true,
		},
	}
	for i, tt := range tests {
		got := linesOverlap(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("test %d: linesOverlap = %v, want %v", i, got, tt.want)
		}
	}
}

func TestMergeResults_Empty(t *testing.T) {
	cr := mergeResults(nil, 0)
	if cr == nil {
		t.Fatal("mergeResults returned nil")
	}
	if len(cr.Consensus) != 0 {
		t.Errorf("Consensus = %d, want 0", len(cr.Consensus))
	}
	if len(cr.All) != 0 {
		t.Errorf("All = %d, want 0", len(cr.All))
	}
}

func TestMergeResults_ConsensusAndUnique(t *testing.T) {
	// Two models: both find the same bug in main.go, model A also finds a unique style issue
	sharedFinding := Finding{
		ID:       "shared1",
		Category: CategoryBug,
		Title:    "Null pointer dereference",
		Severity: SeverityHigh,
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 10, End: 15}},
		},
	}

	uniqueFinding := Finding{
		ID:       "unique1",
		Category: CategoryStyle,
		Title:    "Long function name",
		Severity: SeverityLow,
		Locations: []Location{
			{Path: "util.go", Lines: LineRange{Start: 1, End: 5}},
		},
	}

	// Model B's version of the shared finding (different ID, but matches by fuzzy match)
	sharedFindingB := Finding{
		ID:       "shared2",
		Category: CategoryBug,
		Title:    "Potential nil pointer",
		Severity: SeverityHigh,
		Locations: []Location{
			{Path: "main.go", Lines: LineRange{Start: 11, End: 16}},
		},
	}

	results := []compareModelResult{
		{label: "anthropic:claude", findings: []Finding{sharedFinding, uniqueFinding}},
		{label: "openai:gpt-4", findings: []Finding{sharedFindingB}},
	}

	cr := mergeResults(results, 1000)

	// Both shared findings have different IDs so both appear in consensus
	if len(cr.Consensus) != 2 {
		t.Errorf("Consensus = %d, want 2", len(cr.Consensus))
	}

	// The unique finding from model A should be in Unique
	uniqueA := cr.Unique["anthropic:claude"]
	if len(uniqueA) != 1 {
		t.Errorf("Unique[anthropic:claude] = %d, want 1", len(uniqueA))
	}

	// Model B has no unique findings (its only finding matched)
	uniqueB := cr.Unique["openai:gpt-4"]
	if len(uniqueB) != 0 {
		t.Errorf("Unique[openai:gpt-4] = %d, want 0", len(uniqueB))
	}

	// Total: 2 consensus + 1 unique
	if len(cr.All) != 3 {
		t.Errorf("All = %d, want 3", len(cr.All))
	}

	if cr.LLMMs != 1000 {
		t.Errorf("LLMMs = %d, want 1000", cr.LLMMs)
	}
}

func TestFindingLines_NoLocations(t *testing.T) {
	f := Finding{Title: "No locations"}
	lr := findingLines(f)
	if lr.Start != 0 || lr.End != 0 {
		t.Errorf("findingLines with no locations should return zero LineRange, got %+v", lr)
	}
}

func TestAnyTitleWordOverlap_EmptyTitles(t *testing.T) {
	if anyTitleWordOverlap("", "hello world") {
		t.Error("Empty first title should not overlap")
	}
	if anyTitleWordOverlap("hello world", "") {
		t.Error("Empty second title should not overlap")
	}
	if anyTitleWordOverlap("", "") {
		t.Error("Two empty titles should not overlap")
	}
}

func TestAnyTitleWordOverlap_Positive(t *testing.T) {
	if !anyTitleWordOverlap("SQL injection", "SQL vulnerability") {
		t.Error("Titles sharing 'SQL' should overlap")
	}
}

func TestFuzzyMatch_NoLocations(t *testing.T) {
	a := Finding{Category: CategoryBug, Title: "Bug"}
	b := Finding{Category: CategoryBug, Title: "Bug"}
	// Same path (both empty), lines overlap (both 0-0), same category, shared words
	if !fuzzyMatch(a, b) {
		t.Error("Findings with no locations should match when title/category are the same")
	}
}

func TestTitleSimilar_EmptyWords(t *testing.T) {
	// titleSimilar trims spaces, so "   " becomes "" which is a substring of anything.
	// This is the expected behavior — an empty title is contained in any string.
	if !titleSimilar("   ", "word") {
		t.Error("Whitespace-only title (trimmed to empty) is a substring of any title")
	}
}

func TestMergeResults_AllUnique(t *testing.T) {
	// Two models find completely different things
	results := []compareModelResult{
		{
			label: "model-a",
			findings: []Finding{
				{
					ID:       "a1",
					Category: CategoryBug,
					Title:    "Bug in auth",
					Locations: []Location{
						{Path: "auth.go", Lines: LineRange{Start: 1, End: 5}},
					},
				},
			},
		},
		{
			label: "model-b",
			findings: []Finding{
				{
					ID:       "b1",
					Category: CategoryPerformance,
					Title:    "Slow database query",
					Locations: []Location{
						{Path: "db.go", Lines: LineRange{Start: 100, End: 110}},
					},
				},
			},
		},
	}

	cr := mergeResults(results, 500)

	if len(cr.Consensus) != 0 {
		t.Errorf("Consensus = %d, want 0", len(cr.Consensus))
	}
	if len(cr.All) != 2 {
		t.Errorf("All = %d, want 2", len(cr.All))
	}
	if len(cr.Unique["model-a"]) != 1 {
		t.Errorf("Unique[model-a] = %d, want 1", len(cr.Unique["model-a"]))
	}
	if len(cr.Unique["model-b"]) != 1 {
		t.Errorf("Unique[model-b] = %d, want 1", len(cr.Unique["model-b"]))
	}
}
