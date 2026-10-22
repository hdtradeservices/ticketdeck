package account

import (
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// Owner is the subscription running a ticket's session — what the deck labels a
// row with, so you can see whose rate limits a session is spending, and whether
// it runs somewhere this deck can't drive directly.
type Owner struct {
	Name       string         // account whose subscription runs the session
	Status     session.Status // live status, when a workspace answered
	LastActive time.Time      // transcript mtime: when the session last wrote
	Live       bool           // Status came from a running session, not from disk
	// Home: it runs in this deck's own workspace. A live session that isn't
	// home runs in another deck's workspace (or a plain terminal), where this
	// deck can see it but opening it here would fork it.
	Home bool
	// Workspace names where a live session that isn't home runs.
	Workspace string
}
