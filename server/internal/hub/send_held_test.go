package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/devicewire"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

// The owner's message to an Agent whose computer is away (#10661): the hub marks it holdable (NodeCall.Hold) and the
// machine's manager, here played on the real bus, holds it and answers devicewire.CodeHeld.

// holdManager is the devices manager of machine "box", whose computer is away: a session.send with Hold is held
// (CodeHeld, as internal/devices held.go answers), any other one is refused machine_offline as before. ignoresHold
// plays a manager from before holding. calls are the node calls it got.
type holdManager struct {
	mu          sync.Mutex
	calls       []devicewire.NodeCall
	ignoresHold bool
}

func (k *holdManager) sends() []devicewire.NodeCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []devicewire.NodeCall
	for _, c := range k.calls {
		if c.Method == "session.send" {
			out = append(out, c)
		}
	}
	return out
}

// holdHub is a hub with machine "box" (shown as "Studio Mac"), carried by its manager, listing row; its manager is k.
func holdHub(t *testing.T, row proto.Session) (*Hub, *Machine, *holdManager) {
	t.Helper()
	h := newTestHub(t)
	h.store = newSentStore()
	m := newMachine(h, MachineConfig{ID: "box", Name: "Studio Mac"})
	m.managed, m.peer = true, &busNode{h: h, id: "box", done: make(chan struct{})}
	registerMachine(h, m)
	editSessions(m, func([]proto.Session) []proto.Session { return []proto.Session{row} })
	h.bus.idents = &memIdents{}
	mux := http.NewServeMux()
	h.busRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	token, err := h.createBusToken(context.Background(), assistantwire.AssistantID, "Test manager")
	if err != nil {
		t.Fatal(err)
	}
	k := &holdManager{}
	connectGuardManagerWith(t, srv.URL, assistantwire.AssistantID, token, func(method string, raw json.RawMessage) (any, error) {
		switch method {
		case assistantwire.MethodSendParams: // an assistant conversation's message runs as it came
			var p assistantwire.SendParams
			_ = json.Unmarshal(raw, &p)
			return map[string]any{"params": p.Params}, nil
		case devicewire.MethodCall:
		default:
			return map[string]any{}, nil
		}
		var c devicewire.NodeCall
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		k.mu.Lock()
		k.calls = append(k.calls, c)
		ignores := k.ignoresHold
		k.mu.Unlock()
		if c.Method != "session.send" {
			return map[string]any{}, nil
		}
		if c.Hold == nil || ignores {
			return nil, &rpc.Error{Code: devicewire.CodeMachineOffline, Message: "Studio Mac is offline"}
		}
		var p struct {
			ID   string `json:"client_msg_id"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(c.Params, &p)
		data, _ := json.Marshal(devicewire.HeldAnswer{Delivered: "later", Undelivered: &proto.UndeliveredMessage{ID: p.ID,
			Text: p.Text, At: time.Now().UTC(), State: proto.UndeliveredWaiting, Computer: "Studio Mac",
			Error: "Will send when Studio Mac is back"}})
		return nil, &rpc.Error{Code: devicewire.CodeHeld, Message: "Will send when Studio Mac is back", Data: data}
	})
	h.devices.mu.Lock()
	h.devices.upOpen = true
	h.devices.mu.Unlock()
	return h, m, k
}

// agentRow is an Agent the hub lists while its computer is away.
var agentRow = proto.Session{SessionID: "s1", Agent: "claude", Launcher: "claude", Cwd: "/home/me/project", Status: proto.StatusUnknown}

func sendTo(h *Hub, m *Machine, sid, lang, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/box/"+sid+"/messages", strings.NewReader(body))
	r.SetPathValue("sid", sid)
	if lang != "" {
		r.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	h.sendMessage(w, r, m)
	return w
}

// The owner's message to an Agent whose computer is away goes to the manager holdable, in the request's language, and
// the manager's held answer is the app's: 202 delivered "later" with the message as its row shows it, the chat told it
// is kept, the send record settled later. A retry of the id, in this hub or after a restart, is answered as kept and
// never sent again.
func TestAgentMessageHeldWhileTheComputerIsAway(t *testing.T) {
	h, m, k := holdHub(t, agentRow)
	c := sendSubscriber(h, "box", "s1")
	w := sendTo(h, m, "s1", "ja", `{"text":"run the tests","client_msg_id":"held-1"}`)
	var out struct {
		Delivered   string                    `json:"delivered"`
		Undelivered *proto.UndeliveredMessage `json:"undelivered"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusAccepted || out.Delivered != sendLater ||
		out.Undelivered == nil || out.Undelivered.ID != "held-1" || out.Undelivered.Computer != "Studio Mac" {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	sends := k.sends()
	if len(sends) != 1 || sends[0].Hold == nil || sends[0].Hold.Lang != "ja" || sends[0].Caller == nil || sends[0].MachineID != "box" {
		t.Fatalf("node calls %+v, want one holdable session.send in ja with its caller", sends)
	}
	var params map[string]any
	if err := json.Unmarshal(sends[0].Params, &params); err != nil || params["text"] != "run the tests" ||
		params["client_msg_id"] != "held-1" || params["session_id"] != "s1" {
		t.Fatalf("held params %s", sends[0].Params)
	}
	if event := nextSendState(t, c); event.State != proto.MessageSendProcessing {
		t.Fatalf("first state %+v", event)
	}
	if event := nextSendState(t, c); event.State != proto.MessageSendKept {
		t.Fatalf("final state %+v", event)
	}
	if st, _ := h.store.ClaimSend(context.Background(), "held-1", "box", "s1"); st != sendLater {
		t.Fatalf("send record %q, want %q", st, sendLater)
	}
	if again := sendTo(h, m, "s1", "ja", `{"text":"run the tests","client_msg_id":"held-1"}`); again.Code != http.StatusAccepted ||
		!strings.Contains(again.Body.String(), `"delivered":"later"`) {
		t.Fatalf("retry: %d %s", again.Code, again.Body)
	}
	h.sends.mu.Lock()
	clear(h.sends.flights) // a hub that restarted remembers only its Postgres record
	h.sends.mu.Unlock()
	if again := sendTo(h, m, "s1", "ja", `{"text":"run the tests","client_msg_id":"held-1"}`); again.Code != http.StatusAccepted ||
		!strings.Contains(again.Body.String(), `"delivered":"later"`) {
		t.Fatalf("retry after a restart: %d %s", again.Code, again.Body)
	}
	if n := len(k.sends()); n != 1 {
		t.Fatalf("sent %d times, want once", n)
	}
}

// Only a plain message to an Agent the hub lists may be held. A message without an id cannot go once later; a to-do's
// hand-off, an Agent's first prompt and a launcher switch are decided now; an assistant conversation keeps its own
// (keepUndelivered); a session the hub does not list may be one. None carries Hold, and each fails as before.
func TestOnlyAPlainAgentMessageIsHoldable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		row   proto.Session
		sid   string
		body  string
		fresh bool
	}{
		{name: "no client_msg_id", row: agentRow, sid: "s1", body: `{"text":"hi"}`},
		{name: "a launcher switch", row: agentRow, sid: "s1", body: `{"text":"hi","client_msg_id":"x1","apply_switch":true}`},
		{name: "a to-do's hand-off", row: agentRow, sid: "s1", body: `{"text":"hi","client_msg_id":"x2","todo_id":"t1"}`},
		{name: "an Agent's first prompt", row: agentRow, sid: "s1", body: `{"text":"hi","client_msg_id":"x3"}`, fresh: true},
		{name: "an assistant conversation", row: proto.Session{SessionID: "s1", Assistant: true, Status: proto.StatusUnknown},
			sid: "s1", body: `{"text":"hi","client_msg_id":"x4"}`},
		{name: "a session not listed", row: agentRow, sid: "s9", body: `{"text":"hi","client_msg_id":"x5"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, m, k := holdHub(t, tc.row)
			if tc.fresh {
				h.fresh.add("box", tc.sid, "t1", "/home/me/project")
			}
			w := sendTo(h, m, tc.sid, "en", tc.body)
			sends := k.sends()
			if len(sends) != 1 || sends[0].Hold != nil {
				t.Fatalf("node calls %+v, want one session.send without Hold", sends)
			}
			if w.Code < http.StatusBadRequest {
				t.Fatalf("answer %d %s, want the failure as before", w.Code, w.Body)
			}
		})
	}
}

// A manager from before holding ignores Hold: the app gets machine_offline as before, and a retry may send it.
func TestHoldableSendToAnOlderManagerFailsAsBefore(t *testing.T) {
	h, m, k := holdHub(t, agentRow)
	k.mu.Lock()
	k.ignoresHold = true
	k.mu.Unlock()
	if w := sendTo(h, m, "s1", "en", `{"text":"hi","client_msg_id":"old-1"}`); w.Code != http.StatusServiceUnavailable ||
		strings.Contains(w.Body.String(), `"later"`) {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	if st, _ := h.store.ClaimSend(context.Background(), "old-1", "box", "s1"); st != "" {
		t.Fatalf("send record %q: a retry could not send it", st)
	}
}

// Hold goes on a session.send only: any other call under the same context goes as before.
func TestHoldOnlyOnSessionSend(t *testing.T) {
	h, _, k := holdHub(t, agentRow)
	ctx := withHold(context.Background(), "en")
	node := &busNode{h: h, id: "box", done: make(chan struct{})}
	for _, method := range []string{"session.get", "session.interrupt", "session.send"} {
		if err := node.Call(ctx, method, map[string]any{"session_id": "s1"}, nil); err != nil && method != "session.send" {
			t.Fatalf("%s: %v", method, err)
		}
	}
	if err := node.Call(context.Background(), "session.send", map[string]any{"session_id": "s1"}, nil); err == nil {
		t.Fatal("an unmarked send was held")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.calls) != 4 {
		t.Fatalf("calls %+v", k.calls)
	}
	for i, c := range k.calls {
		if (c.Hold != nil) != (i == 2) {
			t.Fatalf("call %d (%s) carried Hold %+v", i, c.Method, c.Hold)
		}
	}
}
