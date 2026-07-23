package linear

import (
	"sort"
	"strings"
	"time"
)

// DoneVisibleFor is how long a completed ticket stays on the deck (rendered
// struck-through) after it's finished, so recently-closed work stays in view.
const DoneVisibleFor = 12 * time.Hour

// isPRURL reports whether a Linear attachment URL points at a code-review PR/MR
// (GitHub, Bitbucket, or GitLab).
func isPRURL(u string) bool {
	return strings.Contains(u, "/pull/") ||
		strings.Contains(u, "/pull-requests/") ||
		strings.Contains(u, "/merge_requests/")
}

// prState infers a PR's lifecycle state from a Linear attachment subtitle
// (e.g. "Merged", "Open · #123", "Draft"), for the status-colored PR icon.
func prState(subtitle string) string {
	s := strings.ToLower(subtitle)
	switch {
	case strings.Contains(s, "merged"):
		return "merged"
	case strings.Contains(s, "draft"):
		return "draft"
	case strings.Contains(s, "closed"):
		return "closed"
	case strings.Contains(s, "open"):
		return "open"
	default:
		return ""
	}
}

// Issue is the subset of a Linear issue TicketDeck renders.
type Issue struct {
	ID          string // Linear node id (for mutations)
	TeamID      string // owning team's node id (workflow states are per-team)
	Identifier  string // e.g. "ABC-123"
	Title       string
	Description string // markdown body (for the in-app detail view)
	Priority    int    // 0=None 1=Urgent 2=High 3=Medium 4=Low
	PrioLabel   string // "Urgent", "High", ...
	Branch      string // Linear-suggested branch name
	URL         string
	StateName   string // workflow state display name ("Todo", "In Review", ...)
	StateType   string // "triage|backlog|unstarted|started|completed|canceled"
	TeamKey     string // "ZEN", "SMA", "DOPS"
	TeamName    string
	UpdatedAt   string
	PRs         []PR       // linked pull/merge requests (from Linear attachments)
	Labels      []string   // issue label names (e.g. "validation-inconclusive")
	CompletedAt time.Time  // when a completed-type issue was finished (zero if not)
	BlockedBy   []Relation // issues blocking this one (inverseRelations, type "blocks")
}

// Relation is a linked issue (a "blocks" dependency in either direction). ID and
// TeamID are populated only where a downstream write needs them (the dependents
// fetched for the unblock-cascade); the blocked-by note needs just Identifier +
// state.
type Relation struct {
	ID         string
	Identifier string
	TeamID     string
	StateName  string
	StateType  string
}

// blockingDone reports whether a related issue is itself finished (so it no
// longer blocks / no longer needs re-triage).
func (r Relation) blockingDone() bool {
	return r.StateType == "completed" || r.StateType == "canceled"
}

// IsDone reports whether the issue is in a completed-type workflow state.
func (is Issue) IsDone() bool { return is.StateType == "completed" }

// RecentlyDone reports whether a completed issue finished within DoneVisibleFor
// of now — the window it stays visible (struck-through) on the deck.
func (is Issue) RecentlyDone(now time.Time) bool {
	return is.IsDone() && !is.CompletedAt.IsZero() && now.Sub(is.CompletedAt) < DoneVisibleFor
}

// OpenBlockers returns the issues blocking this one that aren't themselves
// done/cancelled — the ones still actually holding it up.
func (is Issue) OpenBlockers() []Relation {
	var out []Relation
	for _, r := range is.BlockedBy {
		if !r.blockingDone() {
			out = append(out, r)
		}
	}
	return out
}

// PR is a pull/merge request linked to an issue via a Linear attachment.
type PR struct {
	URL   string
	Title string
	State string // "open" | "merged" | "closed" | "draft" | "" (unknown)
}

// User is a Linear workspace member (for the assignee picker).
type User struct {
	ID          string
	Name        string
	DisplayName string
	Email       string
}

// Label is the picker display text for a user.
func (u User) Label() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// hiddenStateTypes are Linear workflow-state types TicketDeck never shows:
// Done (completed), Cancelled (canceled), and Duplicate (duplicate).
var hiddenStateTypes = map[string]bool{"completed": true, "canceled": true, "duplicate": true}

// shownStateNames are workflow states kept visible even though their type would
// otherwise hide them. "Validate" is a completed-type state (a QA/review gate)
// but is still actionable, so it stays on the deck alongside started work.
var shownStateNames = map[string]bool{"validate": true}

// HiddenStateTypeList returns the excluded types for the server-side filter.
func HiddenStateTypeList() []string {
	return []string{"completed", "canceled", "duplicate"}
}

// ShownStateNameList returns state names that override the type-based hide, so
// the server-side filter can fetch them back in.
func ShownStateNameList() []string {
	return []string{"Validate"}
}

// IsHidden reports whether an issue should be filtered from the view (BR-2a).
// A state name in shownStateNames overrides the type hide (e.g. Validate); a
// completed ticket also stays visible for DoneVisibleFor after it's done (shown
// struck-through) so recently-closed work lingers in the deck.
func IsHidden(is Issue) bool {
	if shownStateNames[strings.ToLower(is.StateName)] {
		return false
	}
	if is.RecentlyDone(time.Now()) {
		return false
	}
	return hiddenStateTypes[is.StateType]
}

// FilterVisible drops Done/Cancelled/Duplicate issues (BR-2a). Every consumer
// that renders a list should route through this so the rule lives in one place.
func FilterVisible(issues []Issue) []Issue {
	out := issues[:0:0]
	for _, is := range issues {
		if !IsHidden(is) {
			out = append(out, is)
		}
	}
	return out
}

// prioRank orders priorities for display: Urgent→High→Medium→Low→None(last).
// Linear encodes None as 0, which would otherwise sort first.
func prioRank(p int) int {
	if p == 0 {
		return 5
	}
	return p
}

func prioLabel(p int) string {
	switch p {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "No priority"
	}
}

// Group is a set of issues under one priority, sub-grouped by status.
type Group struct {
	Priority  int
	PrioLabel string
	Statuses  []StatusBucket
}

// StatusBucket is the issues sharing one workflow status within a priority group.
type StatusBucket struct {
	Status string
	Issues []Issue
}

// GroupByPriorityThenStatus implements BR-2a: primary grouping by priority
// (Urgent first, No priority last), secondary by status within each priority.
// Issues within a status bucket are ordered by most-recently-updated.
func GroupByPriorityThenStatus(issues []Issue) []Group {
	byPrio := map[int][]Issue{}
	for _, is := range issues {
		byPrio[is.Priority] = append(byPrio[is.Priority], is)
	}

	prios := make([]int, 0, len(byPrio))
	for p := range byPrio {
		prios = append(prios, p)
	}
	sort.Slice(prios, func(i, j int) bool { return prioRank(prios[i]) < prioRank(prios[j]) })

	groups := make([]Group, 0, len(prios))
	for _, p := range prios {
		items := byPrio[p]
		byStatus := map[string][]Issue{}
		for _, is := range items {
			byStatus[is.StateName] = append(byStatus[is.StateName], is)
		}
		statuses := make([]string, 0, len(byStatus))
		for s := range byStatus {
			statuses = append(statuses, s)
		}
		sort.Strings(statuses)

		buckets := make([]StatusBucket, 0, len(statuses))
		for _, s := range statuses {
			bucket := byStatus[s]
			sort.Slice(bucket, func(i, j int) bool { return bucket[i].UpdatedAt > bucket[j].UpdatedAt })
			buckets = append(buckets, StatusBucket{Status: s, Issues: bucket})
		}
		groups = append(groups, Group{Priority: p, PrioLabel: prioLabel(p), Statuses: buckets})
	}
	return groups
}
