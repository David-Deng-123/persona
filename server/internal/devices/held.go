package devices

// The owner's messages to an Agent whose computer is away (#10661).
//
// The hub marks the owner's message to an existing Agent (not an assistant conversation) as one that may be held
// (NodeCall.Hold). When the computer's node is not connected, so nothing was written, or older held messages of the
// same session still wait, the manager keeps the call as it came in its Postgres (held_operations) and answers
// CodeHeld: the hub tells the app it goes later, and every client draws it on the session's row
// (proto.Session.Undelivered with Computer set, read through HeldMessages). Once the node is back (configured and
// listed) the held calls go at once, per session in the order they were held, each exactly as the hub sent it: Call
// again with its caller, submission and share authority, under the same client_msg_id. Sent, a message leaves the row.
//
// Only a try known to have written nothing goes again by itself (the node not connected, closed before the write).
// A try whose outcome is unknown (the connection dropped with it out, its time ran out, the node said delivered
// "unknown", the manager stopped during it) never does: it fails as unclear, and the later ones of its session wait
// behind it until the owner retries or deletes it, or its transcript, read once per connection, shows it under its
// client_msg_id (it did arrive: it leaves the rows). A delivery is judged on the node's call alone: refused when the
// node refused it or it was never asked (its preparation here failed), with why; delivered once the node took it.
// A message still waiting for its computer a day after it was held fails as expired (not one behind a message that may
// have arrived: that one waits for the owner). Retry and Delete are the owner's
// (POST …/held/{msg}/retry, DELETE …/held/{msg}; the assistant's undelivered routes end here for older apps).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/db"
	"github.com/nwverse/persona/server/internal/devicewire"
	"github.com/nwverse/persona/server/internal/i18n"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
	"github.com/nwverse/persona/server/internal/textfmt"
)

// heldStore keeps the held messages (db.DB); a store without it holds none: a send fails as before.
type heldStore interface {
	HoldOperation(ctx context.Context, op db.HeldOperation) (db.HeldOperation, bool, error)
	HeldOperationOf(ctx context.Context, kind, machine, session, opID string) (db.HeldOperation, bool, error)
	HeldOperations(ctx context.Context, kind string) ([]db.HeldOperation, error)
	RecoverHeldOperations(ctx context.Context, kind string) (int64, error)
	ClaimHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error)
	SettleHeldOperation(ctx context.Context, seq int64, from, state, failure, reason string) (bool, error)
	RetryHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error)
	RenewHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error)
	DeleteHeldOperation(ctx context.Context, seq int64, states ...string) (bool, error)
}

// Vars so tests change them.
var (
	heldExpiry      = 24 * time.Hour   // how long a message waits for its computer
	heldTick        = 15 * time.Second // how often the held messages are looked at without anything waking them
	heldSendTimeout = 5 * time.Minute  // one try (an Agent resumed to take it can take minutes)
	heldStoreWait   = 5 * time.Second  // one write to the store
	// heldListWait: a message waits for its session in the node's list this long into a connection (an Agent of a
	// runtime comes back a moment after its node), then goes anyway and the node's answer decides.
	heldListWait = 2 * time.Minute
	// heldLoadBackoff: after the store could not be read, it is not asked again this long (a send goes as before
	// meanwhile, without waiting on it).
	heldLoadBackoff = 5 * time.Second
	// heldBlockedRenew: a message held behind one that may have arrived waits for the owner, not for its computer: its
	// wait starts again this often, so it has nearly a whole day once the owner decides.
	heldBlockedRenew = time.Hour
	heldCheckWait    = 20 * time.Second // reading a session's latest turns for a message that may have arrived
)

// codeHeldNotKept: an eligible message that had to wait behind held ones could not be kept (the store failed);
// nothing was typed.
const codeHeldNotKept = "held_not_kept"

// heldMaxBytes bounds one held call (its words and pictures as the app sent them): a bigger one is not held and the
// app gets the failure as before.
const heldMaxBytes = 16 << 20

// heldRow is one held message as the manager has it.
type heldRow struct {
	db.HeldOperation
	text   string
	images int
	// busy: the owner's Retry or Delete, or its expiry, is writing it now; it and the later ones of its session wait
	// until that is done.
	busy bool
	// checked: the connection (its start) on which the session's transcript was read for this one, which may have
	// arrived: one read per connection.
	checked time.Time
}

func newHeldRow(op db.HeldOperation) *heldRow {
	r := &heldRow{HeldOperation: op}
	var c struct {
		Params struct {
			Text   string            `json:"text"`
			Images []json.RawMessage `json:"images"`
		} `json:"params"`
	}
	if json.Unmarshal(op.Call, &c) == nil {
		r.text, r.images = c.Params.Text, len(c.Params.Images)
	}
	return r
}

// unclear: a try of it may have arrived; the later ones of its session wait behind it.
func (r *heldRow) unclear() bool { return r.State == db.HeldFailed && r.Failure == db.HeldUnclear }

// expired: it has waited a whole day; heldExpire fails it, nothing sends it.
func (r *heldRow) expired(now time.Time) bool { return now.Sub(r.HeldAt) >= heldExpiry }

// heldBook is the held messages, read from the store once and kept in step with it.
type heldBook struct {
	loadMu sync.Mutex // one load at a time
	mu     sync.Mutex
	loaded bool
	failed time.Time             // when the last load failed (heldLoadBackoff)
	ctx    context.Context       // the deliveries' (heldLoop's); nil before it runs
	rows   map[string][]*heldRow // by machine, in the order they were held
	// sending: the sessions (machine + "/" + session) a delivery runs in: one at a time each.
	sending map[string]bool
	now     func() time.Time // tests
}

func (h *heldBook) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (d *Devices) heldStore() heldStore {
	s, _ := d.opts.Store.(heldStore)
	return s
}

// heldStoreCtx bounds one store write, which goes on when ctx ended (what was decided is kept).
func heldStoreCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), heldStoreWait)
}

// heldLoad reads the held messages once, after failing as unclear what a stopped manager left being sent; false
// while they cannot be read (nothing is held then: a send fails as before).
func (d *Devices) heldLoad(ctx context.Context) bool {
	h := &d.held
	// Read, or failed a moment ago (every holdable send asks: they must not wait on a store that is down, one after
	// another).
	settled := func() (bool, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.loaded, h.loaded || !h.failed.IsZero() && time.Since(h.failed) < heldLoadBackoff
	}
	if loaded, done := settled(); done {
		return loaded
	}
	st := d.heldStore()
	if st == nil {
		return false
	}
	h.loadMu.Lock()
	defer h.loadMu.Unlock()
	if loaded, done := settled(); done {
		return loaded
	}
	fail := func(err error) bool {
		d.logf("held messages: %v", err)
		h.mu.Lock()
		h.failed = time.Now()
		h.mu.Unlock()
		return false
	}
	sctx, cancel := heldStoreCtx(ctx)
	defer cancel()
	// Nothing of this manager's is being sent before the load: every row left sending is a stopped manager's.
	if n, err := st.RecoverHeldOperations(sctx, db.HeldKindMessage); err != nil {
		return fail(err)
	} else if n > 0 {
		d.logf("held messages: %d left being sent by a stopped manager, marked unclear", n)
	}
	ops, err := st.HeldOperations(sctx, db.HeldKindMessage)
	if err != nil {
		return fail(err)
	}
	rows := map[string][]*heldRow{}
	for _, op := range ops {
		rows[op.MachineID] = append(rows[op.MachineID], newHeldRow(op))
	}
	h.mu.Lock()
	h.rows, h.sending, h.loaded = rows, map[string]bool{}, true
	h.mu.Unlock()
	for machine := range rows { // rows drawn before the load showed none
		d.heldChanged(machine)
	}
	return true
}

// heldLoop loads the held messages, then sends what can go, fails what waited too long, until ctx ends.
func (d *Devices) heldLoop(ctx context.Context) {
	if d.heldStore() == nil {
		return
	}
	h := &d.held
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	t := time.NewTicker(heldTick)
	defer t.Stop()
	for {
		if d.heldLoad(ctx) {
			d.heldExpire(ctx)
			h.mu.Lock()
			machines := make([]string, 0, len(h.rows))
			for machine := range h.rows {
				machines = append(machines, machine)
			}
			h.mu.Unlock()
			for _, machine := range machines {
				d.heldWake(machine)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// heldChanged has machine's rows published again with what is held for them now (Options.HeldChanged).
func (d *Devices) heldChanged(machine string) {
	if f := d.opts.HeldChanged; f != nil {
		go f(machine)
	}
}

// ---- holding ----

// heldCall is a session.send the hub let the manager hold (NodeCall.Hold), as it would be kept.
type heldCall struct {
	machine, session, id string
	lang                 i18n.Lang
	call                 devicewire.NodeCall // as it came, but for Hold, TimeoutMS and Request
}

// heldCallOf is p as a call the manager may hold; nil when it may not: not a session.send the hub marked, without a
// session or its own client_msg_id, or no store to keep it in.
func (d *Devices) heldCallOf(m *Machine, p devicewire.NodeCall) *heldCall {
	if p.Method != "session.send" || p.Hold == nil || d.heldStore() == nil {
		return nil
	}
	var ids struct {
		SessionID string `json:"session_id"`
		ID        string `json:"client_msg_id"`
	}
	if json.Unmarshal(p.Params, &ids) != nil || ids.SessionID == "" || ids.ID == "" {
		return nil
	}
	call := p
	call.Hold, call.TimeoutMS, call.Request = nil, 0, ""
	return &heldCall{machine: m.cfg.ID, session: ids.SessionID, id: ids.ID, lang: i18n.Lang(p.Hold.Lang), call: call}
}

// heldNotWritten: err proves the call wrote nothing to the node. Only such a call may be held, or sent again by
// itself; anything less sure is taken as one that may have arrived. What proves it:
//   - errOffline: callOnce found no connection, or one already closed, before the call;
//   - rpc.ErrNotSent: marked only where nothing was written yet (the call's wait for the connection, for the writer or
//     the reload ending first; the connection closing while the call waited to write);
//   - rpc.ErrSuspended: refused before the call was registered;
//   - a hop further on answering machine_offline (it never went out there).
//
// Everything Call codes machine_offline (the hub's 503 node_unreachable, delivered "none") is one of these: on this
// path rpc.ErrClosed comes only as NotSent(ErrClosed) or as ErrDropped. Not taken: a bare ErrClosed (a wrapper that
// lost ErrDropped's identity would make a possible delivery look unwritten), and rpc.ErrNotWritten alone, which a
// writer's own context error after part of a frame went out (websocket.NetConn) also yields.
func heldNotWritten(err error) bool {
	if errors.Is(err, errOffline) || errors.Is(err, rpc.ErrNotSent) || errors.Is(err, rpc.ErrSuspended) {
		return true
	}
	var re *rpc.Error
	return errors.As(err, &re) && re.Code == devicewire.CodeMachineOffline
}

// heldMayHold: a call that failed with err is held. Not when its caller went away (cancelled): nobody waits for the
// answer, and its retry is held then.
func heldMayHold(err error) bool { return heldNotWritten(err) && !errors.Is(err, context.Canceled) }

// holdBehind holds c without calling the node when it must not overtake what its session holds (a message waiting,
// being sent, or one that may have arrived), or answers the one c is held as already. nil: c goes to the node.
func (d *Devices) holdBehind(ctx context.Context, c *heldCall) error {
	if !d.heldLoad(ctx) {
		return nil // cannot tell: as before
	}
	h := &d.held
	h.mu.Lock()
	held, blocked := false, false
	for _, r := range h.rows[c.machine] {
		if r.SessionID != c.session {
			continue
		}
		if r.OpID == c.id {
			held = true
			break
		}
		if r.State != db.HeldFailed || r.unclear() {
			blocked = true
		}
	}
	h.mu.Unlock()
	switch {
	case held:
		return d.heldAnswer(c.machine, c.session, c.id, c.lang)
	case !blocked:
		return nil
	}
	if err := d.hold(ctx, c); err != nil {
		return err
	}
	// Not kept, and it must not go before the ones waiting: nothing was typed, the app may try again.
	return &rpc.Error{Code: codeHeldNotKept, Message: i18n.T(c.lang, "devices-held.not_kept"),
		Data: json.RawMessage(`{"delivered":"none"}`)}
}

// hold keeps c and answers CodeHeld; nil when it could not be kept (nor the held ones read).
func (d *Devices) hold(ctx context.Context, c *heldCall) error {
	if !d.heldLoad(ctx) {
		return nil
	}
	raw, err := json.Marshal(c.call)
	if err != nil || len(raw) > heldMaxBytes {
		d.logf("[%s] %s: a message too large to hold (%d bytes)", c.machine, c.session, len(raw))
		return nil
	}
	h := &d.held
	now := h.clock().UTC()
	sctx, cancel := heldStoreCtx(ctx)
	defer cancel()
	st := d.heldStore()
	op, fresh, err := st.HoldOperation(sctx, db.HeldOperation{Kind: db.HeldKindMessage, OpID: c.id, MachineID: c.machine,
		SessionID: c.session, Call: raw, Lang: string(c.lang), SentAt: now, HeldAt: now})
	if err != nil {
		// It may be kept all the same (the answer lost after the commit): one look by its key decides, else a row the
		// app was told failed would go after the next start, maybe after the owner sent it again.
		lctx, lcancel := heldStoreCtx(ctx)
		kept, found, lerr := st.HeldOperationOf(lctx, db.HeldKindMessage, c.machine, c.session, c.id)
		lcancel()
		if lerr != nil || !found {
			d.logf("[%s] %s: a message could not be held: %v", c.machine, c.session, err)
			return nil
		}
		d.logf("[%s] %s: a message was held, though its store said %v", c.machine, c.session, err)
		op, fresh = kept, true
	}
	h.mu.Lock()
	rows := h.rows[op.MachineID]
	i, found := slices.BinarySearchFunc(rows, op.Seq, func(r *heldRow, seq int64) int { return cmp.Compare(r.Seq, seq) })
	if !found {
		h.rows[op.MachineID] = slices.Insert(rows, i, newHeldRow(op))
	}
	n := len(h.rows[op.MachineID])
	h.mu.Unlock()
	if fresh {
		d.logf("[%s] %s: a message held until its computer is back (%d held there)", c.machine, c.session, n)
		d.heldChanged(c.machine)
		d.heldWake(c.machine) // it may be back already
	}
	return d.heldAnswer(op.MachineID, op.SessionID, op.OpID, c.lang)
}

// heldAnswer is CodeHeld with held message id as its row shows it.
func (d *Devices) heldAnswer(machine, session, id string, lang i18n.Lang) error {
	h := &d.held
	var row heldRow
	found, behind := false, false
	h.mu.Lock()
	for _, r := range h.rows[machine] {
		if r.SessionID != session {
			continue
		}
		if r.OpID == id {
			row, found = *r, true
			break
		}
		behind = behind || r.unclear()
	}
	h.mu.Unlock()
	name := d.heldComputer(machine)
	msg := proto.UndeliveredMessage{ID: id, State: proto.UndeliveredWaiting, Computer: name, // gone meanwhile (sent, deleted)
		Error: i18n.T(lang, "devices-held.waiting", i18n.Args{"name": name})}
	if found {
		msg = row.wire(lang, name, behind)
	}
	data, _ := json.Marshal(devicewire.HeldAnswer{Delivered: "later", Undelivered: &msg})
	return &rpc.Error{Code: devicewire.CodeHeld, Message: msg.Error, Data: data}
}

// ---- what the rows show ----

// heldComputer is what machine is called now.
func (d *Devices) heldComputer(machine string) string {
	if m := d.machineByID(machine); m != nil {
		return m.name()
	}
	return machine
}

// HeldMessages is assistant.Devices': the messages held for machine's Agents, by session, oldest first, each with the
// line under it in lang; nil when there are none. It runs on every decoration of the rows: from memory.
func (d *Devices) HeldMessages(machine string, lang i18n.Lang) map[string][]proto.UndeliveredMessage {
	h := &d.held
	h.mu.Lock()
	rows := h.rows[machine]
	if len(rows) == 0 {
		h.mu.Unlock()
		return nil
	}
	snap := make([]heldRow, len(rows))
	for i, r := range rows {
		snap[i] = *r
	}
	h.mu.Unlock()
	name := d.heldComputer(machine)
	out := map[string][]proto.UndeliveredMessage{}
	behind := map[string]bool{} // a message of the session before it may have arrived
	for i := range snap {
		r := &snap[i]
		out[r.SessionID] = append(out[r.SessionID], r.wire(lang, name, behind[r.SessionID]))
		if r.unclear() {
			behind[r.SessionID] = true
		}
	}
	return out
}

// wire is r as its row shows it: the owner's message with the line under it.
func (r *heldRow) wire(lang i18n.Lang, computer string, behind bool) proto.UndeliveredMessage {
	m := proto.UndeliveredMessage{ID: r.OpID, Text: r.text, Images: r.images, At: r.SentAt, State: proto.UndeliveredWaiting,
		Computer: computer}
	switch {
	case r.State == db.HeldFailed:
		// Every way it stopped is failed on the wire: an older app takes any other state for one sent by itself.
		m.State = proto.UndeliveredFailed
		switch r.Failure {
		case db.HeldRefused:
			reason := r.Error
			if reason == "" {
				reason = i18n.T(lang, "devices-held.no_reason")
			}
			m.Error = i18n.T(lang, "devices-held.refused", i18n.Args{"reason": reason})
		case db.HeldExpired:
			m.Error = i18n.T(lang, "devices-held.expired", i18n.Args{"name": computer})
		default:
			m.Error = i18n.T(lang, "devices-held.unclear")
		}
	case r.State == db.HeldSending:
		m.Error = i18n.T(lang, "devices-held.sending")
	case behind:
		m.Error = i18n.T(lang, "devices-held.behind")
	default:
		m.Error = i18n.T(lang, "devices-held.waiting", i18n.Args{"name": computer})
	}
	return m
}

// ---- sending ----

// heldHead is what leads a session's held messages (headLocked).
type heldHead int

const (
	heldHeadNone    heldHead = iota // nothing to do now: none waits, or one ahead is being sent or changed
	heldHeadSend                    // the next one to send
	heldHeadUnclear                 // one that may have arrived holds the rest back
)

// headLocked is what leads the session's held messages: the first that goes next or holds the rest back. One being
// sent, or the owner's (Retry, Delete) or the expiry's to change now, holds the later ones back whatever its state
// (heldHeadNone); one that may have arrived too (heldHeadUnclear); a refused or expired one does not. h.mu is held.
func (h *heldBook) headLocked(machine, session string) (*heldRow, heldHead) {
	for _, r := range h.rows[machine] {
		if r.SessionID != session {
			continue
		}
		switch {
		case r.busy, r.State == db.HeldSending:
			return r, heldHeadNone
		case r.unclear():
			return r, heldHeadUnclear
		case r.State == db.HeldWaiting:
			return r, heldHeadSend
		}
	}
	return nil, heldHeadNone
}

// heldWake starts on machine what its held messages can do now: per session, sending the next one (heldOn says the
// node takes it), or reading the transcript once on this connection for one that may have arrived (heldLanded); one
// at a time per session, the sessions apart. Called when its node's session list changes (a new connection's first
// list once its node is configured: what was held while it was away goes then), on a Retry, a new held message, and
// every heldTick.
func (d *Devices) heldWake(machine string) {
	h := &d.held
	h.mu.Lock()
	ctx := h.ctx
	if !h.loaded || ctx == nil || ctx.Err() != nil || len(h.rows[machine]) == 0 {
		h.mu.Unlock()
		return
	}
	type due struct {
		session string
		unclear bool
		checked time.Time
	}
	var sessions []string
	for _, r := range h.rows[machine] {
		if !slices.Contains(sessions, r.SessionID) {
			sessions = append(sessions, r.SessionID)
		}
	}
	now := h.clock()
	var dues []due
	for _, session := range sessions {
		if h.sending[machine+"/"+session] {
			continue
		}
		switch r, head := h.headLocked(machine, session); {
		case head == heldHeadSend && !r.expired(now):
			dues = append(dues, due{session: session})
		case head == heldHeadUnclear:
			dues = append(dues, due{session: session, unclear: true, checked: r.checked})
		}
	}
	h.mu.Unlock()
	if len(dues) == 0 { // only failed ones, or every session's delivery runs: nothing to look up
		return
	}
	m := d.machineByID(machine)
	if m == nil {
		return
	}
	dues = slices.DeleteFunc(dues, func(e due) bool { // not under h.mu
		listed, ready := m.heldOn(e.session)
		if e.unclear {
			return listed.IsZero() || listed.Equal(e.checked)
		}
		return !ready
	})
	var start []string
	h.mu.Lock()
	for _, e := range dues {
		if key := machine + "/" + e.session; !h.sending[key] {
			h.sending[key] = true
			start = append(start, e.session)
		}
	}
	h.mu.Unlock()
	for _, session := range start {
		d.workers.Add(1)
		go func() {
			defer d.workers.Done()
			d.heldSend(ctx, machine, session)
		}()
	}
}

// heldOn is what m's node offers a held message to session now. listed: the start of the connection whose current
// list has the session, whatever its status (the node resumes a stopped one); zero when not connected or not listed.
// ready: a message may go: listed, or heldListWait into the connection without it, and the node's answer decides.
// Right after the handshake an Agent of a runtime is not back yet: sent then, the message would be refused for a
// session that is there a second later.
func (m *Machine) heldOn(session string) (listed time.Time, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.peer == nil {
		return time.Time{}, false
	}
	select {
	case <-m.peer.Done():
		return time.Time{}, false
	default:
	}
	if m.sessionsListed {
		for i := range m.sessions { // by index: a range value copies every (large) session it passes
			if m.sessions[i].SessionID == session {
				return m.connectedAt, true
			}
		}
	}
	return time.Time{}, !m.connectedAt.IsZero() && time.Since(m.connectedAt) >= heldListWait
}

func (h *heldBook) rowLocked(machine string, seq int64) *heldRow {
	for _, r := range h.rows[machine] {
		if r.Seq == seq {
			return r
		}
	}
	return nil
}

func (h *heldBook) dropLocked(machine string, seq int64) {
	h.rows[machine] = slices.DeleteFunc(h.rows[machine], func(r *heldRow) bool { return r.Seq == seq })
	if len(h.rows[machine]) == 0 {
		delete(h.rows, machine)
	}
}

// heldSend does what the session's held messages can do, in order, while ctx lasts: sends each next one while the node
// takes them (heldOn), and reads the transcript once per connection for one that may have arrived (heldLanded). An
// expired one is left to heldExpire.
func (d *Devices) heldSend(ctx context.Context, machine, session string) {
	h := &d.held
	st := d.heldStore()
	key := machine + "/" + session
	for {
		m := d.machineByID(machine)
		var listed time.Time
		ready := false
		if m != nil {
			listed, ready = m.heldOn(session)
		}
		now := h.clock()
		h.mu.Lock()
		r, head := h.headLocked(machine, session)
		act := heldHeadNone
		switch {
		case ctx.Err() != nil:
		case head == heldHeadSend && ready && !r.expired(now):
			act = heldHeadSend
		case head == heldHeadUnclear && !listed.IsZero() && !listed.Equal(r.checked):
			act, r.checked = heldHeadUnclear, listed // read once on this connection, whatever comes of it
		}
		if act == heldHeadNone {
			delete(h.sending, key) // decided with the rows locked: what comes after this goes with the next wake
			h.mu.Unlock()
			if m != nil && head != heldHeadNone && ctx.Err() == nil {
				if l, ok := m.heldOn(session); ok != ready || !l.Equal(listed) {
					d.heldWake(machine) // its node changed meanwhile: its wake found this delivery still running
				}
			}
			return
		}
		seq, id, call, lang := r.Seq, r.OpID, r.Call, r.Lang
		if act == heldHeadUnclear {
			h.mu.Unlock()
			if d.heldLanded(ctx, m, machine, session, seq, id) {
				continue // it did arrive: the next ones go
			}
			h.mu.Lock()
			delete(h.sending, key)
			h.mu.Unlock()
			return
		}
		r.State = db.HeldSending // from now on Delete answers that it is being sent
		h.mu.Unlock()
		sctx, cancel := heldStoreCtx(ctx)
		claimed, err := st.ClaimHeldOperation(sctx, seq, now)
		cancel()
		if err != nil || !claimed {
			// Not marked as being sent: it must not go. A store that disagrees (another manager took it) leaves it
			// to the owner.
			h.mu.Lock()
			if r := h.rowLocked(machine, seq); r != nil {
				if err != nil {
					r.State = db.HeldWaiting
				} else {
					r.State, r.Failure, r.Error = db.HeldFailed, db.HeldUnclear, ""
				}
			}
			delete(h.sending, key)
			h.mu.Unlock()
			d.logf("[%s] %s: a held message not sent: the store did not mark it as being sent (%v)", machine, session, err)
			d.heldChanged(machine)
			return
		}
		h.mu.Lock()
		if r := h.rowLocked(machine, seq); r != nil {
			r.TriedAt, r.Attempts = now, r.Attempts+1
		}
		h.mu.Unlock()
		d.heldChanged(machine)
		outcome, reason := d.heldDeliver(ctx, call, lang)
		if !d.heldSettle(ctx, machine, session, seq, outcome, reason) {
			h.mu.Lock()
			delete(h.sending, key)
			h.mu.Unlock()
			return
		}
	}
}

// heldLanded reads the session's latest turns for message id (seq), which may have arrived: there under its
// client_msg_id (never by its words), it did arrive and leaves the rows as sent. False when it is not there or the
// turns could not be read: it stays for the owner, and the later ones wait behind it. Nothing is sent.
func (d *Devices) heldLanded(ctx context.Context, m *Machine, machine, session string, seq int64, id string) bool {
	if m == nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, heldCheckWait)
	var res struct {
		Items []proto.Item `json:"items"`
	}
	err := m.call(cctx, "session.items", map[string]any{"session_id": session, "turns": 3, "lazy_images": true}, &res)
	cancel()
	if err != nil {
		d.logf("[%s] %s: a held message that may have arrived: its transcript could not be read: %v", machine, session, err)
		return false
	}
	if !slices.ContainsFunc(res.Items, func(it proto.Item) bool { return it.Role == "user" && it.ClientMsgID == id }) {
		d.logf("[%s] %s: a held message that may have arrived is not in its transcript: left to the owner", machine, session)
		return false
	}
	sctx, scancel := heldStoreCtx(ctx)
	defer scancel()
	ok, err := d.heldStore().DeleteHeldOperation(sctx, seq, db.HeldFailed)
	if err != nil || !ok { // gone, or the owner's Retry took it meanwhile: theirs
		d.logf("[%s] %s: a held message that arrived stays (%v)", machine, session, err)
		return false
	}
	h := &d.held
	h.mu.Lock()
	h.dropLocked(machine, seq)
	h.mu.Unlock()
	d.logf("[%s] %s: a held message that may have arrived is in its transcript: taken as sent", machine, session)
	d.heldChanged(machine)
	return true
}

// heldTry is a held message's delivery, in its context (heldDeliveryKey): callNode notes whether it called the node
// and what that call answered, so the outcome is judged on the node's call alone.
type heldTry struct {
	called bool
	err    error
}

// heldDeliveryKey marks a held message's delivery in its context (a *heldTry): its share receipt is settled only by an
// outcome that is final (callNode).
type heldDeliveryKey struct{}

// heldDeliver sends held call raw as the hub sent it (its caller, submission and share authority with it), and says
// what came of it, judged on the node's call alone: stopped before the node was asked (its preparation here failed),
// nothing was written and it is refused, with why; the node took it, it was delivered whatever came after.
func (d *Devices) heldDeliver(ctx context.Context, raw json.RawMessage, lang string) (heldOutcome, string) {
	l := i18n.Lang(lang)
	var p devicewire.NodeCall
	if err := json.Unmarshal(raw, &p); err != nil {
		return heldRefused, heldReason(l, err)
	}
	p.Hold = nil // never held again by its own delivery
	cctx, cancel := context.WithTimeout(ctx, heldSendTimeout)
	defer cancel()
	try := &heldTry{}
	cctx = i18n.With(context.WithValue(cctx, heldDeliveryKey{}, try), l)
	_, err := d.callNode(cctx, p)
	switch {
	case !try.called && ctx.Err() != nil:
		return heldNotSent, "" // the manager stopping before the node was asked: it waits again
	case !try.called:
		return heldRefused, heldReason(l, err)
	case try.err == nil:
		return heldDelivered, "" // the node took it (an answer too large to pass on comes after)
	}
	return heldOutcomeOf(try.err)
}

// heldReason is why a held message was refused, in words: the node's (or the manager's coded) message, else that it
// could not be made ready here.
func heldReason(l i18n.Lang, err error) string {
	var re *rpc.Error
	if errors.As(err, &re) && cmp.Or(re.Message, re.Code) != "" {
		return textfmt.Clip(textfmt.OneLine(cmp.Or(re.Message, re.Code)), 300)
	}
	return i18n.T(l, "devices-held.not_prepared")
}

// heldSettles: a call's share receipt is settled now (finishShareCall), which is always but for a held message's
// delivery that wrote nothing or was refused: it stays held and may go again, its receipt waiting for that.
func heldSettles(ctx context.Context, err error) bool {
	if err == nil || ctx.Value(heldDeliveryKey{}) == nil {
		return true
	}
	outcome, _ := heldOutcomeOf(err)
	return outcome == heldUnclear
}

type heldOutcome int

const (
	heldDelivered heldOutcome = iota
	heldNotSent               // nothing written: it waits again
	heldRefused               // the node refused it, nothing typed
	heldUnclear               // it may have arrived: never sent again by itself
)

// heldOutcomeOf is what a node call that ended with err did, and the node's words for a refusal.
func heldOutcomeOf(err error) (heldOutcome, string) {
	if err == nil {
		return heldDelivered, ""
	}
	if heldNotWritten(err) {
		return heldNotSent, ""
	}
	var re *rpc.Error
	if !errors.As(err, &re) {
		return heldUnclear, "" // the connection dropped with it out, its time ran out, the manager stopped during it
	}
	var data struct {
		Delivered string `json:"delivered"`
	}
	_ = json.Unmarshal(re.Data, &data)
	switch {
	case data.Delivered == "unknown", re.Code == devicewire.CodeNodeClosed, re.Code == devicewire.CodeDeadline:
		return heldUnclear, ""
	case re.Code == "" && (re.Message == rpc.ErrClosed.Error() || re.Message == context.DeadlineExceeded.Error() ||
		re.Message == context.Canceled.Error()):
		return heldUnclear, "" // a hop further on relayed its own drop or wait
	}
	return heldRefused, textfmt.Clip(textfmt.OneLine(cmp.Or(re.Message, re.Code)), 300)
}

// heldSettle keeps what delivery seq did; false: the session's delivery stops here (nothing written: its computer
// went; or it may have arrived, and the later ones wait behind it).
func (d *Devices) heldSettle(ctx context.Context, machine, session string, seq int64, outcome heldOutcome, reason string) bool {
	h := &d.held
	st := d.heldStore()
	sctx, cancel := heldStoreCtx(ctx)
	defer cancel()
	defer d.heldChanged(machine)
	if outcome == heldDelivered {
		if _, err := st.DeleteHeldOperation(sctx, seq, db.HeldSending); err != nil {
			// Still marked as being sent in the store: a restart shows it as one that may have arrived.
			d.logf("[%s] %s: a held message was sent but stays in the store: %v", machine, session, err)
		}
		h.mu.Lock()
		h.dropLocked(machine, seq)
		h.mu.Unlock()
		d.logf("[%s] %s: a held message was sent", machine, session)
		return true
	}
	state, failure := db.HeldFailed, ""
	switch outcome {
	case heldNotSent:
		state, reason = db.HeldWaiting, ""
	case heldRefused:
		failure = db.HeldRefused
	default:
		failure, reason = db.HeldUnclear, ""
	}
	ok, err := st.SettleHeldOperation(sctx, seq, db.HeldSending, state, failure, reason)
	if err != nil || !ok {
		// The store still has it as being sent: what a restart shows of it, so here too.
		d.logf("[%s] %s: a held message's outcome not kept (%v): taken as unclear", machine, session, err)
		state, failure, reason = db.HeldFailed, db.HeldUnclear, ""
	}
	h.mu.Lock()
	if r := h.rowLocked(machine, seq); r != nil {
		r.State, r.Failure, r.Error = state, failure, reason
	}
	h.mu.Unlock()
	switch {
	case state == db.HeldWaiting:
		d.logf("[%s] %s: a held message waits again: its computer is away", machine, session)
		return false
	case failure == db.HeldRefused:
		d.logf("[%s] %s: a held message was refused: %s", machine, session, reason)
		return true // nothing typed: the next one goes
	}
	d.logf("[%s] %s: a held message may have arrived: left to the owner", machine, session)
	return false
}

// heldExpire, the one place a message expires, fails each that has waited heldExpiry for its computer. One held
// behind a message that may have arrived waits for the owner, not for its computer: it never expires there, and its
// wait starts again every heldBlockedRenew, so it has nearly a day once the owner decides.
func (d *Devices) heldExpire(ctx context.Context) {
	h := &d.held
	now := h.clock()
	type due struct {
		machine string
		seq     int64
		renew   bool
	}
	var dues []due
	h.mu.Lock()
	for machine, rows := range h.rows {
		behind := map[string]bool{}
		for _, r := range rows {
			switch {
			case r.unclear():
				behind[r.SessionID] = true
			case r.State != db.HeldWaiting || r.busy:
			case behind[r.SessionID] && now.Sub(r.HeldAt) >= heldBlockedRenew:
				r.busy = true // neither sent nor changed by the owner meanwhile
				dues = append(dues, due{machine, r.Seq, true})
			case !behind[r.SessionID] && r.expired(now):
				r.busy = true
				dues = append(dues, due{machine, r.Seq, false})
			}
		}
	}
	h.mu.Unlock()
	changed := map[string]bool{}
	for _, e := range dues {
		sctx, cancel := heldStoreCtx(ctx)
		var ok bool
		var err error
		if e.renew {
			ok, err = d.heldStore().RenewHeldOperation(sctx, e.seq, now)
		} else {
			ok, err = d.heldStore().SettleHeldOperation(sctx, e.seq, db.HeldWaiting, db.HeldFailed, db.HeldExpired, "")
		}
		cancel()
		h.mu.Lock()
		if r := h.rowLocked(e.machine, e.seq); r != nil {
			r.busy = false
			switch {
			case err != nil: // the next look tries again
			case ok && e.renew:
				r.HeldAt = now
			case ok:
				r.State, r.Failure, r.Error = db.HeldFailed, db.HeldExpired, ""
				changed[e.machine] = true
			case !e.renew: // the store has it otherwise: left to the owner
				r.State, r.Failure, r.Error = db.HeldFailed, db.HeldUnclear, ""
				changed[e.machine] = true
			}
		}
		h.mu.Unlock()
		switch {
		case err != nil:
			d.logf("[%s] a held message's wait not kept: %v", e.machine, err)
		case ok && !e.renew:
			d.logf("[%s] a held message expired: its computer was away for %s", e.machine, heldExpiry)
		}
	}
	for machine := range changed {
		d.heldChanged(machine)
		d.heldWake(machine) // the ones an expired message held back go now
	}
}

// ---- the owner's Retry and Delete ----

var (
	// errHeldSendingNow: the message is being sent this moment: Delete could not take it back.
	errHeldSendingNow = i18n.Err("devices-held.sending_now")
	// errHeldStore: the store did not take the change.
	errHeldStore = i18n.Err("devices-held.store_failed")
)

// heldFind finds held message msg of session sid of machine ("": any machine) and marks it busy for the owner's
// change; nil when there is none. err: it is being sent now.
func (h *heldBook) heldFind(machine, sid, msg string) (*heldRow, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for m, rows := range h.rows {
		if machine != "" && m != machine {
			continue
		}
		for _, r := range rows {
			if r.SessionID != sid || r.OpID != msg {
				continue
			}
			if r.State == db.HeldSending || r.busy {
				return nil, errHeldSendingNow
			}
			r.busy = true
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

// heldRetry is the owner's Retry: a message that failed waits again, a new day from now, and goes at once when its
// computer is there (one that may have arrived too: the owner checked). found false: no such held message.
func (d *Devices) heldRetry(ctx context.Context, machine, sid, msg string) (bool, error) {
	if !d.heldLoad(ctx) {
		return false, nil
	}
	h := &d.held
	r, err := h.heldFind(machine, sid, msg)
	if r == nil {
		return err != nil, err
	}
	now := h.clock()
	ok, err := true, error(nil)
	if r.State != db.HeldWaiting {
		sctx, cancel := heldStoreCtx(ctx)
		ok, err = d.heldStore().RetryHeldOperation(sctx, r.Seq, now)
		cancel()
	}
	held := true
	if err == nil && !ok { // the store has it otherwise (only a failed one is retried)
		held, err = d.heldMirror(ctx, r)
	}
	h.mu.Lock()
	row := h.rowLocked(r.MachineID, r.Seq)
	if row != nil {
		row.busy = false
	}
	switch {
	case err != nil:
		h.mu.Unlock()
		if !errors.Is(err, errHeldSendingNow) {
			d.logf("[%s] %s: a held message's retry not kept: %v", r.MachineID, sid, err)
			err = errHeldStore
		}
		return true, err
	case ok && row != nil && row.State != db.HeldWaiting:
		row.State, row.Failure, row.Error, row.HeldAt = db.HeldWaiting, "", "", now
	}
	h.mu.Unlock()
	d.heldChanged(r.MachineID)
	if !held {
		return false, nil
	}
	d.logf("[%s] %s: a held message retried by the owner", r.MachineID, sid)
	d.heldWake(r.MachineID)
	return true, nil
}

// heldDelete is the owner's Delete on a message that waits or failed; errHeldSendingNow while it is being sent.
func (d *Devices) heldDelete(ctx context.Context, machine, sid, msg string) (bool, error) {
	if !d.heldLoad(ctx) {
		return false, nil
	}
	h := &d.held
	r, err := h.heldFind(machine, sid, msg)
	if r == nil {
		return err != nil, err
	}
	sctx, cancel := heldStoreCtx(ctx)
	defer cancel()
	ok, err := d.heldStore().DeleteHeldOperation(sctx, r.Seq) // never one being sent
	if err == nil && !ok {
		var held bool
		if held, err = d.heldMirror(ctx, r); err == nil && held {
			err = errHeldSendingNow // the store has it, and not waiting or failed: being sent
		}
	}
	h.mu.Lock()
	if err != nil {
		if row := h.rowLocked(r.MachineID, r.Seq); row != nil {
			row.busy = false
		}
		h.mu.Unlock()
		if !errors.Is(err, errHeldSendingNow) {
			d.logf("[%s] %s: a held message's delete not kept: %v", r.MachineID, sid, err)
			err = errHeldStore
		}
		return true, err
	}
	h.dropLocked(r.MachineID, r.Seq)
	h.mu.Unlock()
	d.heldUnshare(sctx, r)
	d.logf("[%s] %s: a held message deleted by the owner", r.MachineID, sid)
	d.heldChanged(r.MachineID)
	d.heldWake(r.MachineID) // the ones it held back go now
	return true, nil
}

// heldMirror takes into memory what the store has of r after it did not take the owner's change: gone, it is
// dropped (held false); waiting or failed, so it is here too; being sent (by another manager on the same database, in
// a switch-over), errHeldSendingNow, and it is left as it is here (a restart shows it unclear).
func (d *Devices) heldMirror(ctx context.Context, r *heldRow) (bool, error) {
	sctx, cancel := heldStoreCtx(ctx)
	defer cancel()
	op, found, err := d.heldStore().HeldOperationOf(sctx, r.Kind, r.MachineID, r.SessionID, r.OpID)
	if err != nil {
		return true, err
	}
	h := &d.held
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !found:
		h.dropLocked(r.MachineID, r.Seq)
		return false, nil
	case op.State == db.HeldSending:
		return true, errHeldSendingNow
	}
	if row := h.rowLocked(r.MachineID, r.Seq); row != nil {
		row.State, row.Failure, row.Error, row.HeldAt = op.State, op.Failure, op.Error, op.HeldAt
	}
	return true, nil
}

// heldUnshare settles a deleted message's share receipt, which its hold left open, as never delivered.
func (d *Devices) heldUnshare(ctx context.Context, r *heldRow) {
	store := d.shareDB()
	if store == nil {
		return
	}
	var p devicewire.NodeCall
	if json.Unmarshal(r.Call, &p) != nil || p.Submission == nil || p.Submission.ID == "" {
		return
	}
	_ = store.SettleShareSubmission(ctx, p.Submission.ID, p.MachineID, r.SessionID, "failed")
}

// heldRPCError is a Retry's or Delete's error for the assistant's routes: coded, in ctx's language.
func heldRPCError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	code := "internal"
	if errors.Is(err, errHeldSendingNow) {
		code = "conflict"
	}
	return &rpc.Error{Code: code, Message: i18n.Localize(err, i18n.From(ctx).Or(i18n.Default))}
}

// HeldRetry is assistant.Devices'.
func (d *Devices) HeldRetry(ctx context.Context, sid, msg string) (bool, error) {
	found, err := d.heldRetry(ctx, "", sid, msg)
	return found, heldRPCError(ctx, err)
}

// HeldDelete is assistant.Devices'.
func (d *Devices) HeldDelete(ctx context.Context, sid, msg string) (bool, error) {
	found, err := d.heldDelete(ctx, "", sid, msg)
	return found, heldRPCError(ctx, err)
}

// postHeldRetry: POST /api/v1/sessions/{mid}/{sid}/held/{msg}/retry → {"ok":true}.
func (d *Devices) postHeldRetry(w http.ResponseWriter, r *http.Request, m *Machine) {
	d.heldRoute(w, r, m, d.heldRetry)
}

// deleteHeld: DELETE /api/v1/sessions/{mid}/{sid}/held/{msg} → {"ok":true}.
func (d *Devices) deleteHeld(w http.ResponseWriter, r *http.Request, m *Machine) {
	d.heldRoute(w, r, m, d.heldDelete)
}

// heldRoute serves the owner's change of a held message from a person's full device: 404 when there is no such message,
// 409 while it is being sent.
func (d *Devices) heldRoute(w http.ResponseWriter, r *http.Request, m *Machine,
	act func(ctx context.Context, machine, sid, msg string) (bool, error)) {
	// A Retry sends it again with its sender's provenance: only a person's own full device may (not the hub's token, not
	// a pairing code), as the hub says.
	if r.Header.Get(assistantwire.RoleHeader) != "full" || r.Header.Get(assistantwire.PersonHeader) != "1" {
		writeErrCode(w, http.StatusForbidden, proto.CodeReadOnly, i18n.Err("devices-held.read_only"))
		return
	}
	found, err := act(r.Context(), m.cfg.ID, r.PathValue("sid"), r.PathValue("msg"))
	switch {
	case errors.Is(err, errHeldSendingNow):
		writeErrCode(w, http.StatusConflict, "conflict", err)
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, err)
	case !found:
		writeErr(w, http.StatusNotFound, i18n.Err("devices-held.not_found"))
	default:
		writeJSON(w, map[string]any{"ok": true})
	}
}
