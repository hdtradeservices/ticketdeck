package main

import (
	"context"

	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// demoFetcher returns canned, generic data so the TUI can be exercised without a
// Linear API key (`--demo`). It covers every priority, a range of statuses,
// session badges, linked PRs, and a validation label.
type demoFetcher struct{}

func (demoFetcher) FetchAssignedOpen(context.Context) ([]linear.Issue, error) {
	return []linear.Issue{
		{Identifier: "DEMO-101", Title: "[Bug] Checkout total ignores expired coupons", Description: "Steps:\n1. Add an item to the cart.\n2. Apply an expired coupon code.\n\nExpected: the coupon is rejected and the total is unchanged.\nActual: the discount is still applied.", URL: "https://linear.app/acme/issue/DEMO-101", Priority: 1, PrioLabel: "Urgent", StateName: "In Progress", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:14:31Z"},
		{Identifier: "DEMO-102", Title: "[Bug] Webhook retries dead-letter on 400 from provider", Priority: 2, PrioLabel: "High", StateName: "In Review", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T17:50:37Z", PRs: []linear.PR{{URL: "https://github.com/acme/widgets/pull/241", Title: "fix: tolerate provider 400 in webhook retrier", State: "open"}}},
		{Identifier: "DEMO-103", Title: "[Bug] Order create times out (>60s) under load", Priority: 2, PrioLabel: "High", StateName: "Planned", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:26:28Z", Labels: []string{"Bug", "validation-inconclusive"}},
		{Identifier: "DEMO-104", Title: "Add import mapping for the new product type", Priority: 2, PrioLabel: "High", StateName: "Todo", StateType: "unstarted", TeamKey: "DEMO", UpdatedAt: "2026-07-16T18:22:58Z"},
		{Identifier: "DEMO-105", Title: "Autoscaler fights manual replica count on deploy", Priority: 2, PrioLabel: "High", StateName: "In Review", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T17:13:09Z"},
		{Identifier: "DEMO-106", Title: "Sync seller policies and expose per-store valid values", Priority: 3, PrioLabel: "Medium", StateName: "Planned", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T15:39:03Z"},
		{Identifier: "DEMO-107", Title: "Support inline shipping / return policies", Priority: 3, PrioLabel: "Medium", StateName: "Todo", StateType: "unstarted", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:22:49Z"},
		{Identifier: "DEMO-108", Title: "Durable observability for stale-write blocks", Priority: 4, PrioLabel: "Low", StateName: "Merged", StateType: "started", TeamKey: "DEMO", UpdatedAt: "2026-07-16T16:14:49Z", PRs: []linear.PR{{URL: "https://github.com/acme/widgets/pull/188", Title: "feat: stale-write log-based metric", State: "merged"}}},
		// A Duplicate-typed ticket — must be filtered out of the view (BR-2a).
		{Identifier: "DEMO-109", Title: "dup of DEMO-101 (should not appear)", Priority: 2, PrioLabel: "High", StateName: "Duplicate", StateType: "duplicate", TeamKey: "DEMO", UpdatedAt: "2026-07-16T10:00:00Z"},
	}, nil
}

// DemoSessions fabricates session statuses so --demo shows the badges without a
// live Claude daemon matching these ticket keys.
func (demoFetcher) DemoSessions() map[string]session.Status {
	return map[string]session.Status{
		"DEMO-101": session.Working,
		"DEMO-102": session.NeedsInput,
		"DEMO-103": session.Idle,
		"DEMO-106": session.Completed,
		"DEMO-104": session.Stopped,
	}
}

// DemoOtherSessions fabricates sessions not tied to a visible ticket (an off-list
// done ticket + an ad-hoc scratch session) so --demo shows the bottom section.
func (demoFetcher) DemoOtherSessions() []session.SessionRef {
	return []session.SessionRef{
		{Name: "DEMO-090", Status: session.Idle},
		{Name: "scratch-1", Status: session.Working},
	}
}
