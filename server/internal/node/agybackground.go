package node

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/server/internal/lazyre"
	"github.com/nwverse/persona/server/internal/proto"
)

const (
	agyTaskPrefix           = "Tool is running as a background task with task id:"
	agyBackgroundEndedLimit = 20
	agyTimerExpiryGrace     = time.Minute
)

var (
	agyTaskName      = lazyre.New(`^task-[0-9]+$`)
	agyTaskEnd       = lazyre.New(`(?i)\bTask id "([^"\n]+)" (?:was )?(finished|failed|canceled|cancelled|killed|stopped)\b`)
	agyTaskMessage   = lazyre.New(`(?m)^[ \t]*\[Message\] timestamp=\S+ sender=(\S+) priority=\S+ content=([\s\S]*)`)
	agyCronIteration = lazyre.New(`\(iteration [0-9]+\)$`)
	agyTaskListRow   = lazyre.New("^- .* \\(`([^`\\s]+)`\\)$")
)

func agyBackgroundNotice(content string) string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "Created At:") {
		_, content, _ = strings.Cut(content, "\n")
	}
	if content, ok := strings.CutPrefix(strings.TrimSpace(content), agyTaskPrefix); ok {
		return content
	}
	return ""
}

func agyToolString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	// Native exports can encode a tool argument as JSON inside a string.
	var decoded string
	if strings.HasPrefix(value, `"`) && json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return value
}

type agyBackgroundTasks struct {
	running    []proto.BackgroundTask
	ended      []proto.BackgroundTask
	nextExpiry time.Time
}

func (b agyBackgroundTasks) apply(s *proto.Session) {
	s.Background = nil
	if !agyStopped(s.Status) {
		s.Background = b.running
	}
	s.SetBackgroundEnded(recentBackgroundEnded(b.ended))
}

type agyTaskRecord struct {
	task      proto.BackgroundTask
	lastSeen  time.Time
	inferred  bool
	oneShot   bool
	condition string
	deadline  time.Time
}

func (r *agyTaskRecord) finish(status string, at time.Time, inferred bool) {
	if r == nil || (r.task.Status != "" && (!r.inferred || inferred)) {
		return
	}
	r.task.Status, r.task.EndedAt, r.inferred = status, &at, inferred
}

type agyBackgroundCall struct {
	planner *agyStep
	tool    *agyToolCall
}

func agyBackground(steps []agyStep, conversation, dir string, brain *os.Root, now time.Time) agyBackgroundTasks {
	byID := make(map[string]*agyTaskRecord)
	var ordered []*agyTaskRecord
	pending := make(map[int]agyBackgroundCall)
	var inventoryTimes []time.Time
	inventorySeen := make(map[string]time.Time)
	for i := range steps {
		step := &steps[i]
		at, _ := time.Parse(time.RFC3339Nano, step.CreatedAt)
		slot := pending[step.seq]
		delete(pending, step.seq)
		switch step.Type {
		case "PLANNER_RESPONSE":
			for offset, call := range step.ToolCalls {
				// Late results retain their native step index. An overlapping claim makes the slot ambiguous.
				seq := step.seq + 1 + offset
				if _, claimed := pending[seq]; claimed {
					pending[seq] = agyBackgroundCall{}
				} else {
					pending[seq] = agyBackgroundCall{planner: step, tool: call}
				}
				if call == nil || call.Name != "manage_task" {
					continue
				}
				var args map[string]json.RawMessage
				if json.Unmarshal(call.Args, &args) == nil && strings.EqualFold(agyToolString(args["Action"]), "kill") {
					byID[agyToolString(args["TaskId"])].finish(proto.WorkflowKilled, at, false)
				}
			}
		case "SYSTEM_MESSAGE":
			end := agyTaskEnd.FindStringSubmatch(step.Content)
			if end != nil {
				status := proto.WorkflowKilled
				switch strings.ToLower(end[2]) {
				case "finished":
					status = proto.WorkflowCompleted
				case "failed":
					status = proto.WorkflowFailed
				}
				byID[end[1]].finish(status, at, false)
			}
			if m := agyTaskMessage.FindStringSubmatch(step.Content); m != nil {
				sender := agyQualifiedTaskID(conversation, m[1])
				message := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[2]), "</SYSTEM_MESSAGE>"))
				heartbeat := strings.Contains(strings.ToLower(message), "still running")
				if record := byID[sender]; record != nil {
					if at.After(record.lastSeen) {
						record.lastSeen = at
					}
					if end == nil && record.oneShot {
						if agyCronIteration.MatchString(message) {
							record.oneShot = false
						} else if !heartbeat {
							record.finish(proto.WorkflowCompleted, at, false)
						}
					}
				}
				if heartbeat {
					continue
				}
				for _, record := range ordered {
					if record.oneShot && record.task.ID != sender && record.condition != "" &&
						(record.condition == "any" || record.condition == sender) &&
						!at.IsZero() && !at.Before(record.task.StartedAt) &&
						(record.deadline.IsZero() || at.Before(record.deadline)) {
						record.finish(proto.WorkflowKilled, at, true)
					}
				}
			}
		case "GENERIC":
			var call *agyToolCall
			if slot.planner != nil && slot.planner.branch == step.branch {
				calledAt, _ := time.Parse(time.RFC3339Nano, slot.planner.CreatedAt)
				if at.IsZero() || calledAt.IsZero() || !at.Before(calledAt) {
					call = slot.tool
				}
				if ids, ok := agyTaskInventory(step, slot, calledAt, at, conversation); ok {
					inventoryTimes = append(inventoryTimes, calledAt)
					for id := range ids {
						if calledAt.After(inventorySeen[id]) {
							inventorySeen[id] = calledAt
						}
					}
				}
			}
			notice := agyBackgroundNotice(firstNonEmpty(step.backgroundStart, step.Content))
			id, details, _ := strings.Cut(notice, "\n")
			id = strings.TrimSpace(id)
			name, ours := strings.CutPrefix(id, conversation+"/")
			if !ours || !agyTaskName.MatchString(name) {
				continue
			}
			record := byID[id]
			if record == nil {
				task := proto.BackgroundTask{ID: id, Kind: "shell", StartedAt: at}
				for line := range strings.SplitSeq(details, "\n") {
					line = strings.TrimSpace(line)
					if value, ok := strings.CutPrefix(line, "Task Description:"); ok && task.Description == "" {
						task.Description = strings.Clone(strings.TrimSpace(value))
					}
					if value, ok := strings.CutPrefix(line, "Task logs are available at:"); ok && task.OutputFile == "" {
						task.OutputFile = agyTaskLog(strings.TrimSpace(value), dir, conversation, name, brain)
					}
				}
				if call != nil {
					setAgyTaskKind(&task, call.Name, call.Args)
				}
				oneShot, condition, duration := agyOneShotSchedule(task.Description, call)
				record = &agyTaskRecord{task: task, oneShot: oneShot, condition: agyQualifiedTaskID(conversation, condition)}
				if duration > 0 && !at.IsZero() {
					record.deadline = at.Add(duration)
				}
				byID[id] = record
				ordered = append(ordered, record)
			}
			if at.After(record.lastSeen) {
				record.lastSeen = at
			}
			if updated, err := time.Parse(time.RFC3339Nano, step.UpdatedAt); err == nil {
				at = updated
				if updated.After(record.lastSeen) {
					record.lastSeen = updated
				}
			}
			switch strings.ToUpper(step.Status) {
			case "DONE":
				record.finish(proto.WorkflowCompleted, at, true)
			case "ERROR":
				record.finish(proto.WorkflowFailed, at, true)
			case "CANCELED", "CANCELLED":
				record.finish(proto.WorkflowKilled, at, true)
			}
		}
	}
	slices.SortFunc(inventoryTimes, time.Time.Compare)
	var result agyBackgroundTasks
	for _, record := range ordered {
		if record.task.Status == "" && !record.task.StartedAt.IsZero() && len(inventoryTimes) > 0 {
			// Any snapshot strictly after the last evidence of this task omitted it. Wait until
			// the whole transcript is read because newer activity can arrive out of order.
			lastSeen := record.lastSeen
			if listedAt := inventorySeen[record.task.ID]; listedAt.After(lastSeen) {
				lastSeen = listedAt
			}
			if i := sort.Search(len(inventoryTimes), func(i int) bool { return inventoryTimes[i].After(lastSeen) }); i < len(inventoryTimes) {
				record.finish(proto.WorkflowCompleted, inventoryTimes[i], true)
			}
		}
		if record.task.EndedAt == nil && record.oneShot && !record.deadline.IsZero() {
			// Allow time for the timer's own completion message to reach the export.
			expiry := record.deadline.Add(agyTimerExpiryGrace)
			if !now.Before(expiry) {
				record.finish(proto.WorkflowCompleted, record.deadline, true)
			} else if result.nextExpiry.IsZero() || expiry.Before(result.nextExpiry) {
				result.nextExpiry = expiry
			}
		}
		if record.task.EndedAt != nil {
			result.ended = append(result.ended, record.task)
		} else if len(result.running) < maxRuntimeBackground {
			result.running = append(result.running, record.task)
		}
	}
	slices.SortStableFunc(result.ended, func(a, b proto.BackgroundTask) int { return a.EndedAt.Compare(*b.EndedAt) })
	if extra := len(result.ended) - agyBackgroundEndedLimit; extra > 0 {
		result.ended = slices.Clone(result.ended[extra:])
	}
	return result
}

func agyQualifiedTaskID(conversation, id string) string {
	if agyTaskName.MatchString(id) {
		return conversation + "/" + id
	}
	return id
}

// agyTaskInventory accepts complete, unfiltered list results from the current conversation.
func agyTaskInventory(step *agyStep, slot agyBackgroundCall, calledAt, at time.Time, conversation string) (map[string]struct{}, bool) {
	call := slot.tool
	if call == nil || call.Name != "manage_task" || step.Error != "" || !strings.EqualFold(step.Status, "DONE") ||
		step.Index == nil || *step.Index < 0 || slot.planner.Index == nil || *slot.planner.Index < 0 ||
		calledAt.IsZero() || at.IsZero() || at.Before(calledAt) ||
		slices.Contains(step.TruncatedFields, "content") || slices.Contains(slot.planner.TruncatedFields, "tool_calls") {
		return nil, false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Args, &args) != nil || !strings.EqualFold(agyToolString(args["Action"]), "list") {
		return nil, false
	}
	// A nonempty selector or an unknown option may make the inventory a subset.
	delete(args, "Action")
	if id := string(args["TaskId"]); id == `""` || id == "null" {
		delete(args, "TaskId")
	}
	if len(args) != 0 {
		return nil, false
	}
	// The tool's timestamp can predate the result step when an old result is exported late.
	if header, ok := strings.CutPrefix(step.Content, "Created At: "); ok {
		created, _, _ := strings.Cut(header, "\n")
		started, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(created))
		if err != nil || started.Before(calledAt) {
			return nil, false
		}
	}
	output := strings.TrimSpace(agyToolOutput(step.Content))
	if output == "No background tasks are currently running." {
		return nil, true
	}
	header, rows, _ := strings.Cut(output, "\n")
	countText, ok := strings.CutPrefix(strings.TrimSpace(header), "You have ")
	if !ok {
		return nil, false
	}
	countText, ok = strings.CutSuffix(countText, " background task(s) currently running:")
	count, err := strconv.Atoi(countText)
	if !ok || err != nil || count <= 0 {
		return nil, false
	}
	ids := make(map[string]struct{})
	for line := range strings.SplitSeq(rows, "\n") {
		match := agyTaskListRow.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			return nil, false
		}
		id := agyQualifiedTaskID(conversation, match[1])
		if name, ok := strings.CutPrefix(id, conversation+"/"); !ok || !agyTaskName.MatchString(name) {
			return nil, false
		}
		if _, duplicate := ids[id]; duplicate {
			return nil, false
		}
		ids[id] = struct{}{}
	}
	return ids, len(ids) == count
}

// agyOneShotSchedule recognizes timers from the tool arguments or task description.
// Cron takes precedence. Only the tool arguments supply a condition or duration.
func agyOneShotSchedule(description string, call *agyToolCall) (oneShot bool, condition string, duration time.Duration) {
	if strings.HasPrefix(description, "Cron:") {
		return false, "", 0
	}
	var args map[string]json.RawMessage
	if call != nil && call.Name == "schedule" && json.Unmarshal(call.Args, &args) == nil {
		if strings.TrimSpace(agyToolString(args["CronExpression"])) != "" {
			return false, "", 0
		}
		raw := args["DurationSeconds"]
		oneShot = raw != nil && string(raw) != "null"
		if oneShot || strings.HasPrefix(description, "Timer:") {
			condition = strings.TrimSpace(agyToolString(args["TimerCondition"]))
			if condition == "never" {
				condition = ""
			}
			seconds, err := strconv.ParseFloat(firstNonEmpty(agyToolString(raw), string(raw)), 64)
			if nanos := seconds * float64(time.Second); err == nil && nanos > 0 && nanos < 1<<63 {
				duration = time.Duration(nanos)
			}
			return true, condition, duration
		}
	}
	return strings.HasPrefix(description, "Timer:"), "", 0
}

func setAgyTaskKind(task *proto.BackgroundTask, tool string, raw json.RawMessage) {
	switch tool {
	case "schedule":
		task.Kind = "monitor"
	case "run_workflow":
		task.Kind = "workflow"
	case "invoke_subagent":
		task.Kind = "agent"
	case "run_command":
		var args map[string]json.RawMessage
		if json.Unmarshal(raw, &args) == nil {
			task.Command = firstNonEmpty(agyToolString(args["CommandLine"]), agyToolString(args["command"]))
		}
	}
}

// The export can name only this task's log inside its conversation directory.
func agyTaskLog(uri, dir, conversation, name string, brain *os.Root) string {
	path, err := url.PathUnescape(agyLocalFileURLToPath(uri))
	rel := filepath.Join(".system_generated", "tasks", name+".log")
	expected, absErr := filepath.Abs(filepath.Join(dir, "brain", conversation, rel))
	// The log can be announced before it is created.
	if err != nil || absErr != nil || !filepath.IsAbs(path) || filepath.Clean(path) != expected {
		return ""
	}
	info, err := brain.Stat(rel)
	if os.IsNotExist(err) || (err == nil && info.Mode().IsRegular()) {
		return expected
	}
	return ""
}

func agyStopped(status string) bool {
	return status == proto.StatusOffline || status == proto.StatusInterrupted
}

// agyPaneBackgroundFromRows reads the tasks the companion hides on ACP history rows.
func agyPaneBackgroundFromRows(params json.RawMessage) map[string][]proto.BackgroundTask {
	var push struct {
		Sessions []struct {
			ID    string                 `json:"session_id"`
			Tasks []proto.BackgroundTask `json:"agy_pane_background"`
		} `json:"sessions"`
	}
	if json.Unmarshal(params, &push) != nil {
		return nil
	}
	var background map[string][]proto.BackgroundTask
	for _, row := range push.Sessions {
		if len(row.Tasks) == 0 || !runtimeIDRe.MatchString(row.ID) {
			continue
		}
		if background == nil {
			background = make(map[string][]proto.BackgroundTask)
		}
		background[row.ID] = runtimeBackground(row.Tasks, maxRuntimeBackground, true)
	}
	return background
}

// agyPaneRuntimeRow reads the companion row and its pane inventory from the same list update.
func (b *runtimeRegistry) agyPaneRuntimeRow(id string) (proto.Session, bool) {
	h := b.ownerOf(id)
	if h == nil {
		return proto.Session{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hello.Runtime != agyRuntimeID || h.daemon != "" {
		return proto.Session{}, false
	}
	row, ok := h.rowLocked(id)
	if tasks := h.agyPaneBackground[id]; len(row.Background) == 0 && len(tasks) > 0 {
		row.Background = tasks
	}
	return row, ok
}

// Runtime task metadata cannot grant access to node-local files. Join the log
// paths independently confirmed by the local conversation's transcript.
func (n *Node) agyRuntimeTaskLogs(s *proto.Session, conversation string) {
	if len(s.Background) == 0 && len(s.BackgroundEnded()) == 0 {
		return
	}
	meta, ok := n.agyTranscriptMeta(conversation)
	if !ok {
		return
	}
	logs := make(map[string]string)
	for _, tasks := range [][]proto.BackgroundTask{meta.background.running, meta.background.ended} {
		for _, task := range tasks {
			if task.OutputFile != "" {
				logs[task.ID] = task.OutputFile
			}
		}
	}
	if len(logs) == 0 {
		return
	}
	attach := func(tasks []proto.BackgroundTask) []proto.BackgroundTask {
		tasks = slices.Clone(tasks)
		for i := range tasks {
			tasks[i].OutputFile = logs[tasks[i].ID]
		}
		return tasks
	}
	s.Background = attach(s.Background)
	s.SetBackgroundEnded(attach(s.BackgroundEnded()))
}
