package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MoveState transitions an issue to a target workflow state ("Done", "Validate",
// or "Canceled"). It resolves the team-specific state id, then runs issueUpdate.
// This is the only write TicketDeck performs; it needs a write-scoped API key.
func (c *Client) MoveState(ctx context.Context, issue Issue, target string) error {
	if issue.ID == "" || issue.TeamID == "" {
		return fmt.Errorf("missing issue/team id for %s (refresh and retry)", issue.Identifier)
	}
	stateID, stateName, err := c.resolveStateID(ctx, issue.TeamID, target)
	if err != nil {
		return err
	}
	raw, err := c.postGraphQL(ctx, issueUpdateMutation, map[string]any{"id": issue.ID, "stateId": stateID})
	if err != nil {
		return err
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode issueUpdate: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("%s (a write-scoped LINEAR_API_KEY is required)", resp.Errors[0].Message)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("linear rejected the move to %s", stateName)
	}
	return nil
}

const issueUpdateMutation = `
mutation Move($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) { success }
}`

const issueDependentsQuery = `
query Dependents($id: String!) {
  issue(id: $id) {
    relations {
      nodes {
        type
        relatedIssue { id identifier team { id } state { name type } }
      }
    }
  }
}`

// FetchBlocking returns the issues this one blocks (its "blocks" relations) — the
// dependents that may become unblocked when it closes. Read-only.
func (c *Client) FetchBlocking(ctx context.Context, key string) ([]Relation, error) {
	raw, err := c.postGraphQL(ctx, issueDependentsQuery, map[string]any{"id": strings.ToUpper(strings.TrimSpace(key))})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Issue struct {
				Relations struct {
					Nodes []struct {
						Type         string `json:"type"`
						RelatedIssue struct {
							ID         string `json:"id"`
							Identifier string `json:"identifier"`
							Team       struct {
								ID string `json:"id"`
							} `json:"team"`
							State struct {
								Name string `json:"name"`
								Type string `json:"type"`
							} `json:"state"`
						} `json:"relatedIssue"`
					} `json:"nodes"`
				} `json:"relations"`
			} `json:"issue"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode dependents: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("%s", resp.Errors[0].Message)
	}
	var out []Relation
	for _, r := range resp.Data.Issue.Relations.Nodes {
		if r.Type != "blocks" {
			continue
		}
		out = append(out, Relation{
			ID:         r.RelatedIssue.ID,
			Identifier: r.RelatedIssue.Identifier,
			TeamID:     r.RelatedIssue.Team.ID,
			StateName:  r.RelatedIssue.State.Name,
			StateType:  r.RelatedIssue.State.Type,
		})
	}
	return out, nil
}

// UnblockToTriage moves every still-open ticket that `issue` was blocking to its
// team's Triage state — the unblock cascade run when a blocker is completed.
// Returns the identifiers actually moved. Dependents already done/cancelled or
// already in Triage are skipped; a per-dependent write failure is returned as
// err but doesn't stop the others.
func (c *Client) UnblockToTriage(ctx context.Context, issue Issue) ([]string, error) {
	deps, err := c.FetchBlocking(ctx, issue.Identifier)
	if err != nil {
		return nil, err
	}
	var moved []string
	var firstErr error
	for _, d := range deps {
		if d.blockingDone() || strings.EqualFold(d.StateName, "Triage") {
			continue
		}
		if err := c.MoveState(ctx, Issue{ID: d.ID, TeamID: d.TeamID, Identifier: d.Identifier}, "Triage"); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		moved = append(moved, d.Identifier)
	}
	return moved, firstErr
}

// Assign sets an issue's assignee. An empty assigneeID unassigns (sets null).
func (c *Client) Assign(ctx context.Context, issue Issue, assigneeID string) error {
	if issue.ID == "" {
		return fmt.Errorf("missing issue id for %s (refresh and retry)", issue.Identifier)
	}
	var assignee any // null unassigns
	if assigneeID != "" {
		assignee = assigneeID
	}
	raw, err := c.postGraphQL(ctx, issueAssignMutation, map[string]any{"id": issue.ID, "assigneeId": assignee})
	if err != nil {
		return err
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode issueUpdate: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("%s (a write-scoped LINEAR_API_KEY is required)", resp.Errors[0].Message)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("linear rejected the assignee change")
	}
	return nil
}

const issueAssignMutation = `
mutation Assign($id: String!, $assigneeId: String) {
  issueUpdate(id: $id, input: { assigneeId: $assigneeId }) { success }
}`

// SetPriority sets an issue's priority (0=None, 1=Urgent, 2=High, 3=Medium,
// 4=Low).
func (c *Client) SetPriority(ctx context.Context, issue Issue, priority int) error {
	if issue.ID == "" {
		return fmt.Errorf("missing issue id for %s (refresh and retry)", issue.Identifier)
	}
	raw, err := c.postGraphQL(ctx, issuePriorityMutation, map[string]any{"id": issue.ID, "priority": priority})
	if err != nil {
		return err
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode issueUpdate: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("%s (a write-scoped LINEAR_API_KEY is required)", resp.Errors[0].Message)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("linear rejected the priority change")
	}
	return nil
}

const issuePriorityMutation = `
mutation SetPriority($id: String!, $priority: Int!) {
  issueUpdate(id: $id, input: { priority: $priority }) { success }
}`

// Users lists active workspace members for the assignee picker.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	raw, err := c.postGraphQL(ctx, usersQuery, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Users struct {
				Nodes []struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
					Email       string `json:"email"`
				} `json:"nodes"`
			} `json:"users"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("%s", resp.Errors[0].Message)
	}
	out := make([]User, 0, len(resp.Data.Users.Nodes))
	for _, n := range resp.Data.Users.Nodes {
		out = append(out, User{ID: n.ID, Name: n.Name, DisplayName: n.DisplayName, Email: n.Email})
	}
	return out, nil
}

const usersQuery = `
query Users {
  users(first: 250, filter: { active: { eq: true } }) {
    nodes { id name displayName email }
  }
}`

const teamStatesQuery = `
query TeamStates($teamId: String!) {
  team(id: $teamId) { states { nodes { id name type } } }
}`

// resolveStateID finds the target workflow state on the issue's team. Done and
// Canceled match by state type (prefer an exact name match); Validate matches by
// name (it's a custom state), so it errors clearly when the team has none.
func (c *Client) resolveStateID(ctx context.Context, teamID, target string) (id, name string, err error) {
	raw, err := c.postGraphQL(ctx, teamStatesQuery, map[string]any{"teamId": teamID})
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Data struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("decode team states: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", "", fmt.Errorf("%s", resp.Errors[0].Message)
	}
	states := resp.Data.Team.States.Nodes

	var wantType string
	switch strings.ToLower(target) {
	case "done":
		wantType = "completed"
	case "canceled", "cancelled":
		wantType = "canceled"
	}

	// Exact name match first (case-insensitive) — handles "Validate" and picks
	// the intended Done when a team has several completed-type states.
	for _, s := range states {
		if strings.EqualFold(s.Name, target) {
			return s.ID, s.Name, nil
		}
	}
	// Fall back to the first state of the wanted type (Done/Canceled).
	if wantType != "" {
		for _, s := range states {
			if s.Type == wantType {
				return s.ID, s.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no %q state on this team", target)
}
