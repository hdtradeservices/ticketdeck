package linear

import (
	"fmt"
	"sort"
	"strconv"
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
// (e.g. "Merged", "Open · #123", "Draft"). Fallback for sources that populate a
// subtitle at all; GitHub doesn't (see prStateFromMeta).
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

// prStateFromMeta reads a PR's state from a Linear attachment's structured
// metadata. This is the reliable source: GitHub attachments send
// `subtitle: null` and put the state in metadata.status with a separate draft
// flag, so a draft reads as status "open".
//
// status is not limited to open/merged/closed — "inReview" also shows up — so
// anything that isn't finished counts as open rather than falling through to an
// unknown state.
func prStateFromMeta(status string, draft bool) string {
	if draft {
		return "draft"
	}
	switch s := strings.ToLower(status); s {
	case "":
		return "" // no metadata; let the subtitle fallback try
	case "merged":
		return "merged"
	case "closed", "cancelled", "canceled":
		return "closed"
	default:
		return "open" // "open", "inReview", and any future not-yet-finished status
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

// IsValidate reports whether the issue sits in the Validate gate — a
// completed-type state that stays active on the deck rather than reading as
// finished work.
func (is Issue) IsValidate() bool { return strings.EqualFold(is.StateName, "Validate") }

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
	URL    string
	Title  string
	State  string // "open" | "merged" | "closed" | "draft" | "" (unknown)
	Repo   string // repository name, e.g. "etp" (from attachment metadata)
	Number int    // PR/MR number, e.g. 26020
}

// Label identifies a PR compactly as "repo#number" — the disambiguator that
// matters when a ticket spans several repos, which is the common multi-PR case.
// Falls back to parsing the URL when metadata is absent, then to the raw URL.
func (p PR) Label() string {
	repo, num := p.Repo, p.Number
	if repo == "" || num == 0 {
		r, n := parsePRURL(p.URL)
		if repo == "" {
			repo = r
		}
		if num == 0 {
			num = n
		}
	}
	switch {
	case repo != "" && num != 0:
		return fmt.Sprintf("%s#%d", repo, num)
	case repo != "":
		return repo
	case num != 0:
		return fmt.Sprintf("#%d", num)
	}
	return p.URL
}

// parsePRURL pulls the repo name and PR number out of a PR/MR URL, for links
// whose attachment metadata is missing them (non-GitHub sources).
// e.g. https://github.com/org/etp/pull/26020 → ("etp", 26020)
func parsePRURL(u string) (repo string, number int) {
	parts := strings.Split(strings.TrimSuffix(u, "/"), "/")
	for i, p := range parts {
		switch p {
		case "pull", "pull-requests", "merge_requests":
			if i+1 < len(parts) {
				number, _ = strconv.Atoi(parts[i+1])
			}
			// The repo is the path segment before the PR marker; GitLab nests it
			// under "/-/", so step back over that separator.
			j := i - 1
			if j >= 0 && parts[j] == "-" {
				j--
			}
			if j >= 0 {
				repo = parts[j]
			}
			return repo, number
		}
	}
	return "", 0
}

// PRRank orders PR states by how much they still need you: an open PR outranks
// a draft, then merged, then closed. Used for both the row icon's summary state
// and the multi-PR picker's ordering.
func PRRank(state string) int {
	switch state {
	case "open":
		return 4
	case "draft":
		return 3
	case "merged":
		return 2
	case "closed":
		return 1
	}
	return 0
}

// SortPRs orders PRs most-actionable-first (open, draft, merged, closed), then
// by repo, then highest number first so a resubmitted PR leads its supersedes.
// Returns a sorted copy; the input is left alone.
func SortPRs(prs []PR) []PR {
	out := make([]PR, len(prs))
	copy(out, prs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := PRRank(a.State), PRRank(b.State); ra != rb {
			return ra > rb
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number > b.Number
	})
	return out
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

// statusOrder is the fixed display order of status buckets within a priority
// group. Unlisted statuses sort after these (then alphabetically), except Done
// which always sorts last.
var statusOrder = []string{"Validate", "In Review", "Planned", "Triage", "Blocked"}

// statusRank returns the sort key for a status name (case-insensitive); lower
// sorts first. Done is pinned last so finished work sinks to the bottom.
func statusRank(name string) int {
	for i, s := range statusOrder {
		if strings.EqualFold(name, s) {
			return i
		}
	}
	if strings.EqualFold(name, "Done") {
		return len(statusOrder) + 1
	}
	return len(statusOrder) // unlisted, non-Done: between the known set and Done
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
		sort.Slice(statuses, func(i, j int) bool {
			ri, rj := statusRank(statuses[i]), statusRank(statuses[j])
			if ri != rj {
				return ri < rj
			}
			return statuses[i] < statuses[j]
		})

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
