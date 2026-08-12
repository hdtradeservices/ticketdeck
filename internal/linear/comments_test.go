package linear

import (
	"testing"
	"time"
)

func TestFindSectionMatchesMarkerAndHeading(t *testing.T) {
	now := time.Now()
	cs := []Comment{
		{ID: "1", Body: "Triage → ZEN-9: routing to /investigate", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "2", Body: "## Investigation summary\n\nold, unmarked", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "3", Body: "## Investigation summary\n\nnewer\n\n<!-- investigate-summary -->", CreatedAt: now.Add(-time.Hour)},
		{ID: "4", Body: "## Implementation plan\n\nsteps\n\n<!-- implementation-plan -->", CreatedAt: now},
	}

	got, ok := FindSection(cs, SectionInvestigation)
	if !ok || got.ID != "3" {
		t.Errorf("investigation should resolve to the newest match, got %q (found %v)", got.ID, ok)
	}
	got, ok = FindSection(cs, SectionPlan)
	if !ok || got.ID != "4" {
		t.Errorf("plan should resolve to the plan comment, got %q (found %v)", got.ID, ok)
	}
	if _, ok := FindSection(cs[:1], SectionPlan); ok {
		t.Error("a ticket with only a triage comment has no plan")
	}
	// The plan comment quotes the investigation heading in its "Based on" line;
	// that must not make it read as the investigation.
	quoting := []Comment{{ID: "5", Body: `## Implementation plan

**Based on:** the "Investigation summary" comment

<!-- implementation-plan -->`}}
	if _, ok := FindSection(quoting, SectionInvestigation); ok {
		t.Error("a plan quoting the investigation heading inline is not an investigation")
	}
}

func TestStripMarkers(t *testing.T) {
	got := StripMarkers("## Implementation plan\n\nstep one\n\n<!-- implementation-plan -->\n")
	want := "## Implementation plan\n\nstep one"
	if got != want {
		t.Errorf("StripMarkers = %q, want %q", got, want)
	}
}

func TestSectionLabel(t *testing.T) {
	for s, want := range map[Section]string{
		SectionDescription:   "description",
		SectionInvestigation: "investigation",
		SectionPlan:          "plan",
	} {
		if got := s.Label(); got != want {
			t.Errorf("Section(%d).Label() = %q, want %q", s, got, want)
		}
	}
}
