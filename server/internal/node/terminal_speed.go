package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/server/internal/host"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
	"github.com/nwverse/persona/server/internal/screen"
	"github.com/nwverse/persona/server/internal/transcript"
)

var terminalCodexCatalogs = struct {
	sync.Mutex
	homes map[string]terminalCodexCatalog
}{homes: make(map[string]terminalCodexCatalog)}

type terminalCodexCatalog struct {
	info   os.FileInfo
	models map[string]codexModelEfforts
}

func terminalCodexTiers(home, model string) []proto.ServiceTier {
	if model == "" {
		return nil
	}
	st, err := os.Stat(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return nil
	}
	terminalCodexCatalogs.Lock()
	defer terminalCodexCatalogs.Unlock()
	entry := terminalCodexCatalogs.homes[home]
	if entry.info == nil || st.Size() != entry.info.Size() || !st.ModTime().Equal(entry.info.ModTime()) || !os.SameFile(st, entry.info) {
		models, _ := readCodexHomeModelEfforts(home)
		entry = terminalCodexCatalog{info: st, models: models}
		if len(terminalCodexCatalogs.homes) >= 64 {
			clear(terminalCodexCatalogs.homes)
		}
		terminalCodexCatalogs.homes[home] = entry
	}
	return codexSpeedChoices(entry.models[model].tiers)
}

func claudeFastModel(model, version string, env procEnv) bool {
	model, _, _ = strings.Cut(model, "[")
	if i := strings.LastIndexAny(model, "-@"); i >= 0 && len(model[i+1:]) == 8 &&
		strings.IndexFunc(model[i+1:], func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		model = model[:i]
	}
	if env.FastUnavailable {
		return false
	}
	switch model {
	case "claude-fable-5-2":
		return versionAtLeast(version, [3]int{2, 1, 321})
	case "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8":
		// On Vertex, /fast can switch an Opus session to Fable.
		return !env.Vertex && versionAtLeast(version, [3]int{2, 1, 205})
	default:
		return false
	}
}

func (n *Node) terminalSpeedChoices(id, kind, pane string, m quickMeta) []proto.ServiceTier {
	if pane == "" || host.ParsePane(pane).Kind == host.Botmux || n.codexBlocked(id) || len(n.alsoOpenIn(id)) != 0 {
		return nil
	}
	h, _ := n.hosts.Pane(pane)
	if h == nil || !drivesScreen(h) {
		return nil
	}
	switch kind {
	case "codex":
		_, thread, err := n.codexOwner(id)
		if err != nil && !errors.Is(err, errCodexNoOwner) || thread.joined || thread.parent != "" || !m.codex ||
			!versionAtLeast(m.version, [3]int{0, 169, 0}) {
			return nil
		}
		model := m.model
		if m.speed.Model != "" && !m.speed.At.Before(m.modelAt) {
			model = m.speed.Model
		}
		home := n.codexPaneHome(pane)
		if source, ok := n.codexSource(id); ok {
			home = source.home
		}
		return terminalCodexTiers(home, model)
	case "claude":
		env, _ := n.sessionEnv(id)
		if m.surface != transcript.SurfaceDesktop && claudeFastModel(n.shownModel(id, m.model), m.version, env) {
			return []proto.ServiceTier{{ID: "default", Name: "Standard"}, {ID: "priority", Name: "Fast"}}
		}
	}
	return nil
}

func (n *Node) terminalSpeedRow(s *proto.Session, m quickMeta) {
	if s.Status == proto.StatusOffline || s.Status == proto.StatusInterrupted || s.Starting ||
		s.IsSubagent || s.Assistant || s.Capabilities != nil && !s.Capabilities.Send {
		return
	}
	if tiers := n.terminalSpeedChoices(s.SessionID, s.Agent, s.PaneID, m); len(tiers) > 0 {
		s.ServiceTiers, s.ServiceTier = tiers, m.speed.Tier
	}
}

func terminalSpeedCommand(kind, tier string) string {
	if kind == "claude" {
		if tier == "priority" {
			return "/fast on"
		}
		return "/fast off"
	}
	switch tier {
	case "default":
		tier = "standard"
	case "priority":
		tier = "fast"
	}
	return "/model --speed=" + tier
}

func speedUnsupported() error {
	return refusal("session_speed_unsupported", "This session does not offer the selected speed.", nil)
}

func (n *Node) setTerminalSpeed(ctx context.Context, id, tier string) error {
	a, live := n.agent(id)
	n.mu.Lock()
	parked := n.onAgentsList[id]
	n.mu.Unlock()
	if parked || !live || n.isAssistantSession(id) || a.AgentStatus == proto.StatusOffline || a.AgentStatus == proto.StatusInterrupted {
		return speedUnsupported()
	}
	path := n.transcriptPath(id)
	st, err := os.Stat(path)
	if err != nil {
		return speedUnsupported()
	}
	m := n.metaNow(path, st)
	if !slices.ContainsFunc(n.terminalSpeedChoices(id, a.Agent, a.PaneID, m), func(choice proto.ServiceTier) bool {
		return choice.ID == tier
	}) {
		return speedUnsupported()
	}
	pane := a.PaneID
	if n.sessionOfPane(pane) != id {
		return speedUnsupported()
	}
	if a.Agent == "codex" {
		if err := n.codexReady(ctx, pane, true); err != nil {
			return err
		}
	} else {
		if err := n.checkPaneFor(ctx, pane, "claude", sendOpts{readBox: true}); err != nil {
			return err
		}
		if n.plainScreen(pane) {
			draft, err := n.promptHasDraft(ctx, pane)
			if err != nil {
				return err
			}
			if draft {
				return refusal(screen.InputDraft, "The terminal input box is not empty.", nil)
			}
		}
	}
	before, err := n.readPane(ctx, pane, "visible", 60)
	if err != nil {
		return err
	}
	cmd := terminalSpeedCommand(a.Agent, tier)
	started := time.Now()
	if err := n.prompt(ctx, pane, cmd); err != nil {
		if r := deliveryRefusal(err); r != nil {
			return r
		}
		return uncertain(err)
	}
	n.typing.typed(pane, cmd)
	wait, cancel := context.WithTimeout(ctx, codexEffortTimeout)
	defer cancel()
	tick := time.NewTicker(modelLook)
	defer tick.Stop()
	for {
		if now, err := os.Stat(path); err == nil && (now.Size() != st.Size() || !now.ModTime().Equal(st.ModTime())) {
			st = now
			if terminalSpeedRecorded(path, st.Size(), tier, started) {
				n.metaNow(path, st)
				n.notifySessionsSoon()
				return nil
			}
		}
		if shown, err := n.readPane(wait, pane, "visible", 60); err == nil {
			if reason := terminalSpeedRefusal(before, shown, cmd); reason != "" {
				return refusal("session_speed_refused", reason, nil)
			}
		}
		select {
		case <-wait.Done():
			return &rpc.Error{Code: "session_speed_unconfirmed", Message: "The terminal did not confirm the speed change. Check the terminal before trying again.", Data: json.RawMessage(`{"delivered":"unknown"}`)}
		case <-tick.C:
		}
	}
}

// Only a native settings acknowledgment after typing counts. Read a bounded suffix because Claude may rewrite its
// trailing metadata when it appends the command, and Codex may write a turn context at the old speed.
func terminalSpeedRecorded(path string, size int64, tier string, started time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	start := max(size-modelRecordMost, 0)
	r := bufio.NewReader(io.NewSectionReader(f, start, size-start))
	if start > 0 {
		_, _ = r.ReadBytes('\n') // partial first record
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return false
		}
		s := transcript.SessionSpeed(line)
		if s.Applied && s.Tier == tier && !s.At.Before(started.Truncate(time.Millisecond)) {
			return true
		}
	}
}

func terminalSpeedRefusal(before, shown, command string) string {
	lines := func(s string) []string {
		return strings.Split(screen.Plain(s), "\n")
	}
	old := make(map[string]int)
	clean := func(s string) string { return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "■•⎿")) }
	for _, line := range lines(before) {
		old[clean(line)]++
	}
	for _, line := range lines(shown) {
		line = clean(line)
		if old[line] > 0 {
			old[line]--
			continue
		}
		switch {
		case strings.HasPrefix(command, "/model "):
			if strings.HasPrefix(line, "Speed '") && strings.Contains(line, "is unavailable") ||
				strings.HasPrefix(line, "Model arguments are unavailable") ||
				strings.HasPrefix(line, "Session is still starting; try /model") ||
				strings.HasPrefix(line, "Unexpected argument '--speed=") {
				return line
			}
		case strings.HasPrefix(line, "Fast mode "):
			if strings.HasPrefix(line, "Fast mode is not available") || strings.HasPrefix(line, "Fast mode has been disabled") ||
				strings.HasPrefix(line, "Fast mode requires") || strings.HasPrefix(line, "Fast mode unavailable") ||
				strings.HasPrefix(line, "Fast mode needs") {
				return line
			}
		}
	}
	return ""
}
