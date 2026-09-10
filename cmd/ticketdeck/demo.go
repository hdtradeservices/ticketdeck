package main

import (
	"context"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// demoFetcher returns canned, generic data so the TUI can be exercised without a
// Linear API key (`--demo`). It covers every priority, a range of statuses,
// session badges, linked PRs, and a validation label.
type demoFetcher struct{}

func (demoFetcher) FetchAssignedOpen(context.Context) ([]linear.Issue, error) {
	return []linear.Issue{
		{ID: "demo-101", Identifier: "DEMO-101", Title: "[Bug] Checkout total ignores expired coupons", Description: "Steps:\n1. Add an item to the cart.\n2. Apply an expired coupon code.\n\nExpected: the coupon is rejected and the total is unchanged.\nActual: the discount is still applied.", URL: "https://linear.app/acme/issue/DEMO-101", Priority: 1, PrioLabel: "Urgent", StateName: "In Progress", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:14:31Z"},
		{Identifier: "DEMO-102", Title: "[Bug] Webhook retries dead-letter on 400 from provider", Priority: 2, PrioLabel: "High", StateName: "In Review", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T17:50:37Z", PRs: []linear.PR{{URL: "https://github.com/acme/widgets/pull/241", Title: "fix: tolerate provider 400 in webhook retrier", State: "open"}}},
		{Identifier: "DEMO-103", Title: "[Bug] Order create times out (>60s) under load", Priority: 2, PrioLabel: "High", StateName: "Planned", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:26:28Z", Labels: []string{"Bug", "validation-inconclusive"}},
		{Identifier: "DEMO-104", Title: "Add import mapping for the new product type", Priority: 2, PrioLabel: "High", StateName: "Todo", StateType: "unstarted", TeamKey: "DEMO", UpdatedAt: "2026-07-16T18:22:58Z", ProjectID: "demo-p1", ProjectName: "Channel onboarding v2", ProjectSlugID: "demo0001"},
		// Several PRs across repos — the common multi-PR shape, where `p` opens
		// the picker instead of guessing a link.
		{Identifier: "DEMO-105", Title: "Autoscaler fights manual replica count on deploy", Priority: 2, PrioLabel: "High", StateName: "In Review", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T17:13:09Z", ProjectID: "demo-p2", ProjectName: "Platform reliability", ProjectSlugID: "demo0002", PRs: []linear.PR{
			{URL: "https://github.com/acme/platform/pull/912", Title: "fix(autoscaler): stop reconciling manual replica overrides", State: "open", Repo: "platform", Number: 912},
			{URL: "https://github.com/acme/widgets/pull/244", Title: "chore(deploy): drop the hardcoded replica count", State: "merged", Repo: "widgets", Number: 244},
			{URL: "https://github.com/acme/charts/pull/57", Title: "feat(charts): expose replica policy in values.yaml", State: "draft", Repo: "charts", Number: 57},
		}},
		{Identifier: "DEMO-106", Title: "Sync seller policies and expose per-store valid values", Priority: 3, PrioLabel: "Medium", StateName: "Planned", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T15:39:03Z", ProjectID: "demo-p1", ProjectName: "Channel onboarding v2", ProjectSlugID: "demo0001"},
		{Identifier: "DEMO-107", Title: "Support inline shipping / return policies", Priority: 3, PrioLabel: "Medium", StateName: "Todo", StateType: "unstarted", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:22:49Z", ProjectID: "demo-p1", ProjectName: "Channel onboarding v2", ProjectSlugID: "demo0001"},
		{Identifier: "DEMO-108", Title: "Durable observability for stale-write blocks", Priority: 4, PrioLabel: "Low", StateName: "Merged", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:14:49Z", ProjectID: "demo-p2", ProjectName: "Platform reliability", ProjectSlugID: "demo0002", PRs: []linear.PR{{URL: "https://github.com/acme/widgets/pull/188", Title: "feat: stale-write log-based metric", State: "merged"}}},
		// A Duplicate-typed ticket — must be filtered out of the view (BR-2a).
		{Identifier: "DEMO-109", Title: "dup of DEMO-101 (should not appear)", Priority: 2, PrioLabel: "High", StateName: "Duplicate", StateType: "duplicate", TeamKey: "DEMO", UpdatedAt: "2026-07-16T10:00:00Z"},
	}, nil
}

// FetchMyProjects fabricates the projects I lead, so --demo shows the Projects
// section — including a project surfaced only because it holds my tickets
// (Platform reliability is deliberately absent from this list).
func (demoFetcher) FetchMyProjects(context.Context) ([]linear.Project, error) {
	return []linear.Project{
		{
			ID: "demo-p1", Name: "Channel onboarding v2", SlugID: "demo0001",
			URL:      "https://linear.app/acme/project/channel-onboarding-v2-demo0001",
			Summary:  "Bring three new sales channels onto the shared onboarding flow, retiring the per-channel forks.",
			Content:  "## Goal\n\nOne onboarding flow for every channel.\n\n### Milestones\n1. Shared mapping model\n2. Per-store policy sync\n3. Retire the forks",
			Priority: 2, PrioLabel: "High",
			Progress: 0.42, Scope: 58, Health: "atRisk",
			StatusName: "In Progress", StatusType: "started",
			StartDate: "2026-07-01", TargetDate: "2026-09-30",
			LeadName: "You", TeamKeys: []string{"DEMO"}, Mine: true,
			UpdatedAt: "2026-07-16T18:00:00Z",
		},
		{
			ID: "demo-p3", Name: "Billing statement redesign", SlugID: "demo0003",
			URL:      "https://linear.app/acme/project/billing-statement-redesign-demo0003",
			Summary:  "Rebuild the monthly statement so line items reconcile against the ledger.",
			Priority: 3, PrioLabel: "Medium",
			Progress: 0.9, Scope: 30,
			StatusName: "In Progress", StatusType: "started",
			TargetDate: "2026-07-01", // in the past → the overdue flag
			LeadName:   "You", TeamKeys: []string{"DEMO"}, Mine: true,
			UpdatedAt: "2026-07-15T12:00:00Z",
		},
		{
			// Finished within the last few hours: it lingers struck through, then
			// drops off once DoneVisibleFor is up.
			ID: "demo-p4", Name: "Tax engine cutover", SlugID: "demo0004",
			URL:      "https://linear.app/acme/project/tax-engine-cutover-demo0004",
			Summary:  "Move tax calculation onto the shared engine and retire the legacy tables.",
			Priority: 1, PrioLabel: "Urgent",
			Progress: 1, Scope: 21,
			StatusName: "Completed", StatusType: "completed",
			CompletedAt: time.Now().Add(-3 * time.Hour),
			LeadName:    "You", TeamKeys: []string{"DEMO"}, Mine: true,
			UpdatedAt: "2026-07-16T09:00:00Z",
		},
	}, nil
}

// DemoSessions fabricates session statuses so --demo shows the badges without a
// live Claude daemon matching these ticket keys.
func (demoFetcher) DemoSessions() map[string]session.Status {
	return map[string]session.Status{
		"DEMO-101":      session.Working,
		"DEMO-102":      session.NeedsInput,
		"DEMO-106":      session.Completed,
		"DEMO-104":      session.Stopped,
		"proj-demo0001": session.Working,
		"proj-demo0003": session.Stopped,
		// DEMO-103 is deliberately absent: its session runs on another deck, which
		// this deck's own backend can't see. DemoOwners is what badges it.
	}
}

// DemoOwners fabricates which subscription runs each session, so --demo shows
// the account column on a machine that only has one Claude subscription.
func (demoFetcher) DemoOwners() map[string]account.Owner {
	return map[string]account.Owner{
		"DEMO-101": {Name: "default", Status: session.Working, Live: true},
		"DEMO-102": {Name: "support", Status: session.NeedsInput, Live: true},
		// Only the other deck has this one, so its row is the cross-deck case: the
		// badge is that deck's live status, colored like the deck rather than the
		// status, and pressing ⏎ on it hits the one-deck-per-ticket gate.
		"DEMO-103": {Name: "support", Status: session.Working, Live: true, LastActive: time.Now().Add(-11 * time.Minute)},
		"DEMO-104": {Name: "default", Status: session.Stopped},
		"DEMO-106": {Name: "support", Status: session.Completed},
		// A project session held by the other deck: the row is what tells you a
		// project is already being worked, and by whom, without switching decks.
		"proj-demo0001": {Name: "default", Status: session.Working, Live: true},
		"proj-demo0003": {Name: "support", Status: session.Stopped, LastActive: time.Now().Add(-3 * time.Hour)},
	}
}

// FetchComments fabricates the /investigate and /plan write-ups for DEMO-101, so
// the overlay's `i` and `P` views have something to show without a Linear key.
func (demoFetcher) FetchComments(_ context.Context, issueID string) ([]linear.Comment, error) {
	if issueID != "demo-101" {
		return nil, nil
	}
	return []linear.Comment{
		{
			ID:        "c1",
			Author:    "Claude",
			CreatedAt: time.Date(2026, 7, 16, 9, 12, 0, 0, time.UTC),
			Body: `## Investigation summary

**Verdict:** confirmed — expired coupons are applied when the cart is repriced.

### Expected vs actual
Expected the coupon to be rejected. The reprice path skips the expiry check.

<!-- investigate-summary -->`,
		},
		{
			ID:        "c2",
			Author:    "Claude",
			CreatedAt: time.Date(2026, 7, 16, 10, 3, 0, 0, time.UTC),
			Body: `## Implementation plan

### Plan (ordered)
1. Move the expiry check into the shared coupon validator.
2. Call it from the reprice path.
3. Add a regression test for an expired coupon at reprice time.

<!-- implementation-plan -->`,
		},
	}, nil
}

// DemoOtherSessions fabricates sessions not tied to a visible ticket (an off-list
// done ticket + an ad-hoc scratch session) so --demo shows the bottom section.
func (demoFetcher) DemoOtherSessions() []session.SessionRef {
	return []session.SessionRef{
		{Name: "DEMO-090", Status: session.Idle},
		{Name: "scratch-1", Status: session.Working},
	}
}
