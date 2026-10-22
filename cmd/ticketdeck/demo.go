package main

import (
	"context"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
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
		{Identifier: "DEMO-110", Title: "Retry budget for the label printer client", Priority: 3, PrioLabel: "Medium", StateName: "In Progress", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T15:10:00Z"},
		{Identifier: "DEMO-111", Title: "Refresh the quarterly tax tables", Priority: 3, PrioLabel: "Medium", StateName: "Todo", StateType: "unstarted", TeamKey: "DEMO", UpdatedAt: "2026-07-16T14:10:00Z"},
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

// DemoAccounts fabricates four subscriptions, so --demo shows the accounts line,
// the account column and the router on a machine that has one.
func (demoFetcher) DemoAccounts() []account.Account {
	return []account.Account{
		{Name: "matt", ConfigDir: "/demo/.claude"},
		{Name: "m0", ConfigDir: "/demo/.claude-m0"},
		{Name: "m1", ConfigDir: "/demo/.claude-m1"},
		{Name: "support", ConfigDir: "/demo/.claude-support"},
	}
}

// DemoQuotas fabricates usage across the range the router cares about: room
// to spare, nearly spent, out until a reset, and unreadable.
func (demoFetcher) DemoQuotas() map[string]quota.Entry {
	now := time.Now()
	use := func(fh, sd float64) *quota.Usage {
		return &quota.Usage{FiveHourPct: fh, FiveHourReset: now.Add(2 * time.Hour), SevenDayPct: sd, SevenDayReset: now.Add(4 * 24 * time.Hour)}
	}
	return map[string]quota.Entry{
		"matt":    {Usage: use(52, 42), FetchedAt: now},
		"m0":      {Usage: use(93, 61), FetchedAt: now},
		"m1":      {Reason: "token expired"},
		"support": {Usage: use(12, 8), FetchedAt: now},
	}
}

// DemoFleet fabricates sessions in every state the deck shows: working on
// several accounts, waiting on a permission, finished and unread, cut off by
// a usage limit, running in an old per-account deck's workspace, and ad-hoc
// sessions both live and resumable.
func (demoFetcher) DemoFleet(now time.Time) fleet.Snapshot {
	dir := func(a string) string {
		if a == "matt" {
			return "/demo/.claude"
		}
		return "/demo/.claude-" + a
	}
	s := func(key, acct string, st session.Status) fleet.Session {
		kind := ledger.Ticket
		if len(key) > 5 && key[:5] == "proj-" {
			kind = ledger.Project
		}
		return fleet.Session{Key: key, ID: session.DeterministicID(key), Kind: kind, Account: acct, ConfigDir: dir(acct), Status: st, Live: true, Home: true, PaneID: "w1:p1", LastActive: now.Add(-2 * time.Minute)}
	}
	working := s("DEMO-101", "m0", session.Working)
	working.Activity.InFlight.Agents = 2
	working.Activity.Tail = session.Tail{LastTool: "Agent: review the checkout fix", ContextTokens: 142000, Model: "claude-opus-5-5"}
	asking := s("DEMO-102", "support", session.NeedsInput)
	asking.WaitingFor = "permission to run git push"
	elsewhere := s("DEMO-103", "support", session.Working)
	elsewhere.Home, elsewhere.Socket, elsewhere.Workspace = false, "/demo/.config/herdr/sessions/support/herdr.sock", "support"
	elsewhere.LastActive = now.Add(-11 * time.Minute)
	resumable := s("DEMO-104", "matt", session.Stopped)
	resumable.Live, resumable.Home, resumable.LastActive = false, false, now.Add(-5*time.Hour)
	background := s("DEMO-105", "matt", session.Background)
	background.Activity.InFlight.Tasks = 1
	background.Activity.Tail.LastTool = "Bash: gh pr checks 912 --watch"
	unread := s("DEMO-110", "support", session.Unread)
	unread.Activity.Tail = session.Tail{LastTool: "Edit: policy_sync.go", ContextTokens: 61000}
	limited := s("DEMO-111", "m0", session.Limited)
	limited.Activity.Tail.Err = &session.APIError{Kind: "rate_limit", LimitType: "five_hour", ResetsAt: now.Add(40 * time.Minute), UUID: "demo"}
	project := s("proj-demo0001", "matt", session.Working)
	project.Activity.Tail.LastTool = "Read: onboarding_flow.go"
	stoppedProj := s("proj-demo0003", "support", session.Stopped)
	stoppedProj.Live, stoppedProj.Home, stoppedProj.LastActive = false, false, now.Add(-3*time.Hour)

	notes := fleet.Session{Key: "deploy-notes", Name: "Deploy notes", Kind: ledger.Scratch, ID: "11111111-2222-4333-8444-555555555555", Account: "support", ConfigDir: dir("support"), Status: session.Idle, Live: true, Home: true, PaneID: "w1:p9"}
	toolchain := fleet.Session{Key: "toolchain", Name: "bump the go toolchain", Kind: ledger.Scratch, ID: "22222222-2222-4333-8444-555555555555", Account: "m0", ConfigDir: dir("m0"), Status: session.Stopped, LastActive: now.Add(-20 * time.Hour)}
	oldDeck := fleet.Session{Key: "scratch-2", Name: "flaky test triage", Kind: ledger.Scratch, Account: "m1", ConfigDir: dir("m1"), Status: session.Idle, Live: true, Socket: "/demo/.config/herdr/sessions/m1/herdr.sock", Workspace: "m1", PaneID: "w1:p4"}
	offList := s("DEMO-090", "matt", session.Idle)

	return fleet.Snapshot{At: now, Sessions: []fleet.Session{
		working, asking, elsewhere, resumable, background, unread, limited, project, stoppedProj, notes, toolchain, oldDeck, offList,
	}}
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
