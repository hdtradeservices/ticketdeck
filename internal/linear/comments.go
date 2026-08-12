package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Comment is the subset of a Linear comment TicketDeck renders.
type Comment struct {
	ID        string
	Body      string // markdown
	Author    string
	CreatedAt time.Time
}

// Section is one body the description overlay can show: the issue description
// itself, or a skill-authored comment (/investigate's summary, /plan's plan).
type Section int

const (
	SectionDescription Section = iota
	SectionInvestigation
	SectionPlan
)

// Label names the section in hints and headers.
func (s Section) Label() string {
	switch s {
	case SectionInvestigation:
		return "investigation"
	case SectionPlan:
		return "plan"
	default:
		return "description"
	}
}

// match reports whether a comment body is this section's comment. /investigate
// and /plan both end their comment with a hidden marker so a re-run can update
// it in place; older comments predate the markers and are found by the heading
// the templates open with.
func (s Section) match(body string) bool {
	marker, heading := "", ""
	switch s {
	case SectionInvestigation:
		marker, heading = "<!-- investigate-summary -->", "## investigation summary"
	case SectionPlan:
		marker, heading = "<!-- implementation-plan -->", "## implementation plan"
	default:
		return false
	}
	if strings.Contains(body, marker) {
		return true
	}
	for ln := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ln)), heading) {
			return true
		}
	}
	return false
}

// FindSection returns the newest comment that is this section's comment.
func FindSection(cs []Comment, s Section) (Comment, bool) {
	var best Comment
	found := false
	for _, c := range cs {
		if !s.match(c.Body) {
			continue
		}
		if !found || c.CreatedAt.After(best.CreatedAt) {
			best, found = c, true
		}
	}
	return best, found
}

// StripMarkers drops the skills' hidden marker lines from a comment body, which
// glamour would otherwise render as a stray blank block.
func StripMarkers(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "<!--") && strings.HasSuffix(t, "-->") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// issueCommentsQuery fetches an issue's comments, newest content included, so
// the deck can show the /investigate and /plan write-ups next to the
// description. Read-only (BR-2b). Paginated.
const issueCommentsQuery = `
query IssueComments($id: String!, $after: String) {
  issue(id: $id) {
    comments(first: 50, after: $after) {
      nodes {
        id
        body
        createdAt
        user { displayName name }
        botActor { name }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// FetchComments returns every comment on an issue, oldest first as Linear
// returns them. id is the issue's node id (Issue.ID).
func (c *Client) FetchComments(ctx context.Context, id string) ([]Comment, error) {
	if id == "" {
		return nil, fmt.Errorf("linear: no issue id")
	}
	var out []Comment
	var after string
	for {
		raw, err := c.postGraphQL(ctx, issueCommentsQuery, map[string]any{"id": id, "after": nullable(after)})
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Data struct {
				Issue struct {
					Comments struct {
						Nodes []struct {
							ID        string `json:"id"`
							Body      string `json:"body"`
							CreatedAt string `json:"createdAt"`
							User      *struct {
								DisplayName string `json:"displayName"`
								Name        string `json:"name"`
							} `json:"user"`
							BotActor *struct {
								Name string `json:"name"`
							} `json:"botActor"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"comments"`
				} `json:"issue"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("linear: decode: %w", err)
		}
		if len(parsed.Errors) > 0 {
			return nil, fmt.Errorf("linear: %s", parsed.Errors[0].Message)
		}
		cs := parsed.Data.Issue.Comments
		for _, n := range cs.Nodes {
			author := ""
			switch {
			case n.User != nil && n.User.DisplayName != "":
				author = n.User.DisplayName
			case n.User != nil:
				author = n.User.Name
			case n.BotActor != nil:
				author = n.BotActor.Name
			}
			out = append(out, Comment{
				ID:        n.ID,
				Body:      n.Body,
				Author:    author,
				CreatedAt: parseTS(n.CreatedAt),
			})
		}
		if !cs.PageInfo.HasNextPage {
			break
		}
		after = cs.PageInfo.EndCursor
	}
	return out, nil
}
