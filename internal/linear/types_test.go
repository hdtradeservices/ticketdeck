package linear

import (
	"testing"
	"time"
)

func TestIsHiddenExcludesDoneCancelledDuplicate(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"completed", true}, // Done
		{"canceled", true},  // Cancelled
		{"duplicate", true}, // Duplicate
		{"started", false},  // In Progress / In Review / Merged
		{"unstarted", false},
		{"backlog", false},
		{"triage", false},
	}
	for _, c := range cases {
		if got := IsHidden(Issue{StateType: c.typ}); got != c.want {
			t.Errorf("IsHidden(type=%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}

func TestGroupByPriorityThenStatusOrdersAndBuckets(t *testing.T) {
	issues := []Issue{
		{Identifier: "A", Priority: 3, StateName: "Todo", UpdatedAt: "2026-01-01"},
		{Identifier: "B", Priority: 1, StateName: "In Progress"},
		{Identifier: "C", Priority: 3, StateName: "Todo", UpdatedAt: "2026-02-01"},
		{Identifier: "D", Priority: 0, StateName: "Todo"}, // None priority sorts last
	}
	groups := GroupByPriorityThenStatus(issues)
	if groups[0].Priority != 1 {
		t.Errorf("first group should be Urgent(1), got %d", groups[0].Priority)
	}
	if groups[len(groups)-1].Priority != 0 {
		t.Errorf("No-priority(0) should sort last, got %d", groups[len(groups)-1].Priority)
	}
	// Within Medium/Todo, newest-updated first: C (Feb) before A (Jan).
	var medium Group
	for _, g := range groups {
		if g.Priority == 3 {
			medium = g
		}
	}
	todo := medium.Statuses[0]
	if todo.Issues[0].Identifier != "C" {
		t.Errorf("newest issue should sort first in a status bucket, got %s", todo.Issues[0].Identifier)
	}
}

func TestIsPRURL(t *testing.T) {
	prs := []string{
		"https://github.com/acme/widgets/pull/241",
		"https://gitlab.com/acme/app/-/merge_requests/12",
		"https://bitbucket.org/acme/app/pull-requests/7",
	}
	for _, u := range prs {
		if !isPRURL(u) {
			t.Errorf("expected %q to be a PR url", u)
		}
	}
	nonPRs := []string{
		"https://linear.app/acme/issue/ABC-1",
		"https://github.com/acme/widgets/commit/abc",
		"https://github.com/acme/widgets/tree/main",
	}
	for _, u := range nonPRs {
		if isPRURL(u) {
			t.Errorf("expected %q NOT to be a PR url", u)
		}
	}
}

func TestPRStateFromMeta(t *testing.T) {
	// GitHub attachments send subtitle:null and carry state in metadata; a draft
	// arrives as status "open" plus draft:true.
	cases := []struct {
		status string
		draft  bool
		want   string
	}{
		{"open", false, "open"},
		{"open", true, "draft"},
		{"merged", false, "merged"},
		{"closed", false, "closed"},
		{"MERGED", false, "merged"},
		{"inReview", false, "open"}, // real GitHub value: still open, awaiting review
		{"weird", false, "open"},    // unknown-but-unfinished reads as open, not blank
		{"merged", true, "draft"},   // draft wins
		{"", false, ""},             // no metadata → subtitle fallback
	}
	for _, c := range cases {
		if got := prStateFromMeta(c.status, c.draft); got != c.want {
			t.Errorf("prStateFromMeta(%q, %v) = %q, want %q", c.status, c.draft, got, c.want)
		}
	}
}

func TestPRLabel(t *testing.T) {
	// Metadata present → used directly.
	if got := (PR{Repo: "etp", Number: 26020}).Label(); got != "etp#26020" {
		t.Errorf("Label() = %q, want etp#26020", got)
	}
	// No metadata → parsed from the URL, for each forge shape.
	cases := map[string]string{
		"https://github.com/hdtradeservices/etp/pull/26020":     "etp#26020",
		"https://gitlab.com/acme/app/-/merge_requests/12":       "app#12",
		"https://bitbucket.org/acme/app/pull-requests/7":        "app#7",
		"https://github.com/hdtradeservices/ebay/pull/57/files": "ebay#57",
	}
	for u, want := range cases {
		if got := (PR{URL: u}).Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", u, got, want)
		}
	}
	// Unparseable → the raw URL, never an empty label.
	if got := (PR{URL: "https://example.com/thing"}).Label(); got != "https://example.com/thing" {
		t.Errorf("Label() fallback = %q, want the raw url", got)
	}
}

func TestSortPRsMostActionableFirst(t *testing.T) {
	in := []PR{
		{Repo: "etp", Number: 100, State: "merged"},
		{Repo: "walmart", Number: 5, State: "closed"},
		{Repo: "listing", Number: 9, State: "open"},
		{Repo: "charts", Number: 2, State: "draft"},
		{Repo: "etp", Number: 101, State: "merged"}, // same repo → higher number first
	}
	got := SortPRs(in)
	want := []string{"listing#9", "charts#2", "etp#101", "etp#100", "walmart#5"}
	for i, w := range want {
		if got[i].Label() != w {
			t.Errorf("SortPRs()[%d] = %q, want %q (full: %v)", i, got[i].Label(), w, labels(got))
		}
	}
	// The input slice must be untouched (callers render from the original).
	if in[0].Label() != "etp#100" {
		t.Errorf("SortPRs mutated its input: %v", labels(in))
	}
}

func labels(prs []PR) []string {
	out := make([]string, len(prs))
	for i, p := range prs {
		out[i] = p.Label()
	}
	return out
}

func TestPRState(t *testing.T) {
	cases := map[string]string{
		"Merged · #241":  "merged",
		"Draft":          "draft",
		"Closed":         "closed",
		"Open · #12":     "open",
		"":               "",
		"something else": "",
	}
	for subtitle, want := range cases {
		if got := prState(subtitle); got != want {
			t.Errorf("prState(%q) = %q, want %q", subtitle, got, want)
		}
	}
}

func TestValidateShownDoneHidden(t *testing.T) {
	cases := []struct {
		name, stype string
		hidden      bool
	}{
		{"Done", "completed", true},
		{"Validate", "completed", false}, // completed-type but kept visible
		{"Canceled", "canceled", true},
		{"Duplicate", "duplicate", true},
		{"In Progress", "started", false},
		{"Monitoring", "started", false},
		{"Blocked", "started", false},
	}
	for _, c := range cases {
		got := IsHidden(Issue{StateName: c.name, StateType: c.stype})
		if got != c.hidden {
			t.Errorf("IsHidden(%s/%s) = %v, want %v", c.name, c.stype, got, c.hidden)
		}
	}
}

func TestSplitKey(t *testing.T) {
	ok := []struct {
		in   string
		team string
		num  int
	}{
		{"ZEN-3309", "ZEN", 3309},
		{"dops-12", "DOPS", 12},
		{"  sma-7 ", "SMA", 7},
	}
	for _, c := range ok {
		team, num, err := splitKey(c.in)
		if err != nil || team != c.team || num != c.num {
			t.Errorf("splitKey(%q) = (%q,%d,%v), want (%q,%d,nil)", c.in, team, num, err, c.team, c.num)
		}
	}
	for _, bad := range []string{"", "ZEN", "ZEN-", "-5", "ZEN-abc"} {
		if _, _, err := splitKey(bad); err == nil {
			t.Errorf("splitKey(%q) should error", bad)
		}
	}
}

func TestRecentlyDoneAndHidden(t *testing.T) {
	now := time.Now()
	recent := Issue{StateName: "Done", StateType: "completed", CompletedAt: now.Add(-2 * time.Hour)}
	old := Issue{StateName: "Done", StateType: "completed", CompletedAt: now.Add(-13 * time.Hour)}
	nots := Issue{StateName: "Done", StateType: "completed"} // no timestamp
	if !recent.RecentlyDone(now) {
		t.Error("2h-old Done should be recently done")
	}
	if old.RecentlyDone(now) {
		t.Error("13h-old Done should not be recently done")
	}
	if IsHidden(recent) {
		t.Error("recently-done should stay visible")
	}
	if !IsHidden(old) || !IsHidden(nots) {
		t.Error("old / timestamp-less Done should be hidden")
	}
}

func TestOpenBlockers(t *testing.T) {
	is := Issue{BlockedBy: []Relation{
		{Identifier: "ZEN-1", StateType: "started"},
		{Identifier: "ZEN-2", StateType: "completed"}, // done → not blocking
		{Identifier: "ZEN-3", StateType: "canceled"},  // cancelled → not blocking
		{Identifier: "ZEN-4", StateType: "unstarted"},
	}}
	open := is.OpenBlockers()
	if len(open) != 2 || open[0].Identifier != "ZEN-1" || open[1].Identifier != "ZEN-4" {
		t.Errorf("OpenBlockers = %+v, want ZEN-1, ZEN-4", open)
	}
}
