package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
	"github.com/hdtradeservices/ticketdeck/internal/route"
	"github.com/hdtradeservices/ticketdeck/internal/session"
	"github.com/hdtradeservices/ticketdeck/internal/tui"
)

// runSessions implements `ticketdeck sessions`: every account's headroom and
// every session the deck would show — running on any account, in any herdr
// workspace on the machine, or remembered and resumable — without opening the
// deck. Read-only, and it never calls Linear, so it answers in a second over
// ssh too (`rdeck --status` uses it).
func runSessions(w io.Writer) error {
	now := time.Now()
	accts := account.All()
	qa := make([]quota.Account, 0, len(accts))
	for _, a := range accts {
		qa = append(qa, quota.Account{Name: a.Name, ConfigDir: a.ConfigDir})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	readings := quota.Poll(ctx, qa, now)

	led := ledger.Load()
	// Without Linear the deck's own ticket list is unknown, so the tickets and
	// projects the deck has opened sessions for stand in for it.
	var keys []string
	for _, e := range led {
		if (e.Kind == ledger.Ticket || e.Kind == ledger.Project) && e.Key != "" && !e.Forgotten {
			keys = append(keys, e.Key)
		}
	}
	home := account.Current()
	snap := fleet.Poll(fleet.Input{
		Accounts: accts, Home: home, Keys: keys, Ledger: led,
		Herdr: herd.Available(), Peers: true, Now: now,
	})
	cands := snap.Candidates(accts, readings, home, now)
	next := route.Pick(cands, route.Request{Pin: tui.Pin(), Now: now})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\t5H\t7D\tAT THIS PACE\tSESSIONS\tSTATE")
	for _, r := range route.Rank(cands, now) {
		five, seven, pace := "—", "—", "—"
		if r.Usage != nil {
			five, seven = fmt.Sprintf("%.0f%%", r.Usage.FiveHourPct), fmt.Sprintf("%.0f%%", r.Usage.SevenDayPct)
			switch t, ok := readings[r.Name].WeekRunsOut(now); {
			case r.Usage.SevenDayPct >= 100:
				pace = "spent"
			case ok:
				pace = "out " + route.Clock(t, now)
			default:
				pace = "lasts the week"
			}
		}
		state := r.Note
		if r.Verdict == route.Usable {
			state = "room"
		}
		if r.Name == next.Account {
			state += "  ← next new session"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, five, seven, pace, r.Live, state)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	sessions := snap.Sessions
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Live != sessions[j].Live {
			return sessions[i].Live
		}
		return sessions[i].Key < sessions[j].Key
	})
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tACCOUNT\tWHERE\tSTATUS\tLAST WROTE\tDOING")
	for _, s := range sessions {
		where := "this deck"
		switch {
		case !s.Live:
			where = "on disk"
		case !s.Home && s.Workspace != "":
			where = s.Workspace + " deck"
		case !s.Home:
			where = "elsewhere"
		}
		name := s.Name
		if name == "" || name == s.Key {
			name = s.Key
		} else {
			name = s.Key + " (" + name + ")"
		}
		last := "—"
		if !s.LastActive.IsZero() {
			last = s.LastActive.Local().Format("Jan 2 15:04")
		}
		doing := s.Activity.Tail.LastTool
		if e := s.Activity.Tail.Err; e != nil {
			doing = strings.TrimSpace(strings.SplitN(e.Text, "\n", 2)[0])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, s.Account, where, statusWord(s.Status), last, doing)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if snap.OwnErr != nil && herd.Alive(herd.OwnSocket()) {
		fmt.Fprintf(w, "\n(this shell's herdr workspace didn't answer: %v)\n", snap.OwnErr)
	}
	return nil
}

func statusWord(s session.Status) string {
	switch s {
	case session.Working:
		return "working"
	case session.NeedsInput:
		return "needs input"
	case session.Idle:
		return "idle"
	case session.Background:
		return "background"
	case session.Unread:
		return "your turn"
	case session.Limited:
		return "out of usage"
	case session.Errored:
		return "error"
	case session.Stopped, session.Completed:
		return "resumable"
	}
	return "—"
}

func init() {
	subcommands["sessions"] = func([]string) error { return runSessions(os.Stdout) }
}
