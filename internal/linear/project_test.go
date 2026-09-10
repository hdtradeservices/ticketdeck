package linear

import (
	"slices"
	"testing"
	"time"
)

func TestProjectKeyIsStableAndNamespaced(t *testing.T) {
	p := Project{Name: "Listing splits", SlugID: "de76cf88eccc"}
	if got := p.Key(); got != "proj-de76cf88eccc" {
		t.Errorf("Key() = %q", got)
	}
	// A rename must not move the session: the key comes from the slug alone.
	renamed := Project{Name: "Something else entirely", SlugID: "de76cf88eccc"}
	if renamed.Key() != p.Key() {
		t.Error("renaming a project changed its session key")
	}
	if !IsProjectKey(p.Key()) {
		t.Error("a project key should be recognised as one")
	}
	if IsProjectKey("ZEN-3309") {
		t.Error("a ticket key must never read as a project key")
	}
}

func TestProgressPctNeverRoundsUnfinishedWorkTo100(t *testing.T) {
	for _, tc := range []struct {
		frac float64
		want int
	}{
		{0, 0},
		{0.204, 20},
		{0.955, 96},
		{0.999, 99}, // one ticket left — must not read as complete
		{1, 100},
		{1.4, 100}, // clamped
		{-0.2, 0},  // clamped
	} {
		if got := ProgressPct(tc.frac); got != tc.want {
			t.Errorf("ProgressPct(%v) = %d, want %d", tc.frac, got, tc.want)
		}
	}
}

func TestAugmentProjectsAttachesIssuesAndDiscoversStrangers(t *testing.T) {
	mine := []Project{{ID: "p1", Name: "Mine", SlugID: "s1", Mine: true, StatusType: "started"}}
	issues := []Issue{
		{Identifier: "ZEN-1", ProjectID: "p1", ProjectName: "Mine"},
		{Identifier: "ZEN-2", ProjectID: "p1", ProjectName: "Mine"},
		// Someone else's project, holding a ticket of mine.
		{Identifier: "ONB-9", ProjectID: "p9", ProjectName: "Theirs", ProjectSlugID: "s9", ProjectStatus: "In Progress", ProjectStatusType: "started"},
		{Identifier: "ZEN-3"}, // no project at all
	}
	got := AugmentProjects(mine, issues)
	if len(got) != 2 {
		t.Fatalf("want 2 projects (mine + the discovered one), got %d", len(got))
	}
	byID := map[string]Project{}
	for _, p := range got {
		byID[p.ID] = p
	}
	if n := len(byID["p1"].Issues); n != 2 {
		t.Errorf("p1 should carry its 2 tickets, got %d", n)
	}
	theirs, ok := byID["p9"]
	if !ok {
		t.Fatal("a project holding my ticket must get a row, or that ticket disappears")
	}
	if theirs.Mine {
		t.Error("a discovered project is not mine")
	}
	if theirs.Name != "Theirs" || theirs.Key() != "proj-s9" {
		t.Errorf("discovered project not populated from the issue: %+v", theirs)
	}
	if len(theirs.Issues) != 1 {
		t.Errorf("discovered project should carry the ticket that revealed it")
	}
}

// AugmentProjects must not accumulate issues across calls — it is re-run on
// every refresh against the same stored project slice.
func TestAugmentProjectsIsIdempotent(t *testing.T) {
	mine := []Project{{ID: "p1", Name: "Mine", SlugID: "s1", Mine: true}}
	issues := []Issue{{Identifier: "ZEN-1", ProjectID: "p1"}}
	for range 3 {
		got := AugmentProjects(mine, issues)
		if n := len(got[0].Issues); n != 1 {
			t.Fatalf("re-running AugmentProjects duplicated issues: got %d", n)
		}
	}
	if mine[0].Issues != nil {
		t.Error("AugmentProjects must not mutate its input")
	}
}

// Several tickets in one undiscovered project must land on a single row, not
// one row per ticket.
func TestAugmentProjectsGroupsDiscoveredTickets(t *testing.T) {
	issues := []Issue{
		{Identifier: "ONB-1", ProjectID: "p9", ProjectName: "Theirs", ProjectSlugID: "s9"},
		{Identifier: "ONB-2", ProjectID: "p9", ProjectName: "Theirs", ProjectSlugID: "s9"},
		{Identifier: "ONB-3", ProjectID: "p8", ProjectName: "Other", ProjectSlugID: "s8"},
	}
	got := AugmentProjects(nil, issues)
	if len(got) != 2 {
		t.Fatalf("want 2 discovered projects, got %d", len(got))
	}
	total := 0
	for _, p := range got {
		total += len(p.Issues)
	}
	if total != 3 {
		t.Errorf("every ticket should be attached exactly once, got %d", total)
	}
}

func TestIssuesOutsideProjects(t *testing.T) {
	issues := []Issue{
		{Identifier: "ZEN-1", ProjectID: "p1"},
		{Identifier: "ZEN-2"},
		{Identifier: "ZEN-3", ProjectID: "p2"},
		{Identifier: "ZEN-4"},
	}
	shown := []Project{{ID: "p1"}}
	got := IssuesOutsideProjects(issues, shown)
	var keys []string
	for _, is := range got {
		keys = append(keys, is.Identifier)
	}
	// ZEN-3's project dropped off the deck, so the priority sections take its
	// ticket back rather than losing it with the row.
	if !slices.Equal(keys, []string{"ZEN-2", "ZEN-3", "ZEN-4"}) {
		t.Errorf("want the tickets no shown project owns, got %v", keys)
	}
}

func TestSortProjectsByPriorityThenLiveWork(t *testing.T) {
	got := SortProjects([]Project{
		{Name: "backlog-low", StatusType: "backlog", Priority: 4},
		{Name: "started-med", StatusType: "started", Priority: 3},
		{Name: "backlog-urgent", StatusType: "backlog", Priority: 1},
		{Name: "planned-urgent", StatusType: "planned", Priority: 1},
		{Name: "started-urgent", StatusType: "started", Priority: 1},
	})
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	want := []string{"started-urgent", "planned-urgent", "backlog-urgent", "started-med", "backlog-low"}
	if !slices.Equal(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestSortProjectsSinksFinishedWork(t *testing.T) {
	got := SortProjects([]Project{
		{Name: "done-urgent", StatusType: "completed", Priority: 1},
		{Name: "backlog-none", StatusType: "backlog"},
		{Name: "started-low", StatusType: "started", Priority: 4},
	})
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	// Urgent, but finished: it is struck through and on its way off the deck, so
	// it must not sit above work that still has to be done.
	if !slices.Equal(names, []string{"started-low", "backlog-none", "done-urgent"}) {
		t.Errorf("order = %v, want finished last", names)
	}
}

func TestSortProjectsPrefersTheNearerDeadline(t *testing.T) {
	got := SortProjects([]Project{
		{Name: "no-date", StatusType: "started", Priority: 2},
		{Name: "later", StatusType: "started", Priority: 2, TargetDate: "2026-12-01"},
		{Name: "sooner", StatusType: "started", Priority: 2, TargetDate: "2026-09-02"},
	})
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"sooner", "later", "no-date"}) {
		t.Errorf("a dated project should outrank an undated one: %v", names)
	}
}

func TestProjectHiddenKeepsWorkVisible(t *testing.T) {
	done := Project{StatusType: "completed", CompletedAt: time.Now().Add(-30 * 24 * time.Hour)}
	if !ProjectHidden(done) {
		t.Error("a long-finished project with no open tickets should drop off")
	}
	if ProjectHidden(Project{StatusType: "completed", CompletedAt: time.Now().Add(-time.Hour)}) {
		t.Error("a just-finished project should linger like a done ticket")
	}
	// A long-finished project drops off even holding open tickets of mine —
	// IssuesOutsideProjects hands those tickets back to the priority sections.
	stillWorking := done
	stillWorking.Issues = []Issue{{Identifier: "ZEN-1", StateType: "started"}}
	if !ProjectHidden(stillWorking) {
		t.Error("a finished project should drop off once its window is up")
	}
	canceled := Project{StatusType: "canceled", Issues: []Issue{{Identifier: "ZEN-1", StateType: "started"}}}
	if !ProjectHidden(canceled) {
		t.Error("a canceled project should drop off like a canceled ticket")
	}
	if ProjectHidden(Project{StatusType: "started"}) {
		t.Error("a live project should never be hidden")
	}
}

func TestOpenIssueCountIgnoresDoneTickets(t *testing.T) {
	p := Project{Issues: []Issue{
		{Identifier: "ZEN-1", StateType: "started"},
		{Identifier: "ZEN-2", StateType: "completed"},
		{Identifier: "ZEN-3", StateType: "unstarted"},
	}}
	if got := p.OpenIssueCount(); got != 2 {
		t.Errorf("OpenIssueCount = %d, want 2", got)
	}
}

func TestProjectPRsDedupesAndRanks(t *testing.T) {
	shared := PR{URL: "https://github.com/o/etp/pull/1", State: "merged", Repo: "etp", Number: 1}
	p := Project{Issues: []Issue{
		{Identifier: "ZEN-1", PRs: []PR{shared, {URL: "https://github.com/o/etp/pull/2", State: "draft", Repo: "etp", Number: 2}}},
		{Identifier: "ZEN-2", PRs: []PR{shared, {URL: "https://github.com/o/listing/pull/3", State: "open", Repo: "listing", Number: 3}}},
	}}
	got := ProjectPRs(p)
	if len(got) != 3 {
		t.Fatalf("a PR linked from two tickets should appear once: got %d", len(got))
	}
	if got[0].State != "open" || got[1].State != "draft" || got[2].State != "merged" {
		t.Errorf("PRs should be most-actionable-first, got %v", []string{got[0].State, got[1].State, got[2].State})
	}
}

func TestSortProjectIssuesMatchesTheDeckOrder(t *testing.T) {
	got := SortProjectIssues([]Issue{
		{Identifier: "C", Priority: 3, StateName: "Triage", UpdatedAt: "2026-01-01"},
		{Identifier: "A", Priority: 1, StateName: "Triage", UpdatedAt: "2026-01-01"},
		{Identifier: "B", Priority: 3, StateName: "Validate", UpdatedAt: "2026-01-01"},
		{Identifier: "D", Priority: 3, StateName: "Triage", UpdatedAt: "2026-06-01"},
	})
	var keys []string
	for _, is := range got {
		keys = append(keys, is.Identifier)
	}
	// Urgent first; then within Medium, Validate outranks Triage; then newest.
	if !slices.Equal(keys, []string{"A", "B", "D", "C"}) {
		t.Errorf("order = %v", keys)
	}
}

func TestIssueCarriesItsProject(t *testing.T) {
	n := issueNode{Identifier: "ZEN-1"}
	n.Project = &struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		SlugID string `json:"slugId"`
		URL    string `json:"url"`
		Status struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"status"`
	}{ID: "p1", Name: "Mine", SlugID: "s1", URL: "https://linear.app/x/project/mine-s1"}
	n.Project.Status.Name, n.Project.Status.Type = "In Progress", "started"

	is := n.toIssue()
	if is.ProjectID != "p1" || is.ProjectName != "Mine" || is.ProjectSlugID != "s1" {
		t.Errorf("project not mapped onto the issue: %+v", is)
	}
	if is.ProjectStatusType != "started" {
		t.Errorf("project status type = %q", is.ProjectStatusType)
	}
	// A project-less issue must stay project-less rather than panicking on nil.
	if bare := (issueNode{Identifier: "ZEN-2"}).toIssue(); bare.ProjectID != "" {
		t.Error("an issue with no project should have an empty ProjectID")
	}
}
