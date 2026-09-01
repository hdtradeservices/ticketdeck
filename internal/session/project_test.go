package session

import (
	"strings"
	"testing"
)

func TestProjectSystemPromptNamesTheProjectNotTheKey(t *testing.T) {
	tk := Ticket{
		Key: "proj-de76cf88eccc", Title: "Listing Splits", URL: "https://linear.app/z/project/listing-splits-de76cf88eccc",
		Status: "In Progress", PrioLabel: "High", Team: "ZEN", Project: true,
		Context: "Progress: 20% of scope complete. My open tickets in it: ZEN-4356.",
	}
	got := SystemPrompt(tk)
	// The synthetic key is an implementation detail; telling the model that is
	// its subject would be actively misleading.
	if strings.Contains(got, "proj-de76cf88eccc") {
		t.Errorf("project prompt leaked the session key: %q", got)
	}
	for _, want := range []string{"Linear project", `"Listing Splits"`, "In Progress", "ZEN", "ZEN-4356", "this project"} {
		if !strings.Contains(got, want) {
			t.Errorf("project prompt missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "Linear ticket") {
		t.Errorf("a project must not be described as a ticket: %q", got)
	}

	// A ticket keeps the prompt it always had.
	if issue := SystemPrompt(Ticket{Key: "ZEN-1", Title: "fix it"}); !strings.Contains(issue, "Linear ticket ZEN-1") {
		t.Errorf("ticket prompt regressed: %q", issue)
	}
}

func TestProjectTabLabelDropsTheKey(t *testing.T) {
	if got := TabLabel(Ticket{Key: "proj-de76cf88eccc", Title: "Listing Splits", Project: true}); got != "Listing Splits" {
		t.Errorf("project tab should be titled by name, got %q", got)
	}
	if got := TabLabel(Ticket{Key: "ZEN-1", Title: "fix it"}); got != "ZEN-1  fix it" {
		t.Errorf("ticket tab label regressed: %q", got)
	}
}

// A project's session id comes from its key, so it can never collide with a
// ticket's — two units of work sharing one transcript is unrecoverable.
func TestProjectSessionIDIsDistinct(t *testing.T) {
	if DeterministicID("proj-s1") == DeterministicID("ZEN-1") {
		t.Error("project and ticket session ids collided")
	}
}
