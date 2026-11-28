package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/db"
	"github.com/nwverse/persona/server/internal/proto"
)

// An open plan's Start presentation is computed where it is read (#17853): current tasks.trust coverage and the recorded
// wait decide whether Start These Directly is useful. Start itself is not stored, so a read after persistence agrees.
func TestPlanStartPresentation(t *testing.T) {
	ctx := context.Background()
	current := false
	trusted := PlanTask{Machine: "box", Cwd: "/home/me/src/tool", ConfirmOutbound: &current}
	uncovered := PlanTask{Machine: "box", Cwd: "/home/me/src/other", ConfirmOutbound: &current}
	readonly := PlanTask{Machine: "box", Cwd: "/home/me/src/readonly", Readonly: true, ConfirmOutbound: &current}

	t.Run("mixed plan offers only the uncovered task after persistence", func(t *testing.T) {
		s, _ := trustFixture(t)
		s.trustKinds(ctx, []PlanTask{trusted})
		p := &TaskPlan{ID: "plan_mixed", Conversation: "side", State: planOpen, Tasks: []PlanTask{trusted, uncovered, readonly}}
		raw, err := json.Marshal(p.Tasks)
		if err != nil {
			t.Fatal(err)
		}
		read := planFromRow(db.TaskPlanRow{ID: p.ID, Conversation: p.Conversation, State: p.State, Tasks: raw})
		got := s.planWithSpace(ctx, read)
		if got.Start == nil {
			t.Fatal("current hub omitted the open plan's start presentation")
		}
		if got.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || !reflect.DeepEqual(got.Start.Remember, []int{1}) {
			t.Fatalf("start = %+v, want start_authorization with only task 1", got.Start)
		}
		if p.Start != nil {
			t.Fatal("presentation mutated or was stored on the source plan")
		}
	})

	t.Run("direct GET returns the read-time projection", func(t *testing.T) {
		s, _, st := roleContractStoredService(t)
		raw, err := json.Marshal([]PlanTask{uncovered})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := st.CreateTaskPlan(ctx, db.TaskPlanRow{ID: "plan_get", Conversation: "side", State: planOpen, Tasks: raw, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		w := request(s, http.MethodGet, "/api/v1/task-plans/plan_get", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET plan: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Plan *TaskPlan `json:"plan"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Plan == nil || body.Plan.Start == nil || body.Plan.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || !reflect.DeepEqual(body.Plan.Start.Remember, []int{0}) {
			t.Fatalf("GET plan start = %+v, want ordinary authorization for task 0", body.Plan)
		}
	})

	t.Run("covered and read-only tasks offer nothing", func(t *testing.T) {
		s, _ := trustFixture(t)
		s.trustKinds(ctx, []PlanTask{trusted})
		got := s.planWithSpace(ctx, &TaskPlan{ID: "plan_covered", Conversation: "side", State: planOpen, Tasks: []PlanTask{trusted, readonly}})
		if got.Start == nil || got.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || len(got.Start.Remember) != 0 {
			t.Fatalf("covered plan start = %+v, want start_authorization with no remember indexes", got.Start)
		}
	})

	for _, tc := range []struct {
		name   string
		task   PlanTask
		reason assistantwire.PlanStartReason
	}{
		{name: "outbound confirmation", task: PlanTask{Machine: "box", Cwd: "/home/me/src/other", MemoryTopic: []string{"health"}}, reason: assistantwire.PlanStartReasonOutboundConfirmation},
		{name: "owner step", task: PlanTask{Machine: "box", Cwd: "/home/me/src/other", OwnerStep: ownerStepMoney}, reason: assistantwire.PlanStartReasonOwnerStep},
		{name: "request gate", task: PlanTask{Machine: "box", Cwd: "/home/me/src/other", RequestGate: "todo_request"}, reason: assistantwire.PlanStartReasonRequestGate},
		{name: "ordinary conversation continuation", task: PlanTask{Machine: "box", Cwd: "/home/me/src/other", Continues: "todo_previous", HandoffOperationID: "op_previous"}, reason: assistantwire.PlanStartReasonContinuation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := trustFixture(t)
			got := s.planWithSpace(ctx, &TaskPlan{ID: "plan_gate", Conversation: "side", State: planOpen, Tasks: []PlanTask{tc.task}})
			if got.Start == nil || got.Start.Reason != tc.reason || len(got.Start.Remember) != 0 {
				t.Fatalf("start = %+v, want %q and no trust offer", got.Start, tc.reason)
			}
		})
	}

	t.Run("authorized direction continuation keeps the ordinary trust offer", func(t *testing.T) {
		s, _ := trustFixture(t)
		setSettings(t, s, func(v *Settings) { v.Leaders = []string{"lead"} })
		task := uncovered
		task.Continues, task.HandoffOperationID = "todo_previous", "op_previous"
		got := s.planWithSpace(ctx, &TaskPlan{ID: "plan_lead", Conversation: "lead", State: planOpen, Tasks: []PlanTask{task}})
		if got.Start == nil || got.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || !reflect.DeepEqual(got.Start.Remember, []int{0}) {
			t.Fatalf("authorized continuation start = %+v", got.Start)
		}
	})

	t.Run("feed presentation is recomputed without changing stored bytes", func(t *testing.T) {
		s, _ := trustFixture(t)
		raw, err := json.Marshal(map[string]any{"plan": &TaskPlan{ID: "plan_feed", Conversation: "side", State: planOpen, Tasks: []PlanTask{uncovered}}})
		if err != nil {
			t.Fatal(err)
		}
		stored := FeedItem{Kind: feedPlan, Body: raw}
		presented := s.presentPlanFeed(ctx, stored)
		if string(stored.Body) != string(raw) {
			t.Fatal("presenting a feed item mutated its stored body")
		}
		var body struct {
			Plan *TaskPlan `json:"plan"`
		}
		if err := json.Unmarshal(presented.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Plan == nil || body.Plan.Start == nil || body.Plan.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || !reflect.DeepEqual(body.Plan.Start.Remember, []int{0}) {
			t.Fatalf("feed plan start = %+v, want ordinary authorization for task 0", body.Plan)
		}
	})

	t.Run("paginated and cursor feed reads return the projection", func(t *testing.T) {
		f := newFakeHub().withMachine()
		f.devices = []Device{{ID: "box", Name: "Box", Home: "/home/me", HomeReal: "/data/home/me", Online: true, Reachable: true}}
		s := newServiceOn(t, f, nil)
		p := &TaskPlan{ID: "plan_paged", Conversation: "side", State: planOpen, Tasks: []PlanTask{uncovered}}
		raw, err := json.Marshal(map[string]any{"plan": p})
		if err != nil {
			t.Fatal(err)
		}
		s.tasks.plans[p.ID] = p
		s.tasks.feed[p.Conversation] = []FeedItem{{ID: "feed_plan", Conversation: p.Conversation, At: time.Now().UTC(), Kind: feedPlan, Ref: p.ID, Body: raw}}
		cursorRaw, err := json.Marshal(feedCursor{Version: 1, Conversation: p.Conversation, Until: time.Now().UTC().Add(time.Hour), AfterAt: time.Unix(0, 0).UTC(), AfterID: "0"})
		if err != nil {
			t.Fatal(err)
		}
		queries := map[string]string{
			"paginate": "?paginate=1",
			"cursor":   "?cursor=" + base64.RawURLEncoding.EncodeToString(cursorRaw),
		}
		for name, query := range queries {
			t.Run(name, func(t *testing.T) {
				w := request(s, http.MethodGet, "/api/v1/assistant/conversations/side/feed"+query, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET feed: %d %s", w.Code, w.Body.String())
				}
				var response struct {
					Items []FeedItem `json:"items"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Items) != 1 {
					t.Fatalf("feed items = %d, want 1", len(response.Items))
				}
				var body struct {
					Plan *TaskPlan `json:"plan"`
				}
				if err := json.Unmarshal(response.Items[0].Body, &body); err != nil {
					t.Fatal(err)
				}
				if body.Plan == nil || body.Plan.Start == nil || body.Plan.Start.Reason != assistantwire.PlanStartReasonStartAuthorization || !reflect.DeepEqual(body.Plan.Start.Remember, []int{0}) {
					t.Fatalf("paginated feed plan start = %+v, want ordinary authorization for task 0", body.Plan)
				}
			})
		}
	})

	t.Run("an unknown outbound decision is not offered as trust and needs no read-time model", func(t *testing.T) {
		st := &planStartInfoStore{
			profileTaskStore: &profileTaskStore{memStore: &memStore{}, plans: map[string]db.TaskPlanRow{}},
			tracks:           []db.Track{{Slug: "qa-plain", Kind: trackKindSpace, Template: trackTemplateSpace}},
			info:             &infoCategoryMemStore{trackRegistryStore: &trackRegistryStore{memStore: &memStore{}}},
		}
		s, f := newQuietServiceOn(t, st)
		f.infoTagModel, f.infoTagFails = true, true
		s.noInfoTag = false
		f.setSessions("server", proto.Session{SessionID: "qa-plain-1", MachineID: "server", Assistant: true, Space: "qa-plain"})
		res, err := s.taskPlanFrom(ctx, "server", "qa-plain-1", planArgs(false), nil)
		if err != nil {
			t.Fatal(err)
		}
		planID := planIDRe.FindString(res)
		if planID == "" {
			t.Fatalf("task_plan returned no plan id: %s", res)
		}
		row, err := st.TaskPlan(ctx, planID)
		if err != nil {
			t.Fatal(err)
		}
		p := planFromRow(*row)
		if len(p.Tasks) != 1 || p.Tasks[0].ConfirmOutbound == nil || !*p.Tasks[0].ConfirmOutbound || len(p.Tasks[0].MemoryTopic) != 0 {
			t.Fatalf("unknown outbound decision was not persisted without categories: %+v", p.Tasks)
		}
		asked := len(f.infoTagAsked)
		if asked == 0 {
			t.Fatal("the fixture did not exercise information tagging")
		}
		for range 2 {
			got := s.planWithSpace(ctx, p)
			if got.Start == nil || got.Start.Reason != assistantwire.PlanStartReasonOutboundConfirmation || len(got.Start.Remember) != 0 {
				t.Fatalf("unknown outbound start = %+v, want outbound_confirmation without trust", got.Start)
			}
		}
		if len(f.infoTagAsked) != asked {
			t.Fatalf("presenting the plan invoked the tagging model again: %d -> %d", asked, len(f.infoTagAsked))
		}
	})

	t.Run("a legacy plan without an outbound decision keeps legacy presentation", func(t *testing.T) {
		s, _ := trustFixture(t)
		legacy := uncovered
		legacy.ConfirmOutbound = nil
		got := s.planWithSpace(ctx, &TaskPlan{ID: "plan_legacy", Conversation: "side", State: planOpen, Tasks: []PlanTask{legacy}})
		if got.Start != nil {
			t.Fatalf("legacy plan start = %+v, want no fabricated current-hub projection", got.Start)
		}
	})

	t.Run("a settled plan has no start presentation", func(t *testing.T) {
		s, _ := trustFixture(t)
		got := s.planWithSpace(ctx, &TaskPlan{ID: "plan_started", Conversation: "side", State: planStarted, Tasks: []PlanTask{uncovered}})
		if got.Start != nil {
			t.Fatalf("settled plan start = %+v", got.Start)
		}
	})
}

// planStartInfoStore combines durable plans with the known-track and information-category fakes needed by exitUse.
type planStartInfoStore struct {
	*profileTaskStore
	tracks []db.Track
	info   *infoCategoryMemStore
}

func (st *planStartInfoStore) Tracks(context.Context) ([]db.Track, error) { return st.tracks, nil }

func (st *planStartInfoStore) AddInfoCategories(ctx context.Context, kind, ref string, cats []string, by, model string) error {
	return st.info.AddInfoCategories(ctx, kind, ref, cats, by, model)
}

func (st *planStartInfoStore) RemoveInfoCategories(ctx context.Context, kind, ref string, cats []string) error {
	return st.info.RemoveInfoCategories(ctx, kind, ref, cats)
}

func (st *planStartInfoStore) InfoCategories(ctx context.Context, kind string, refs []string) (map[string][]db.InfoCategory, error) {
	return st.info.InfoCategories(ctx, kind, refs)
}
