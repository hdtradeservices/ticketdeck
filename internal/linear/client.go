package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const endpoint = "https://api.linear.app/graphql"

// Client is a minimal read-only Linear GraphQL client. It performs no writes
// (BR-2b) and never routes data through a model (BR-2).
type Client struct {
	apiKey string
	http   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 20 * time.Second},
	}
}

// assignedOpenQuery fetches the current viewer's assigned issues, excluding
// completed/canceled workflow states server-side (BR-2a). Paginated.
const assignedOpenQuery = `
query AssignedOpen($after: String) {
  viewer {
    assignedIssues(
      first: 100
      after: $after
      orderBy: updatedAt
      filter: {
        or: [
          { state: { type: { nin: ["completed", "canceled", "duplicate"] } } }
          { state: { name: { eq: "Validate" } } }
        ]
      }
    ) {
      nodes {
        id
        identifier
        title
        description
        priority
        priorityLabel
        branchName
        url
        state { name type }
        team { id key name }
        updatedAt
        attachments { nodes { url title subtitle } }
        labels { nodes { name } }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// issueNode is the shared GraphQL node shape for the issue fields TicketDeck
// reads — used by both the assigned-issues list and the single-issue lookup.
type issueNode struct {
	ID            string `json:"id"`
	Identifier    string `json:"identifier"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Priority      int    `json:"priority"`
	PriorityLabel string `json:"priorityLabel"`
	BranchName    string `json:"branchName"`
	URL           string `json:"url"`
	State         struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Team struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	UpdatedAt   string `json:"updatedAt"`
	Attachments struct {
		Nodes []struct {
			URL      string `json:"url"`
			Title    string `json:"title"`
			Subtitle string `json:"subtitle"`
		} `json:"nodes"`
	} `json:"attachments"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// toIssue maps a GraphQL node to the Issue TicketDeck renders.
func (n issueNode) toIssue() Issue {
	issue := Issue{
		ID:          n.ID,
		TeamID:      n.Team.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		Priority:    n.Priority,
		PrioLabel:   n.PriorityLabel,
		Branch:      n.BranchName,
		URL:         n.URL,
		StateName:   n.State.Name,
		StateType:   n.State.Type,
		TeamKey:     n.Team.Key,
		TeamName:    n.Team.Name,
		UpdatedAt:   n.UpdatedAt,
	}
	for _, a := range n.Attachments.Nodes {
		if isPRURL(a.URL) {
			issue.PRs = append(issue.PRs, PR{URL: a.URL, Title: a.Title, State: prState(a.Subtitle)})
		}
	}
	for _, l := range n.Labels.Nodes {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue
}

type gqlResponse struct {
	Data struct {
		Viewer struct {
			AssignedIssues struct {
				Nodes    []issueNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"assignedIssues"`
		} `json:"viewer"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// FetchAssignedOpen returns all assigned, non-completed/canceled issues for the
// authenticated user, following pagination.
func (c *Client) FetchAssignedOpen(ctx context.Context) ([]Issue, error) {
	var out []Issue
	var after string
	for {
		resp, err := c.query(ctx, assignedOpenQuery, map[string]any{"after": nullable(after)})
		if err != nil {
			return nil, err
		}
		ai := resp.Data.Viewer.AssignedIssues
		for _, n := range ai.Nodes {
			issue := n.toIssue()
			// Client-side guard in case the server filter is ever loosened.
			if IsHidden(issue) {
				continue
			}
			out = append(out, issue)
		}
		if !ai.PageInfo.HasNextPage {
			break
		}
		after = ai.PageInfo.EndCursor
	}
	return out, nil
}

// issueByKeyQuery fetches a single issue by its human key (team key + number),
// so `ticketdeck describe` can show any ticket — including Done ones that the
// assigned-open list wouldn't return. Read-only (BR-2b).
const issueByKeyQuery = `
query IssueByKey($team: String!, $number: Float!) {
  issues(first: 1, filter: { team: { key: { eq: $team } }, number: { eq: $number } }) {
    nodes {
      id
      identifier
      title
      description
      priority
      priorityLabel
      branchName
      url
      state { name type }
      team { id key name }
      updatedAt
      attachments { nodes { url title subtitle } }
      labels { nodes { name } }
    }
  }
}`

// FetchIssue returns a single issue by its key (e.g. "ZEN-3309").
func (c *Client) FetchIssue(ctx context.Context, key string) (Issue, error) {
	team, number, err := splitKey(key)
	if err != nil {
		return Issue{}, err
	}
	raw, err := c.postGraphQL(ctx, issueByKeyQuery, map[string]any{"team": team, "number": number})
	if err != nil {
		return Issue{}, err
	}
	var parsed struct {
		Data struct {
			Issues struct {
				Nodes []issueNode `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Issue{}, fmt.Errorf("linear: decode: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return Issue{}, fmt.Errorf("linear: %s", parsed.Errors[0].Message)
	}
	if len(parsed.Data.Issues.Nodes) == 0 {
		return Issue{}, fmt.Errorf("linear: no issue %s", strings.ToUpper(key))
	}
	return parsed.Data.Issues.Nodes[0].toIssue(), nil
}

// splitKey parses a ticket key ("ZEN-3309") into its team key and number.
func splitKey(key string) (string, int, error) {
	key = strings.TrimSpace(strings.ToUpper(key))
	i := strings.LastIndex(key, "-")
	if i <= 0 || i == len(key)-1 {
		return "", 0, fmt.Errorf("bad ticket key %q (want e.g. ZEN-3309)", key)
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad ticket key %q (want e.g. ZEN-3309)", key)
	}
	return key[:i], n, nil
}

func (c *Client) query(ctx context.Context, query string, vars map[string]any) (*gqlResponse, error) {
	raw, err := c.postGraphQL(ctx, query, vars)
	if err != nil {
		return nil, err
	}
	var parsed gqlResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("linear: decode: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("linear: %s", parsed.Errors[0].Message)
	}
	return &parsed, nil
}

// postGraphQL performs a GraphQL POST and returns the raw response body after
// HTTP-level checks (rate limit, non-200). Callers unmarshal into their own
// shape and check the GraphQL `errors` array.
func (c *Client) postGraphQL(ctx context.Context, query string, vars map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Personal API keys go in Authorization raw; OAuth tokens use Bearer.
	if strings.HasPrefix(c.apiKey, "lin_oauth") {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else {
		req.Header.Set("Authorization", c.apiKey)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{RetryAfter: res.Header.Get("Retry-After")}
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear: http %d: %s", res.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// RateLimitError signals an HTTP 429 so the caller can back off (BR-2c).
type RateLimitError struct{ RetryAfter string }

func (e *RateLimitError) Error() string {
	if e.RetryAfter != "" {
		return "linear: rate limited (retry after " + e.RetryAfter + ")"
	}
	return "linear: rate limited"
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
