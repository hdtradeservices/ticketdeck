package herd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Watch subscribes to a herdr server's events and calls changed whenever
// something the deck shows may have moved: a pane opened or closed, an agent
// appeared, or an agent's status changed. It runs until ctx ends. up says
// whether the subscription is live; while it is not, the caller should poll.
//
// Events say that something changed, not what it is now — herdr's own advice
// is to treat them as a cue for an authoritative read — so changed carries no
// payload, and a burst of events within debounce calls it once.
//
// herdr only sends status changes for panes named in the subscription, so the
// watcher keeps two subscriptions: one to the pane lifecycle, and one to the
// status of each agent pane, renewed whenever an agent appears or a pane goes.
// A connection that drops, or a server that reports events_lost, is
// resubscribed after a pause, and changed is called then too: whatever happened
// in between was missed.
func Watch(ctx context.Context, socket string, changed func(), up func(bool)) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := watchOnce(ctx, socket, changed, up)
		up(false)
		if ctx.Err() != nil {
			return
		}
		changed()
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if watchLog != nil {
			watchLog(fmt.Sprintf("herdr events: %v; resubscribing in %v", err, backoff))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// watchLog, when set, hears why a subscription dropped.
var watchLog func(string)

// debounce gathers a burst of events into one call.
var debounce = 150 * time.Millisecond

// lifecycle is what the pane-lifecycle subscription asks for. Not
// pane.updated: it fires on terminal-title changes, which a busy session makes
// often and which say nothing the status events don't.
var lifecycle = []string{"pane.created", "pane.closed", "pane.exited", "pane.agent_detected"}

// agentPanes lists the panes whose status to follow; tests replace it.
var agentPanes = func(socket string) ([]string, error) {
	agents, err := ListAt(socket)
	if err != nil {
		return nil, err
	}
	var panes []string
	for _, a := range agents {
		if a.PaneID != "" {
			panes = append(panes, a.PaneID)
		}
	}
	return panes, nil
}

func watchOnce(ctx context.Context, socket string, changed func(), up func(bool)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan string, 64)
	errs := make(chan error, 4)

	subs := make([]map[string]string, 0, len(lifecycle))
	for _, t := range lifecycle {
		subs = append(subs, map[string]string{"type": t})
	}
	life, err := subscribe(ctx, socket, subs, events, errs)
	if err != nil {
		return err
	}
	defer life.close()

	var status *subscription
	follow := func() error {
		// A pane can close between the listing and the subscription, which
		// herdr refuses whole; list again and retry.
		var err error
		for range 3 {
			var panes []string
			if panes, err = agentPanes(socket); err != nil {
				return err
			}
			var next *subscription
			if len(panes) > 0 {
				subs := make([]map[string]string, 0, len(panes))
				for _, p := range panes {
					subs = append(subs, map[string]string{"type": "pane.agent_status_changed", "pane_id": p})
				}
				if next, err = subscribe(ctx, socket, subs, events, errs); err != nil {
					continue
				}
			}
			// The new subscription is live before the old one goes, so no
			// status change falls between them.
			if status != nil {
				status.close()
			}
			status = next
			return nil
		}
		return err
	}
	if err := follow(); err != nil {
		return err
	}
	defer func() {
		if status != nil {
			status.close()
		}
	}()
	up(true)

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			return err
		case ev := <-events:
			switch ev {
			case "pane_agent_detected", "pane_closed", "pane_exited":
				if err := follow(); err != nil {
					return err
				}
			}
			if !pending {
				pending = true
				timer.Reset(debounce)
			}
		case <-timer.C:
			pending = false
			changed()
		}
	}
}

// subscription is one open events.subscribe connection.
type subscription struct {
	cancel context.CancelFunc
}

func (s *subscription) close() { s.cancel() }

// subscribe opens a connection to the server's socket, subscribes, and reads
// events into events until it is closed. An error after the subscription
// started goes to errs, unless the subscription was closed on purpose.
func subscribe(ctx context.Context, socket string, subs []map[string]string, events chan<- string, errs chan<- error) (*subscription, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	req, err := json.Marshal(map[string]any{
		"id": "ticketdeck", "method": "events.subscribe",
		"params": map[string]any{"subscriptions": subs},
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(req, '\n')); err != nil {
		_ = conn.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var ack struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &ack); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack.Error != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("events.subscribe: %s: %s", ack.Error.Code, ack.Error.Message)
	}
	_ = conn.SetDeadline(time.Time{})

	sctx, cancel := context.WithCancel(ctx)
	go func() {
		<-sctx.Done()
		_ = conn.Close()
	}()
	go func() {
		defer cancel()
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				report(sctx, errs, err)
				return
			}
			var ev struct {
				Event string `json:"event"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if ev.Error != nil {
				// events_lost: the server dropped events this reader was too slow
				// for, and closes the connection after saying so.
				report(sctx, errs, errors.New("herdr: "+ev.Error.Code))
				return
			}
			if ev.Event == "" {
				continue
			}
			select {
			case events <- ev.Event:
			case <-sctx.Done():
				return
			}
		}
	}()
	return &subscription{cancel: cancel}, nil
}

func report(ctx context.Context, errs chan<- error, err error) {
	if ctx.Err() != nil {
		return // closed on purpose
	}
	select {
	case errs <- err:
	default:
	}
}

// SetWatchLog has Watch report why a subscription dropped.
func SetWatchLog(f func(string)) { watchLog = f }
