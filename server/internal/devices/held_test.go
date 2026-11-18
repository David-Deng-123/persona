package devices

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/server/internal/apierr"
	"github.com/nwverse/persona/server/internal/db"
	"github.com/nwverse/persona/server/internal/devicewire"
	"github.com/nwverse/persona/server/internal/i18n"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

// heldMem is the settings and held_operations in memory, as db.DB keeps them.
type heldMem struct {
	*memStore
	mu   sync.Mutex
	seq  int64
	rows map[int64]db.HeldOperation
	fail error // every held_operations call fails with it, when set
	// loads counts the loads (RecoverHeldOperations); retryGate, when set, holds a Retry's write until it is closed,
	// and retrying is told when one waits there.
	loads     int
	retryGate chan struct{}
	retrying  chan struct{}
}

func newHeldMem() *heldMem {
	return &heldMem{memStore: newMemStore(), rows: map[int64]db.HeldOperation{}}
}

func (s *heldMem) HoldOperation(_ context.Context, op db.HeldOperation) (db.HeldOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return db.HeldOperation{}, false, s.fail
	}
	for _, r := range s.rows {
		if r.Kind == op.Kind && r.MachineID == op.MachineID && r.SessionID == op.SessionID && r.OpID == op.OpID {
			return r, false, nil
		}
	}
	s.seq++
	op.Seq, op.State, op.Failure, op.Error, op.Attempts, op.TriedAt = s.seq, db.HeldWaiting, "", "", 0, time.Time{}
	op.Call = slices.Clone(op.Call)
	s.rows[op.Seq] = op
	return op, true, nil
}

func (s *heldMem) HeldOperations(_ context.Context, kind string) ([]db.HeldOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	var out []db.HeldOperation
	for _, r := range s.rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b db.HeldOperation) int { return cmp.Compare(a.Seq, b.Seq) })
	return out, nil
}

func (s *heldMem) RecoverHeldOperations(_ context.Context, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.fail != nil {
		return 0, s.fail
	}
	var n int64
	for seq, r := range s.rows {
		if r.Kind == kind && r.State == db.HeldSending {
			r.State, r.Failure, r.Error = db.HeldFailed, db.HeldUnclear, ""
			s.rows[seq] = r
			n++
		}
	}
	return n, nil
}

func (s *heldMem) ClaimHeldOperation(_ context.Context, seq int64, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	r, ok := s.rows[seq]
	if !ok || r.State != db.HeldWaiting {
		return false, nil
	}
	r.State, r.TriedAt, r.Attempts = db.HeldSending, at, r.Attempts+1
	s.rows[seq] = r
	return true, nil
}

func (s *heldMem) SettleHeldOperation(_ context.Context, seq int64, from, state, failure, reason string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	r, ok := s.rows[seq]
	if !ok || r.State != from {
		return false, nil
	}
	r.State, r.Failure, r.Error = state, failure, reason
	s.rows[seq] = r
	return true, nil
}

func (s *heldMem) RetryHeldOperation(_ context.Context, seq int64, at time.Time) (bool, error) {
	s.mu.Lock()
	gate, retrying := s.retryGate, s.retrying
	s.mu.Unlock()
	if gate != nil {
		retrying <- struct{}{}
		<-gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	r, ok := s.rows[seq]
	if !ok {
		return false, nil
	}
	r.State, r.Failure, r.Error, r.HeldAt = db.HeldWaiting, "", "", at
	s.rows[seq] = r
	return true, nil
}

func (s *heldMem) RenewHeldOperation(_ context.Context, seq int64, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	r, ok := s.rows[seq]
	if !ok || r.State != db.HeldWaiting {
		return false, nil
	}
	r.HeldAt = at
	s.rows[seq] = r
	return true, nil
}

func (s *heldMem) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func (s *heldMem) DeleteHeldOperation(_ context.Context, seq int64, states ...string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	r, ok := s.rows[seq]
	if !ok || len(states) > 0 && !slices.Contains(states, r.State) {
		return false, nil
	}
	delete(s.rows, seq)
	return true, nil
}

// all are the rows, in order.
func (s *heldMem) all() []db.HeldOperation {
	out, _ := (&heldMem{rows: s.snapshot()}).HeldOperations(context.Background(), db.HeldKindMessage)
	return out
}

func (s *heldMem) snapshot() map[int64]db.HeldOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]db.HeldOperation, len(s.rows))
	for k, v := range s.rows {
		out[k] = v
	}
	return out
}

func (s *heldMem) setFail(err error) {
	s.mu.Lock()
	s.fail = err
	s.mu.Unlock()
}

// heldNode is a node in this process that records the messages it is sent; gate, when set, keeps a send waiting
// until it is closed, and answer, when set, is what a send gets.
type heldNode struct {
	mu     sync.Mutex
	sends  []map[string]any
	conn   net.Conn
	peer   *rpc.Peer
	gate   chan struct{}
	answer func(params map[string]any) (any, error)
	listed []string // the sessions it lists; nil: s1 and s2
	// items are what session.items answers, by session; reads counts those calls.
	items map[string][]proto.Item
	reads int
}

func (n *heldNode) itemReads() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reads
}

// list has the node list these sessions from now on and say so (sessions.changed).
func (n *heldNode) list(ids ...string) {
	n.mu.Lock()
	n.listed = ids
	p := n.peer
	n.mu.Unlock()
	_ = p.Notify("sessions.changed", map[string]any{"sessions": n.sessions()})
}

func (n *heldNode) sessions() []proto.Session {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := n.listed
	if ids == nil {
		ids = []string{"s1", "s2"}
	}
	var out []proto.Session
	for _, id := range ids {
		out = append(out, proto.Session{SessionID: id, Status: proto.StatusIdle})
	}
	return out
}

func (n *heldNode) dial(context.Context) (net.Conn, error) {
	managerSide, nodeSide := net.Pipe()
	p := rpc.New("n", nodeSide)
	p.OnRequest = func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "node.info":
			return proto.NodeInfo{Hostname: "box", OS: "linux", Arch: "amd64", Home: "/home/me", Version: "test"}, nil
		case "sessions.list":
			return map[string]any{"sessions": n.sessions()}, nil
		case "session.items":
			var q struct {
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal(params, &q)
			n.mu.Lock()
			n.reads++
			items := n.items[q.SessionID]
			n.mu.Unlock()
			return map[string]any{"items": items}, nil
		case "session.send":
			var m map[string]any
			_ = json.Unmarshal(params, &m)
			n.mu.Lock()
			n.sends = append(n.sends, m)
			gate, answer := n.gate, n.answer
			n.mu.Unlock()
			if gate != nil {
				<-gate
			}
			if answer != nil {
				return answer(m)
			}
			return map[string]any{"ok": true}, nil
		}
		return map[string]any{}, nil
	}
	n.mu.Lock()
	n.conn, n.peer = nodeSide, p
	n.mu.Unlock()
	go func() { _ = p.Serve(context.Background(), nodeSide); nodeSide.Close() }()
	return managerSide, nil
}

// sent are the client_msg_ids of the messages sent so far, in order.
func (n *heldNode) sent() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, m := range n.sends {
		id, _ := m["client_msg_id"].(string)
		out = append(out, id)
	}
	return out
}

func (n *heldNode) drop() {
	n.mu.Lock()
	c := n.conn
	n.mu.Unlock()
	_ = c.Close()
}

// heldDevices is devices that hold messages (an in-memory store, the held loop running), tick its loop's period;
// published counts the rows' republications per machine.
func heldDevices(t *testing.T, st *heldMem, tick time.Duration) (*Devices, *published) {
	t.Helper()
	oldTick := heldTick
	heldTick = tick
	d := newTestDevices(t)
	d.opts.Store = st
	pub := &published{n: map[string]int{}}
	d.opts.HeldChanged = pub.add
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.heldLoop(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		waited := make(chan struct{})
		go func() { d.workers.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Error("the held deliveries did not end")
		}
		heldTick = oldTick
	})
	eventually(t, 5*time.Second, "held messages loaded", func() bool {
		d.held.mu.Lock()
		defer d.held.mu.Unlock()
		return d.held.loaded
	})
	return d, pub
}

type published struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *published) add(machine string) {
	p.mu.Lock()
	p.n[machine]++
	p.mu.Unlock()
}

func (p *published) count(machine string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[machine]
}

// offlineMachine is a machine whose node is not connected (no run loop).
func offlineMachine(d *Devices, id, name string) *Machine {
	m := newMachine(d, MachineConfig{ID: id, Name: name})
	d.addEntry(m)
	return m
}

// laterMachine runs machine id on node n, whose node can be reached only once up is closed.
func laterMachine(t *testing.T, d *Devices, id, name string, n *heldNode, up chan struct{}) *Machine {
	t.Helper()
	m := newMachine(d, MachineConfig{ID: id, Name: name, Kind: "ssh"})
	m.dialNode = func(ctx context.Context) (net.Conn, error) {
		select {
		case <-up:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return n.dial(ctx)
	}
	d.addEntry(m)
	runMachine(t, m)
	return m
}

// heldSend is the hub's send of message id with text to session sid of machine, marked as one that may be held.
func heldSend(machine, sid, id, text string) devicewire.NodeCall {
	params, _ := json.Marshal(map[string]any{"session_id": sid, "client_msg_id": id, "text": text})
	return devicewire.NodeCall{MachineID: machine, Method: "session.send", Params: params, TimeoutMS: 2000, Request: "r-" + id,
		Caller:     &devicewire.Caller{Kind: "device", CredentialID: "phone"},
		Submission: &devicewire.Submission{ID: "sub-" + id, Kind: "human_input", OriginalText: text},
		Authority:  &devicewire.ShareAuthority{Issuer: "hub", Machine: machine, Generation: "g1"},
		Hold:       &devicewire.Hold{Lang: "en"}}
}

// heldAnswerOf is a CodeHeld answer's held message; it fails the test for any other answer.
func heldAnswerOf(t *testing.T, out json.RawMessage, err error) proto.UndeliveredMessage {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != devicewire.CodeHeld {
		t.Fatalf("not held: %s %v", out, err)
	}
	var a devicewire.HeldAnswer
	if json.Unmarshal(re.Data, &a) != nil || a.Delivered != "later" || a.Undelivered == nil {
		t.Fatalf("held answer %s", re.Data)
	}
	return *a.Undelivered
}

func heldIDs(list []proto.UndeliveredMessage) []string {
	var out []string
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// A message to an Agent whose computer is not connected is held: the hub gets CodeHeld with the message as its row
// shows it, the call is kept as the hub sent it (caller, submission and share authority; not its hold, wait or request
// key), and the same message sent again is the same held one. A send the hub did not mark fails as before.
func TestHeldWhenTheComputerIsAway(t *testing.T) {
	st := newHeldMem()
	d, pub := heldDevices(t, st, time.Hour)
	offlineMachine(d, "mac", "Studio Mac")
	ctx := context.Background()

	out, err := d.Call(ctx, heldSend("mac", "s1", "m1", "hello"))
	got := heldAnswerOf(t, out, err)
	want := proto.UndeliveredMessage{ID: "m1", Text: "hello", At: got.At, State: proto.UndeliveredWaiting, Computer: "Studio Mac",
		Error: "Will send when Studio Mac is back"}
	if got != want || got.At.IsZero() {
		t.Fatalf("held = %+v, want %+v", got, want)
	}
	rows := st.all()
	if len(rows) != 1 || rows[0].MachineID != "mac" || rows[0].SessionID != "s1" || rows[0].OpID != "m1" || rows[0].Lang != "en" ||
		rows[0].State != db.HeldWaiting {
		t.Fatalf("stored %+v", rows)
	}
	var kept map[string]json.RawMessage
	_ = json.Unmarshal(rows[0].Call, &kept)
	for _, k := range []string{"caller", "submission", "share_authority", "params", "method", "machine_id"} {
		if kept[k] == nil {
			t.Errorf("the stored call lost %s: %s", k, rows[0].Call)
		}
	}
	for _, k := range []string{"hold", "timeout_ms", "request"} {
		if kept[k] != nil {
			t.Errorf("the stored call keeps %s: %s", k, rows[0].Call)
		}
	}
	if list := d.HeldMessages("mac", i18n.En)["s1"]; len(list) != 1 || list[0] != want {
		t.Fatalf("the row shows %+v", list)
	}
	eventually(t, 2*time.Second, "the rows republished", func() bool { return pub.count("mac") > 0 })

	// The same message again (the app retried it): the same one, held once.
	out, err = d.Call(ctx, heldSend("mac", "s1", "m1", "hello"))
	if again := heldAnswerOf(t, out, err); again.ID != "m1" || len(st.all()) != 1 {
		t.Fatalf("held twice: %+v, %d rows", again, len(st.all()))
	}

	// Not marked by the hub, or without its own id: machine_offline as before, nothing kept.
	plain := heldSend("mac", "s1", "m2", "x")
	plain.Hold = nil
	noID := heldSend("mac", "s1", "", "x")
	for _, p := range []devicewire.NodeCall{plain, noID} {
		var re *rpc.Error
		if _, err := d.Call(ctx, p); !errors.As(err, &re) || re.Code != devicewire.CodeMachineOffline {
			t.Errorf("%s: %v", p.Params, err)
		}
	}
	if len(st.all()) != 1 {
		t.Fatalf("kept %d", len(st.all()))
	}
	// A store that does not keep it: the failure as before.
	st.setFail(errors.New("database down"))
	var re *rpc.Error
	if _, err := d.Call(ctx, heldSend("mac", "s2", "m3", "x")); !errors.As(err, &re) || re.Code != devicewire.CodeMachineOffline {
		t.Fatalf("store down: %v", err)
	}
}

// Once the computer's node is back (configured and listed), what was held for it goes at once, in order per session,
// each once, as the hub sent it (the owner's input mark that its caller and submission give, its client_msg_id), and
// leaves the rows. A newer message to a session with held ones waits behind them.
func TestHeldMessagesGoWhenTheComputerIsBack(t *testing.T) {
	st := newHeldMem()
	d, pub := heldDevices(t, st, time.Hour) // no tick: only the connection wakes them
	n := &heldNode{}
	up := make(chan struct{})
	m := laterMachine(t, d, "mac", "Studio Mac", n, up)
	ctx := context.Background()
	for _, c := range []struct{ sid, id string }{{"s1", "a"}, {"s2", "x"}, {"s1", "b"}} {
		out, err := d.Call(ctx, heldSend("mac", c.sid, c.id, "text "+c.id))
		heldAnswerOf(t, out, err)
	}
	if got := heldIDs(d.HeldMessages("mac", i18n.En)["s1"]); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("s1 holds %v", got)
	}
	before := pub.count("mac")

	close(up)
	eventually(t, 5*time.Second, "the held messages sent", func() bool { return len(n.sent()) == 3 && len(st.all()) == 0 })
	got := n.sent()
	if i, j := slices.Index(got, "a"), slices.Index(got, "b"); i < 0 || j < i || !slices.Contains(got, "x") {
		t.Fatalf("sent %v: not in order", got)
	}
	n.mu.Lock()
	for _, s := range n.sends {
		if s["owner_input"] != true || !strings.HasPrefix(s["text"].(string), "text ") {
			t.Errorf("sent %v: not as the hub sent it", s)
		}
	}
	n.mu.Unlock()
	if d.HeldMessages("mac", i18n.En) != nil {
		t.Fatalf("still shown: %v", d.HeldMessages("mac", i18n.En))
	}
	eventually(t, 2*time.Second, "the rows republished", func() bool { return pub.count("mac") > before })

	// Each once: another wake sends nothing again.
	d.heldWake("mac")
	time.Sleep(100 * time.Millisecond)
	if len(n.sent()) != 3 {
		t.Fatalf("sent again: %v", n.sent())
	}
	// Online, nothing held: a message goes straight to the node.
	if out, err := d.Call(ctx, heldSend("mac", "s1", "c", "now")); err != nil || !m.reachable() {
		t.Fatalf("online send: %s %v", out, err)
	}
	if got := n.sent(); len(got) != 4 || got[3] != "c" {
		t.Fatalf("sent %v", got)
	}
}

// A try cut off by the connection dropping with it out may have arrived: it is never sent again by itself, the row
// says so, and the later ones of its session wait behind it, a new one too, even with the computer back; the owner's
// Retry sends it again, then the rest.
func TestHeldUnclearIsNotSentAgain(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, 50*time.Millisecond)
	n := &heldNode{gate: make(chan struct{})}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		out, err := d.Call(ctx, heldSend("mac", "s1", id, id))
		heldAnswerOf(t, out, err)
	}
	close(up)
	eventually(t, 5*time.Second, "a sent", func() bool { return len(n.sent()) == 1 })
	if list := d.HeldMessages("mac", i18n.En)["s1"]; list[0].Error != "Sending…" || list[0].State != proto.UndeliveredWaiting {
		t.Fatalf("while sent: %+v", list)
	}
	n.drop() // the connection drops with a out
	gate := n.gate
	n.mu.Lock()
	n.gate = nil
	n.mu.Unlock()
	close(gate)
	eventually(t, 5*time.Second, "a unclear", func() bool {
		rows := st.all()
		return len(rows) == 2 && rows[0].State == db.HeldFailed && rows[0].Failure == db.HeldUnclear
	})
	list := d.HeldMessages("mac", i18n.En)["s1"]
	if list[0].State != proto.UndeliveredFailed || list[0].Error != "May have arrived before the connection dropped. Check the chat, then retry or delete." ||
		list[1].State != proto.UndeliveredWaiting || list[1].Error != "Waits behind the message above" {
		t.Fatalf("rows %+v", list)
	}
	// Back and ticking: neither goes; a new message waits behind them.
	eventually(t, 10*time.Second, "back", func() bool { return d.machineByID("mac").reachable() })
	out, err := d.Call(ctx, heldSend("mac", "s1", "c", "c"))
	if held := heldAnswerOf(t, out, err); held.Error != "Waits behind the message above" {
		t.Fatalf("c: %+v", held)
	}
	time.Sleep(300 * time.Millisecond)
	if got := n.sent(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("sent %v", got)
	}
	// The owner checked and retries a: it goes again, then b and c.
	mux := d.Handler(http.NotFoundHandler())
	if code, body := clipCall(t, mux, "POST", "/api/v1/sessions/mac/s1/held/a/retry", "", "full"); code != http.StatusOK || body["ok"] != true {
		t.Fatalf("retry: %d %v", code, body)
	}
	eventually(t, 5*time.Second, "all sent", func() bool { return len(st.all()) == 0 })
	if got := n.sent(); !slices.Equal(got, []string{"a", "a", "b", "c"}) {
		t.Fatalf("sent %v", got)
	}
}

// A node's refusal (nothing typed) fails the message with the node's words; it does not hold the next one back.
// A new message to its session then goes straight to the node.
func TestHeldRefusedDoesNotBlock(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{answer: func(m map[string]any) (any, error) {
		if m["client_msg_id"] == "a" {
			return nil, &rpc.Error{Code: "input_draft", Message: "a draft is in the way", Data: json.RawMessage(`{"delivered":"none"}`)}
		}
		return map[string]any{"ok": true}, nil
	}}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		out, err := d.Call(ctx, heldSend("mac", "s1", id, id))
		heldAnswerOf(t, out, err)
	}
	close(up)
	eventually(t, 5*time.Second, "b sent", func() bool { return slices.Equal(n.sent(), []string{"a", "b"}) && len(st.all()) == 1 })
	list := d.HeldMessages("mac", i18n.En)["s1"]
	if len(list) != 1 || list[0].ID != "a" || list[0].State != proto.UndeliveredFailed || list[0].Error != "Not sent: a draft is in the way" {
		t.Fatalf("rows %+v", list)
	}
	if out, err := d.Call(ctx, heldSend("mac", "s1", "c", "c")); err != nil {
		t.Fatalf("c: %s %v", out, err)
	}
	if got := n.sent(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("sent %v", got)
	}
}

// A day after it was held a message still waiting fails as expired; Retry gives it a new day.
func TestHeldExpiresAfterADay(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour)
	var mu sync.Mutex
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	d.held.now = clock
	offlineMachine(d, "mac", "Studio Mac")
	ctx := context.Background()
	out, err := d.Call(ctx, heldSend("mac", "s1", "a", "a"))
	heldAnswerOf(t, out, err)
	mu.Lock()
	now = now.Add(23 * time.Hour)
	mu.Unlock()
	d.heldExpire(ctx)
	if st.all()[0].State != db.HeldWaiting {
		t.Fatal("expired early")
	}
	mu.Lock()
	now = now.Add(time.Hour)
	mu.Unlock()
	d.heldExpire(ctx)
	if r := st.all()[0]; r.State != db.HeldFailed || r.Failure != db.HeldExpired {
		t.Fatalf("stored %+v", r)
	}
	if list := d.HeldMessages("mac", i18n.En)["s1"]; list[0].State != proto.UndeliveredFailed || list[0].Error != "Not sent: Studio Mac was away for 24 hours" {
		t.Fatalf("row %+v", list)
	}
	// Expired does not hold a new message back: it is held as the computer is away, after it.
	out, err = d.Call(ctx, heldSend("mac", "s1", "b", "b"))
	if held := heldAnswerOf(t, out, err); held.Error != "Will send when Studio Mac is back" {
		t.Fatalf("b: %+v", held)
	}
	found, err := d.HeldRetry(ctx, "s1", "a")
	if !found || err != nil {
		t.Fatalf("retry: %v %v", found, err)
	}
	if r := st.all()[0]; r.State != db.HeldWaiting || !r.HeldAt.Equal(clock()) || r.Failure != "" {
		t.Fatalf("after retry %+v", r)
	}
	if list := d.HeldMessages("mac", i18n.En)["s1"]; list[0].State != proto.UndeliveredWaiting {
		t.Fatalf("row %+v", list)
	}
}

// Delete takes a waiting or failed message away, from a full device only; while it is being sent it is refused (409),
// and a message there is not is 404.
func TestHeldDelete(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{gate: make(chan struct{})}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	ctx := context.Background()
	for _, c := range []struct{ sid, id string }{{"s1", "a"}, {"s2", "x"}} {
		out, err := d.Call(ctx, heldSend("mac", c.sid, c.id, c.id))
		heldAnswerOf(t, out, err)
	}
	mux := d.Handler(http.NotFoundHandler())
	if code, body := clipCall(t, mux, "DELETE", "/api/v1/sessions/mac/s2/held/x", "", "answer"); code != http.StatusForbidden || body["code"] != proto.CodeReadOnly {
		t.Fatalf("answer-only device: %d %v", code, body)
	}
	if code, body := clipCall(t, mux, "DELETE", "/api/v1/sessions/mac/s2/held/x", "", "full"); code != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete: %d %v", code, body)
	}
	if code, _ := clipCall(t, mux, "DELETE", "/api/v1/sessions/mac/s2/held/x", "", "full"); code != http.StatusNotFound {
		t.Fatalf("again: %d", code)
	}
	if code, _ := clipCall(t, mux, "POST", "/api/v1/sessions/mac/s2/held/x/retry", "", "full"); code != http.StatusNotFound {
		t.Fatalf("retry of a deleted one: %d", code)
	}
	if found, err := d.HeldDelete(ctx, "s2", "x"); found || err != nil {
		t.Fatalf("assistant's delete of a deleted one: %v %v", found, err)
	}
	close(up)
	eventually(t, 5*time.Second, "a being sent", func() bool { return len(n.sent()) == 1 })
	if code, body := clipCall(t, mux, "DELETE", "/api/v1/sessions/mac/s1/held/a", "", "full"); code != http.StatusConflict || body["code"] != "conflict" {
		t.Fatalf("while sent: %d %v", code, body)
	}
	found, err := d.HeldDelete(i18n.With(ctx, i18n.En), "s1", "a")
	var re *rpc.Error
	if !found || !errors.As(err, &re) || re.Code != "conflict" || re.Message != "This message is being sent right now. Try again in a moment." {
		t.Fatalf("assistant's delete while sent: %v %v", found, err)
	}
	close(n.gate)
	eventually(t, 5*time.Second, "a sent", func() bool { return len(st.all()) == 0 })
	if got := n.sent(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("sent %v", got)
	}
}

// A manager that starts again finds what the last one held: a message it left being sent may have arrived (unclear,
// never sent again by itself, the next one waits behind it); one left waiting goes once the computer is back.
func TestHeldAcrossARestart(t *testing.T) {
	st := newHeldMem()
	call := func(sid, id string) json.RawMessage {
		p := heldSend("mac", sid, id, id)
		p.Hold, p.TimeoutMS, p.Request = nil, 0, ""
		raw, _ := json.Marshal(p)
		return raw
	}
	at := time.Now().Add(-time.Hour)
	for _, c := range []struct{ sid, id string }{{"s1", "a"}, {"s1", "b"}, {"s2", "x"}} {
		if _, _, err := st.HoldOperation(context.Background(), db.HeldOperation{Kind: db.HeldKindMessage, OpID: c.id, MachineID: "mac",
			SessionID: c.sid, Call: call(c.sid, c.id), Lang: "en", SentAt: at, HeldAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := st.ClaimHeldOperation(context.Background(), 1, at); !ok { // the last manager stopped while sending a
		t.Fatal("claim")
	}
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	list := d.HeldMessages("mac", i18n.En)
	if s1 := list["s1"]; len(s1) != 2 || s1[0].State != proto.UndeliveredFailed || s1[1].Error != "Waits behind the message above" {
		t.Fatalf("s1 %+v", s1)
	}
	if r := st.all()[0]; r.State != db.HeldFailed || r.Failure != db.HeldUnclear {
		t.Fatalf("stored %+v", r)
	}
	close(up)
	eventually(t, 5*time.Second, "x sent", func() bool { return slices.Equal(n.sent(), []string{"x"}) })
	time.Sleep(200 * time.Millisecond)
	if got := n.sent(); !slices.Equal(got, []string{"x"}) || len(st.all()) != 2 {
		t.Fatalf("sent %v, %d left", got, len(st.all()))
	}
}

// What a delivery's error says it did: only what surely wrote nothing goes again by itself; the node's coded refusal
// fails it with its words; everything else may have arrived.
func TestHeldOutcomeOf(t *testing.T) {
	refusal := &rpc.Error{Code: "input_draft", Message: "a draft is in the way", Data: json.RawMessage(`{"delivered":"none"}`)}
	for _, c := range []struct {
		err    error
		want   heldOutcome
		reason string
	}{
		{nil, heldDelivered, ""},
		{fmt.Errorf("mac: %w", apierr.ErrOffline), heldNotSent, ""},
		{rpc.NotSent(rpc.ErrClosed), heldNotSent, ""},
		{rpc.NotSent(context.Canceled), heldNotSent, ""},
		{rpc.NotSent(context.DeadlineExceeded), heldNotSent, ""},
		{rpc.ErrSuspended, heldNotSent, ""},
		{&rpc.Error{Code: devicewire.CodeMachineOffline, Message: "away"}, heldNotSent, ""},
		{wireError(fmt.Errorf("mac: %w", apierr.ErrOffline)), heldNotSent, ""},
		{wireError(rpc.NotSent(rpc.ErrClosed)), heldNotSent, ""},
		{rpc.ErrDropped, heldUnclear, ""},
		{rpc.ErrClosed, heldUnclear, ""}, // bare: proves nothing (a wrapper may have lost ErrDropped's identity)
		{fmt.Errorf("mac: %w", rpc.ErrClosed), heldUnclear, ""},
		{writerCtxErr{}, heldUnclear, ""}, // ErrNotWritten without ErrNotSent: a writer's own context error
		{fmt.Errorf("mac: %w", rpc.ErrDropped), heldUnclear, ""},
		{context.DeadlineExceeded, heldUnclear, ""},
		{context.Canceled, heldUnclear, ""},
		{errors.New("write: broken pipe"), heldUnclear, ""},
		{&rpc.Error{Message: "it may have gone", Data: json.RawMessage(`{"delivered":"unknown"}`)}, heldUnclear, ""},
		{&rpc.Error{Code: devicewire.CodeNodeClosed, Message: "closed"}, heldUnclear, ""},
		{&rpc.Error{Code: devicewire.CodeDeadline, Message: "late"}, heldUnclear, ""},
		{&rpc.Error{Message: rpc.ErrClosed.Error()}, heldUnclear, ""},
		{refusal, heldRefused, "a draft is in the way"},
		{&rpc.Error{Code: "session_gone"}, heldRefused, "session_gone"},
	} {
		got, reason := heldOutcomeOf(c.err)
		if got != c.want || reason != c.reason {
			t.Errorf("%v: %d %q, want %d %q", c.err, got, reason, c.want, c.reason)
		}
	}
	// Held at the send only when nothing was written and somebody waits for the answer.
	held := map[error]bool{
		apierr.ErrOffline:                               true,
		rpc.NotSent(context.DeadlineExceeded):           true,
		rpc.NotSent(rpc.ErrClosed):                      true,
		rpc.ErrSuspended:                                true,
		&rpc.Error{Code: devicewire.CodeMachineOffline}: true,
		rpc.NotSent(context.Canceled):                   false, // nobody waits for the answer
		rpc.ErrDropped:                                  false,
		rpc.ErrClosed:                                   false,
		writerCtxErr{}:                                  false,
		context.DeadlineExceeded:                        false,
		refusal:                                         false,
	}
	for err, want := range held {
		if heldMayHold(err) != want {
			t.Errorf("heldMayHold(%v) = %v", err, !want)
		}
	}
}

// The words under a held message, in every language: a computer's name, the node's reason.
func TestHeldLinesInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Langs {
		for _, r := range []heldRow{
			{HeldOperation: db.HeldOperation{State: db.HeldWaiting}},
			{HeldOperation: db.HeldOperation{State: db.HeldSending}},
			{HeldOperation: db.HeldOperation{State: db.HeldFailed, Failure: db.HeldUnclear}},
			{HeldOperation: db.HeldOperation{State: db.HeldFailed, Failure: db.HeldRefused, Error: "REASON"}},
			{HeldOperation: db.HeldOperation{State: db.HeldFailed, Failure: db.HeldRefused}},
			{HeldOperation: db.HeldOperation{State: db.HeldFailed, Failure: db.HeldExpired}},
		} {
			m := r.wire(l, "NAME", false)
			if m.Error == "" || strings.Contains(m.Error, "{") || strings.Contains(m.Error, "devices-held.") {
				t.Errorf("%s %+v: %q", l, r.HeldOperation, m.Error)
			}
			if (r.State == db.HeldWaiting || r.Failure == db.HeldExpired) && !strings.Contains(m.Error, "NAME") {
				t.Errorf("%s %+v: %q has no computer", l, r.HeldOperation, m.Error)
			}
			if r.Error != "" && !strings.Contains(m.Error, "REASON") {
				t.Errorf("%s: %q has no reason", l, m.Error)
			}
			if wantFailed := r.State == db.HeldFailed; (m.State == proto.UndeliveredFailed) != wantFailed {
				t.Errorf("%s %+v: state %s", l, r.HeldOperation, m.State)
			}
		}
	}
}

// While the held messages cannot be read nothing is held (the send fails as before), and after a failed read the
// store is left alone for heldLoadBackoff: the sends meanwhile do not wait on it, one after another. Then the send path
// reads them itself.
func TestHeldNeedsTheStore(t *testing.T) {
	old := heldLoadBackoff
	heldLoadBackoff = 200 * time.Millisecond
	t.Cleanup(func() { heldLoadBackoff = old })
	st := newHeldMem()
	st.setFail(errors.New("database down"))
	d := newTestDevices(t)
	d.opts.Store = st
	offlineMachine(d, "mac", "Studio Mac")
	ctx := context.Background()
	var re *rpc.Error
	for i := range 3 {
		if _, err := d.Call(ctx, heldSend("mac", "s1", "a", "a")); !errors.As(err, &re) || re.Code != devicewire.CodeMachineOffline {
			t.Fatalf("store down, send %d: %v", i, err)
		}
	}
	if found, err := d.HeldDelete(ctx, "s1", "a"); found || err != nil {
		t.Fatalf("delete with the store down: %v %v", found, err)
	}
	if n := st.loadCount(); n != 1 {
		t.Fatalf("the store was asked %d times within its back-off", n)
	}
	st.setFail(nil)
	time.Sleep(heldLoadBackoff)
	out, err := d.Call(ctx, heldSend("mac", "s1", "a", "a"))
	if held := heldAnswerOf(t, out, err); held.ID != "a" || len(st.all()) != 1 || st.loadCount() != 2 {
		t.Fatalf("held %+v, %d stored, %d loads", held, len(st.all()), st.loadCount())
	}
}

// Right after the handshake an Agent of a runtime is not listed yet (it comes back a moment after its node): a
// message held for it waits, through any wake, until the node lists its session, then goes at once, once.
func TestHeldWaitsForItsSessionToBeListed(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour) // no tick: the list arriving wakes it
	n := &heldNode{listed: []string{"s2"}}
	up := make(chan struct{})
	m := laterMachine(t, d, "mac", "Studio Mac", n, up)
	out, err := d.Call(context.Background(), heldSend("mac", "s1", "a", "a"))
	heldAnswerOf(t, out, err)
	close(up)
	eventually(t, 5*time.Second, "connected and listed", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.peer != nil && m.sessionsListed
	})
	d.heldWake("mac")                  // what a tick does
	time.Sleep(300 * time.Millisecond) // and the connection's wake
	if got := n.sent(); len(got) != 0 {
		t.Fatalf("sent before its session was listed: %v", got)
	}
	if rows := st.all(); len(rows) != 1 || rows[0].State != db.HeldWaiting || rows[0].Attempts != 0 {
		t.Fatalf("stored %+v", rows)
	}
	n.list("s1", "s2")
	eventually(t, 5*time.Second, "a sent", func() bool { return len(st.all()) == 0 })
	time.Sleep(200 * time.Millisecond)
	if got := n.sent(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("sent %v", got)
	}
}

// A session still not listed heldListWait into the connection: the message goes anyway, and the node's answer decides
// (its refusal fails it with its words).
func TestHeldGoesAnywayAfterTheListWait(t *testing.T) {
	old := heldListWait
	heldListWait = 300 * time.Millisecond
	t.Cleanup(func() { heldListWait = old })
	st := newHeldMem()
	d, _ := heldDevices(t, st, 20*time.Millisecond)
	n := &heldNode{listed: []string{"s2"}, answer: func(map[string]any) (any, error) {
		return nil, &rpc.Error{Code: "session_not_found", Message: "no such session", Data: json.RawMessage(`{"delivered":"none"}`)}
	}}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	out, err := d.Call(context.Background(), heldSend("mac", "s1", "a", "a"))
	heldAnswerOf(t, out, err)
	start := time.Now()
	close(up)
	eventually(t, 5*time.Second, "a refused", func() bool {
		rows := st.all()
		return len(rows) == 1 && rows[0].State == db.HeldFailed
	})
	if waited := time.Since(start); waited < heldListWait {
		t.Fatalf("sent after %s, before the list wait", waited)
	}
	if got := n.sent(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("sent %v", got)
	}
	if list := d.HeldMessages("mac", i18n.En)["s1"]; len(list) != 1 || list[0].Error != "Not sent: no such session" {
		t.Fatalf("row %+v", list)
	}
}

// writerCtxErr is what rpc.Call answers when the writer itself failed with the call context's error (a
// websocket.NetConn whose own context ended, part of the frame possibly out): ErrNotWritten, but not ErrNotSent.
type writerCtxErr struct{}

func (writerCtxErr) Error() string { return "failed to write frame: context deadline exceeded" }
func (writerCtxErr) Is(target error) bool {
	return target == rpc.ErrNotWritten || target == context.DeadlineExceeded
}

// putHeld keeps message id for session sid of machine mac in st as a manager held it at, in state (failed with
// failure); its seq.
func putHeld(t *testing.T, st *heldMem, sid, id, state, failure string, at time.Time) int64 {
	t.Helper()
	p := heldSend("mac", sid, id, id)
	p.Hold, p.TimeoutMS, p.Request = nil, 0, ""
	raw, _ := json.Marshal(p)
	op, _, err := st.HoldOperation(context.Background(), db.HeldOperation{Kind: db.HeldKindMessage, OpID: id, MachineID: "mac",
		SessionID: sid, Call: raw, Lang: "en", SentAt: at, HeldAt: at})
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	r := st.rows[op.Seq]
	r.State, r.Failure = state, failure
	st.rows[op.Seq] = r
	st.mu.Unlock()
	return op.Seq
}

// A held message that cannot be made ready to send here (its preparation fails before the node is asked) wrote
// nothing: it fails as refused, with why, and does not hold the next one back.
func TestHeldNotPreparedIsRefused(t *testing.T) {
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	ctx := context.Background()
	out, err := d.Call(ctx, heldSend("mac", "s1", "a", "a"))
	heldAnswerOf(t, out, err)
	bad := heldSend("mac", "s1", "b", "b")
	bad.Params = json.RawMessage(`{"session_id":"s1","client_msg_id":"b","text":5}`) // held behind a; not a text
	out, err = d.Call(ctx, bad)
	heldAnswerOf(t, out, err)
	out, err = d.Call(ctx, heldSend("mac", "s1", "c", "c"))
	heldAnswerOf(t, out, err)
	close(up)
	eventually(t, 5*time.Second, "a and c sent", func() bool { return slices.Equal(n.sent(), []string{"a", "c"}) && len(st.all()) == 1 })
	if r := st.all()[0]; r.OpID != "b" || r.State != db.HeldFailed || r.Failure != db.HeldRefused {
		t.Fatalf("b stored %+v", r)
	}
	if list := d.HeldMessages("mac", i18n.En)["s1"]; len(list) != 1 || list[0].Error != "Not sent: Persona couldn't get it ready to send" {
		t.Fatalf("row %+v", list)
	}
}

// Once the node took a held message it was delivered, whatever comes after its answer (too large to pass on): never
// refused, so Retry could not send it twice.
func TestHeldDeliveredWhateverComesAfter(t *testing.T) {
	old := maxForward
	maxForward = 16 << 10
	t.Cleanup(func() { maxForward = old })
	st := newHeldMem()
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{answer: func(m map[string]any) (any, error) {
		if m["client_msg_id"] == "a" {
			return map[string]any{"ok": true, "pad": strings.Repeat("x", 32<<10)}, nil
		}
		return map[string]any{"ok": true}, nil
	}}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	for _, id := range []string{"a", "b"} {
		out, err := d.Call(context.Background(), heldSend("mac", "s1", id, id))
		heldAnswerOf(t, out, err)
	}
	close(up)
	eventually(t, 5*time.Second, "both sent", func() bool { return slices.Equal(n.sent(), []string{"a", "b"}) && len(st.all()) == 0 })
}

// A message that may have arrived heads its session: on each new connection that lists the session, its transcript
// is read once; there under its client_msg_id it did arrive, leaves the rows, and the next one goes once.
func TestHeldLandedUnclearIsTakenAsSent(t *testing.T) {
	st := newHeldMem()
	at := time.Now().Add(-time.Hour)
	putHeld(t, st, "s1", "a", db.HeldFailed, db.HeldUnclear, at)
	putHeld(t, st, "s1", "b", db.HeldWaiting, "", at)
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{items: map[string][]proto.Item{"s1": {{Role: "user", Text: "a", ClientMsgID: "a"}, {Role: "assistant", Text: "ok"}}}}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	close(up)
	eventually(t, 5*time.Second, "b sent", func() bool { return slices.Equal(n.sent(), []string{"b"}) && len(st.all()) == 0 })
	d.heldWake("mac")
	time.Sleep(100 * time.Millisecond)
	if got, reads := n.sent(), n.itemReads(); !slices.Equal(got, []string{"b"}) || reads != 1 {
		t.Fatalf("sent %v, %d reads", got, reads)
	}
}

// Not in the transcript by its id (words alike do not count), or the transcript not read: it stays for the owner,
// nothing is sent, and it is read once a connection however often the deliveries look.
func TestHeldUnclearNotLandedStays(t *testing.T) {
	st := newHeldMem()
	at := time.Now().Add(-time.Hour)
	putHeld(t, st, "s1", "a", db.HeldFailed, db.HeldUnclear, at)
	putHeld(t, st, "s1", "b", db.HeldWaiting, "", at)
	d, _ := heldDevices(t, st, 20*time.Millisecond)
	n := &heldNode{items: map[string][]proto.Item{"s1": {{Role: "user", Text: "a"}, {Role: "user", Text: "x", ClientMsgID: "x"}}}}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	close(up)
	eventually(t, 5*time.Second, "the transcript read", func() bool { return n.itemReads() == 1 })
	time.Sleep(300 * time.Millisecond) // many ticks
	if got, reads := n.sent(), n.itemReads(); len(got) != 0 || reads != 1 {
		t.Fatalf("sent %v, %d reads", got, reads)
	}
	if rows := st.all(); len(rows) != 2 || !(rows[0].State == db.HeldFailed && rows[0].Failure == db.HeldUnclear) || rows[1].State != db.HeldWaiting {
		t.Fatalf("stored %+v", rows)
	}
	// A new connection reads it again.
	n.drop()
	eventually(t, 10*time.Second, "read on the new connection", func() bool { return n.itemReads() == 2 })
	if got := n.sent(); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}

// While the owner's Retry of a failed message is being written, it holds the later ones of its session back: they
// never overtake it.
func TestHeldRetryInFlightHoldsLaterOnes(t *testing.T) {
	st := newHeldMem()
	at := time.Now().Add(-time.Hour)
	putHeld(t, st, "s1", "a", db.HeldFailed, db.HeldRefused, at)
	putHeld(t, st, "s1", "b", db.HeldWaiting, "", at)
	d, _ := heldDevices(t, st, time.Hour)
	n := &heldNode{}
	up := make(chan struct{})
	laterMachine(t, d, "mac", "Studio Mac", n, up)
	st.mu.Lock()
	st.retryGate, st.retrying = make(chan struct{}), make(chan struct{}, 1)
	gate, retrying := st.retryGate, st.retrying
	st.mu.Unlock()
	retried := make(chan error, 1)
	go func() {
		_, err := d.HeldRetry(context.Background(), "s1", "a")
		retried <- err
	}()
	<-retrying // a's Retry is being written
	close(up)  // the computer comes back meanwhile: its list wakes the deliveries
	eventually(t, 5*time.Second, "connected", func() bool {
		listed, _ := d.machineByID("mac").heldOn("s1")
		return !listed.IsZero()
	})
	d.heldWake("mac")
	time.Sleep(200 * time.Millisecond)
	if got := n.sent(); len(got) != 0 {
		t.Fatalf("sent %v while a's Retry was written", got)
	}
	close(gate)
	if err := <-retried; err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "both sent", func() bool { return len(st.all()) == 0 })
	if got := n.sent(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("sent %v", got)
	}
}

// A message held behind one that may have arrived waits for the owner, not for its computer: it never expires (nor
// says its computer was away), and its wait starts again, so it has nearly a day once the owner decides. One not
// behind such a message expires after a day.
func TestHeldBehindUnclearDoesNotExpire(t *testing.T) {
	st := newHeldMem()
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	putHeld(t, st, "s1", "a", db.HeldFailed, db.HeldUnclear, at)
	b := putHeld(t, st, "s1", "b", db.HeldWaiting, "", at)
	putHeld(t, st, "s2", "x", db.HeldWaiting, "", at)
	d, _ := heldDevices(t, st, time.Hour)
	var mu sync.Mutex
	now := at.Add(25 * time.Hour)
	d.held.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	offlineMachine(d, "mac", "Studio Mac")
	d.heldExpire(context.Background())
	rows := st.snapshot()
	if r := rows[b]; r.State != db.HeldWaiting || !r.HeldAt.Equal(now) {
		t.Fatalf("b %+v", r)
	}
	list := d.HeldMessages("mac", i18n.En)
	if s1 := list["s1"]; len(s1) != 2 || s1[1].State != proto.UndeliveredWaiting || s1[1].Error != "Waits behind the message above" {
		t.Fatalf("s1 %+v", s1)
	}
	if s2 := list["s2"]; len(s2) != 1 || s2[0].Error != "Not sent: Studio Mac was away for 24 hours" {
		t.Fatalf("s2 %+v", s2)
	}
}

// A message that waited a whole day is not sent when its computer comes back before the tick fails it: the expiry
// is heldExpire's alone.
func TestHeldExpiredIsNotSent(t *testing.T) {
	st := newHeldMem()
	at := time.Now()
	putHeld(t, st, "s1", "a", db.HeldWaiting, "", at)
	d, _ := heldDevices(t, st, time.Hour) // its first look found it fresh
	var mu sync.Mutex
	now := at.Add(25 * time.Hour)
	d.held.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	n := &heldNode{}
	up := make(chan struct{})
	m := laterMachine(t, d, "mac", "Studio Mac", n, up)
	close(up)
	eventually(t, 5*time.Second, "listed", func() bool { listed, _ := m.heldOn("s1"); return !listed.IsZero() })
	d.heldWake("mac")
	time.Sleep(200 * time.Millisecond)
	if got := n.sent(); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
	d.heldExpire(context.Background())
	if r := st.all()[0]; r.State != db.HeldFailed || r.Failure != db.HeldExpired {
		t.Fatalf("stored %+v", r)
	}
}
