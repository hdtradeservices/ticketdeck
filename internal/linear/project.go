package linear

import (
	"math"
	"sort"
	"strings"
	"time"
)

// ProjectKeyPrefix namespaces a project's session key so it can never collide
// with a Linear ticket key (which is always TEAM-123).
const ProjectKeyPrefix = "proj-"

// Project is the subset of a Linear project TicketDeck renders as a top-section
// row you can open a Claude session against.
type Project struct {
	ID          string // Linear node id (for mutations)
	Name        string
	SlugID      string // stable short id from the project URL; the session key derives from it
	URL         string
	Summary     string // Linear's `description`: the one-line summary
	Content     string // the long project doc (markdown), for the detail overlay
	Priority    int    // 0=None 1=Urgent 2=High 3=Medium 4=Low
	PrioLabel   string
	Progress    float64 // 0..1, share of scope completed
	Scope       float64 // total estimate points in the project
	Health      string  // "onTrack" | "atRisk" | "offTrack" | ""
	StatusName  string  // "Backlog", "In Progress", ...
	StatusType  string  // backlog|planned|started|paused|completed|canceled
	StartDate   string
	TargetDate  string
	LeadID      string
	LeadName    string
	TeamKeys    []string
	UpdatedAt   string
	CompletedAt time.Time
	// Mine is true when I lead the project. A project that is false here is on
	// the deck only because it owns tickets assigned to me — see
	// AugmentProjects, which exists so filtering those tickets out of the
	// priority sections can never make them disappear entirely.
	Mine bool
	// Issues are my assigned, open issues in this project. Filled in by
	// AugmentProjects rather than by the fetch, so one issue list serves both
	// the priority sections and the project rows.
	Issues []Issue
}

// Key is the project's session identity: the id TicketDeck binds a Claude
// session to, and the name it gives that session's herdr agent.
//
// It derives from SlugID, not the name, because a session must survive a
// rename: the transcript lives at a path derived from this key, so a key that
// tracked the title would orphan the conversation the moment someone edited it.
func (p Project) Key() string { return ProjectKeyPrefix + p.SlugID }

// IsProjectKey reports whether a session key belongs to a project rather than a
// ticket. Ticket keys are TEAM-123 and never carry this prefix.
func IsProjectKey(key string) bool { return strings.HasPrefix(key, ProjectKeyPrefix) }

// ProgressPct is Progress as a whole percent, for display. Unfinished work
// never rounds up to 100: Linear reports 0.999 for a project with one ticket
// left, and "100%" beside two open tickets reads as a bug in the deck.
func ProgressPct(frac float64) int {
	pct := int(math.Round(min(max(frac, 0), 1) * 100))
	if pct == 100 && frac < 1 {
		return 99
	}
	return pct
}

// ProgressPct is the project's Progress as a whole percent.
func (p Project) ProgressPct() int { return ProgressPct(p.Progress) }

// IsDone reports whether the project sits in a completed-type status.
func (p Project) IsDone() bool { return p.StatusType == "completed" }

// RecentlyDone reports whether a completed project finished within
// DoneVisibleFor — the window it lingers on the deck, like a done ticket.
func (p Project) RecentlyDone(now time.Time) bool {
	return p.IsDone() && !p.CompletedAt.IsZero() && now.Sub(p.CompletedAt) < DoneVisibleFor
}

// OpenIssueCount is how many of my assigned issues in this project are still
// open (a recently-done one lingers in Issues but isn't outstanding work).
func (p Project) OpenIssueCount() int {
	n := 0
	for _, is := range p.Issues {
		if !is.IsDone() {
			n++
		}
	}
	return n
}

// hiddenProjectStatusTypes are the statuses a project drops off the deck in,
// mirroring the ticket rule (BR-2a).
var hiddenProjectStatusTypes = map[string]bool{"completed": true, "canceled": true}

// ProjectHidden reports whether a project should be filtered from the view. A
// completed project lingers for DoneVisibleFor like a done ticket; a project
// still holding open tickets of mine always stays, since hiding it would hide
// those tickets with it.
func ProjectHidden(p Project) bool {
	if p.OpenIssueCount() > 0 {
		return false
	}
	if p.RecentlyDone(time.Now()) {
		return false
	}
	return hiddenProjectStatusTypes[p.StatusType]
}

// FilterVisibleProjects drops completed/canceled projects (BR-2a).
func FilterVisibleProjects(ps []Project) []Project {
	out := ps[:0:0]
	for _, p := range ps {
		if !ProjectHidden(p) {
			out = append(out, p)
		}
	}
	return out
}

// projectStatusRank orders project statuses by how live the work is: started
// first, then planned, backlog, paused, and finished last.
func projectStatusRank(t string) int {
	switch t {
	case "started":
		return 0
	case "planned":
		return 1
	case "backlog":
		return 2
	case "paused":
		return 3
	}
	return 4 // completed / canceled
}

// SortProjects orders projects the way you work them: live before planned,
// then by priority, then by the nearest target date, then by name. Returns a
// sorted copy.
func SortProjects(ps []Project) []Project {
	out := make([]Project, len(ps))
	copy(out, ps)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := projectStatusRank(a.StatusType), projectStatusRank(b.StatusType); ra != rb {
			return ra < rb
		}
		if ra, rb := prioRank(a.Priority), prioRank(b.Priority); ra != rb {
			return ra < rb
		}
		// Empty target dates sort last: a project with a deadline is the more
		// urgent of two otherwise-equal ones.
		if a.TargetDate != b.TargetDate {
			if a.TargetDate == "" {
				return false
			}
			if b.TargetDate == "" {
				return true
			}
			return a.TargetDate < b.TargetDate
		}
		return a.Name < b.Name
	})
	return out
}

// AugmentProjects attaches my assigned issues to the projects that own them and
// returns the full project list to render, sorted.
//
// It also invents a project row for any project that owns one of my issues but
// wasn't in the "my projects" fetch — someone else's project I have a ticket
// in. Without that, filtering project-bearing tickets out of the priority
// sections would silently drop those tickets off the deck entirely.
func AugmentProjects(projects []Project, issues []Issue) []Project {
	byID := make(map[string]*Project, len(projects))
	out := make([]Project, len(projects))
	copy(out, projects)
	for i := range out {
		out[i].Issues = nil
		byID[out[i].ID] = &out[i]
	}
	// Discovered projects go in a second pass: appending to `out` while holding
	// pointers into it would invalidate them on the next grow.
	var discovered []Project
	byDiscovered := map[string]*Project{}
	for _, is := range issues {
		if is.ProjectID == "" {
			continue
		}
		if p, ok := byID[is.ProjectID]; ok {
			p.Issues = append(p.Issues, is)
			continue
		}
		if p, ok := byDiscovered[is.ProjectID]; ok {
			p.Issues = append(p.Issues, is)
			continue
		}
		discovered = append(discovered, Project{
			ID:         is.ProjectID,
			Name:       is.ProjectName,
			SlugID:     is.ProjectSlugID,
			URL:        is.ProjectURL,
			StatusName: is.ProjectStatus,
			StatusType: is.ProjectStatusType,
			Issues:     []Issue{is},
		})
		byDiscovered[is.ProjectID] = &discovered[len(discovered)-1]
	}
	out = append(out, discovered...)
	return SortProjects(out)
}

// IssuesWithoutProject returns the issues that belong to no project — the ones
// that still group into the priority sections. Everything else is reachable
// under its project row instead.
func IssuesWithoutProject(issues []Issue) []Issue {
	out := issues[:0:0]
	for _, is := range issues {
		if is.ProjectID == "" {
			out = append(out, is)
		}
	}
	return out
}

// SortProjectIssues orders a project's issues the way the priority sections do:
// priority first, then status, then most-recently-updated.
func SortProjectIssues(issues []Issue) []Issue {
	out := make([]Issue, len(issues))
	copy(out, issues)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := prioRank(a.Priority), prioRank(b.Priority); ra != rb {
			return ra < rb
		}
		if ra, rb := statusRank(a.StateName), statusRank(b.StateName); ra != rb {
			return ra < rb
		}
		return a.UpdatedAt > b.UpdatedAt
	})
	return out
}

// ProjectPRs is every PR linked to the project's issues, most-actionable-first
// and de-duplicated — what `p` opens from a project row.
func ProjectPRs(p Project) []PR {
	seen := map[string]bool{}
	var out []PR
	for _, is := range p.Issues {
		for _, pr := range is.PRs {
			if seen[pr.URL] {
				continue
			}
			seen[pr.URL] = true
			out = append(out, pr)
		}
	}
	return SortPRs(out)
}

// ProjectHealthLabel maps Linear's health signal to a compact row flag. onTrack
// gets none: healthy is the default, and flagging it would spend the row's
// scarcest column saying nothing is wrong.
func ProjectHealthLabel(health string) string {
	switch health {
	case "atRisk":
		return "⚑ at risk"
	case "offTrack":
		return "⚑ off track"
	}
	return ""
}
