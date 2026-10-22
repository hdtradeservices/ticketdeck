package herd

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeServer answers events.subscribe the way herdr 0.9.1 does (verified
// against a throwaway server, 2026-10-22): an acknowledgement line, then one
// line per event.
type fakeServer struct {
	socket string
	subs   chan sub
}

type sub struct {
	types []string
	panes []string
	conn  net.Conn
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	// Unix socket paths are short; t.TempDir() under a long package path can
	// overrun them.
	dir, err := filepathTemp(t)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{socket: filepath.Join(dir, "h.sock"), subs: make(chan sub, 16)}
	l, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					Method string `json:"method"`
					Params struct {
						Subscriptions []map[string]string `json:"subscriptions"`
					} `json:"params"`
				}
				_ = json.Unmarshal(line, &req)
				var s sub
				for _, x := range req.Params.Subscriptions {
					s.types = append(s.types, x["type"])
					if p := x["pane_id"]; p != "" {
						s.panes = append(s.panes, p)
					}
				}
				s.conn = c
				_, _ = c.Write([]byte(`{"id":"ticketdeck","result":{"type":"subscription_started"}}` + "\n"))
				f.subs <- s
			}()
		}
	}()
	return f
}

func (f *fakeServer) next(t *testing.T) sub {
	t.Helper()
	select {
	case s := <-f.subs:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no subscription arrived")
		return sub{}
	}
}

func filepathTemp(t *testing.T) (string, error) {
	dir := filepath.Join("/tmp", "td-ev-"+time.Now().Format("150405.000000"))
	if err := mkdir(dir); err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = removeAll(dir) })
	return dir, nil
}

func TestWatchFollowsAgentsAndReconnects(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	panes := []string{"w1:p1"}
	old, oldDebounce := agentPanes, debounce
	agentPanes = func(string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(panes), nil
	}
	debounce = 10 * time.Millisecond
	t.Cleanup(func() { agentPanes, debounce = old, oldDebounce })

	changed := make(chan struct{}, 16)
	up := make(chan bool, 16)
	watch(t, f.socket, func() { changed <- struct{}{} }, func(b bool) { up <- b })

	life := f.next(t)
	if !slices.Equal(life.types, lifecycle) {
		t.Fatalf("lifecycle subscription = %v", life.types)
	}
	status := f.next(t)
	if !slices.Equal(status.panes, []string{"w1:p1"}) || status.types[0] != "pane.agent_status_changed" {
		t.Fatalf("status subscription = %+v", status)
	}
	expect := func(what string, ch any) {
		t.Helper()
		switch c := ch.(type) {
		case chan struct{}:
			select {
			case <-c:
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s", what)
			}
		case chan bool:
			select {
			case <-c:
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s", what)
			}
		}
	}
	expect("up", up)

	// A status change, then a burst of them, each burst one call.
	_, _ = status.conn.Write([]byte(`{"data":{"agent":"claude","agent_status":"working","pane_id":"w1:p1","workspace_id":"w1"},"event":"pane.agent_status_changed"}` + "\n"))
	expect("changed after a status event", changed)

	// A new agent: the status subscription is renewed to follow it too.
	mu.Lock()
	panes = []string{"w1:p1", "w1:p2"}
	mu.Unlock()
	_, _ = life.conn.Write([]byte(`{"data":{"agent":"claude","pane_id":"w1:p2","type":"pane_agent_detected","workspace_id":"w1"},"event":"pane_agent_detected"}` + "\n"))
	renewed := f.next(t)
	if !slices.Equal(renewed.panes, []string{"w1:p1", "w1:p2"}) {
		t.Fatalf("renewed subscription = %+v", renewed.panes)
	}
	expect("changed after an agent appeared", changed)
	// The old status connection is closed once the new one is live.
	_ = status.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(status.conn).ReadByte(); err == nil {
		t.Error("the old status subscription is still open")
	}

	// The server drops the lifecycle connection: down, changed, and back.
	_ = life.conn.Close()
	select {
	case b := <-up:
		if b {
			t.Fatal("up(true) before up(false)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no up(false) after the connection dropped")
	}
	expect("changed after the drop", changed)
	if again := f.next(t); !slices.Equal(again.types, lifecycle) {
		t.Errorf("resubscribed with %v", again.types)
	}
}

// events_lost is herdr closing a subscription it fell behind on; the watcher
// treats it as a drop.
func TestWatchTreatsEventsLostAsADrop(t *testing.T) {
	f := newFakeServer(t)
	old := agentPanes
	agentPanes = func(string) ([]string, error) { return nil, nil }
	t.Cleanup(func() { agentPanes = old })
	up := make(chan bool, 16)
	watch(t, f.socket, func() {}, func(b bool) { up <- b })
	life := f.next(t)
	if <-up != true {
		t.Fatal("not up")
	}
	_, _ = life.conn.Write([]byte(`{"id":"ticketdeck","error":{"code":"events_lost","message":"subscriber fell behind"}}` + "\n"))
	select {
	case b := <-up:
		if b {
			t.Fatal("still up after events_lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events_lost didn't drop the subscription")
	}
}

// watch runs Watch until the test ends, and waits for it to stop before the
// test's other cleanups put back what it reads.
func watch(t *testing.T, socket string, changed func(), up func(bool)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, socket, changed, up)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func mkdir(p string) error     { return os.MkdirAll(p, 0o700) }
func removeAll(p string) error { return os.RemoveAll(p) }
