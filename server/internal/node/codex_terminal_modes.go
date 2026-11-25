package node

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/server/internal/host"
	"github.com/nwverse/persona/server/internal/i18n"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
	"github.com/nwverse/persona/server/internal/shellcmd"
	"github.com/nwverse/persona/server/internal/transcript"
)

// Model and thinking effort of a Codex on the terminal route, from the phone (#12572,
// docs/integrations/codex.md "On the terminal route"). A Codex in a herdr or Orca pane that Persona does not drive
// through an app-server (it runs in its own process, or is not joined on one yet; codex_effort.go is the other route)
// gets its messages typed into its box, and now its settings too. Since 0.169, Codex's own /model takes the choice as
// arguments:
//
//	/model <model> --reasoning=<level>    /model --reasoning=<level>    /model <model>
//
// Codex applies it to this session only (its config.toml stays as it is), mid-turn too, from the next turn. The node
// types that line the way a person would, after the checks a message gets (a draft in the box or a dialog on the screen
// refuses, nothing typed), and counts the change as made only when the rollout records it: Codex writes
// thread_settings_applied with the thread's model and effort as soon as it applies them. The screen is read only for
// Codex's refusal, which is passed on in Codex's own words. With neither within codexSettingsWait, the change is
// reported as unconfirmed, and it is never typed again.

// codexModelArgsMinVersion is the first Codex whose /model takes the choice as arguments instead of opening its picker.
var codexModelArgsMinVersion = [3]int{0, 169, 0}

var (
	// codexSettingsWait bounds how long the rollout has to record a typed /model; codexSettingsLook is the pause between
	// two looks at the rollout and the screen. Tests shorten both.
	codexSettingsWait = 15 * time.Second
	codexSettingsLook = 250 * time.Millisecond
	// codexStartingRetry is the wait before the one retry a Codex that says it is still starting gets.
	codexStartingRetry = 2 * time.Second
)

// codexSettingsMost is how much of the rollout written after the typing is read for the change's record.
const codexSettingsMost = 8 << 20

// codexTerminal is a Codex on the terminal route whose /model takes arguments: the pane to type into, its rollout (where
// Codex records the change) and the Codex home the rollout is in (its models_cache.json says the efforts of each model).
type codexTerminal struct {
	pane, rollout, home string
}

type codexTerminalSettings struct {
	model, effort, tier string
}

// codexOnTerminal says whether session id is a live Codex on the terminal route: in a pane that takes keys (botmux's
// takes whole messages), not joined on an app-server (codex_effort.go drives that one), not a subagent (its parent drives
// it). Then t is where to type /model, or why is the coded refusal of a session whose change could not be made or
// confirmed that way: an older Codex started it, or it has no rollout yet.
func (n *Node) codexOnTerminal(id string) (t codexTerminal, why error, ok bool) {
	n.mu.Lock()
	a, live := n.agents[id]
	n.mu.Unlock()
	if !live || a.Agent != "codex" || a.PaneID == "" || host.ParsePane(a.PaneID).Kind == host.Botmux {
		return codexTerminal{}, nil, false
	}
	_, thread, err := n.codexOwner(id)
	switch {
	case err == nil && (thread.joined || thread.parent != ""):
		return codexTerminal{}, nil, false // driven through its app-server, or a subagent
	case err != nil && !errors.Is(err, errCodexNoOwner):
		return codexTerminal{}, nil, false // which session this is, or what runs it, is in doubt
	}
	path := n.codexPath(id)
	if path == "" {
		return codexTerminal{}, refusal(proto.CodeModesCodexNoRollout, i18n.T(i18n.Default, "rpc-errors.modes_codex_no_rollout"), nil), true
	}
	if !versionAtLeast(codexSessionVersion(path), codexModelArgsMinVersion) {
		return codexTerminal{}, refusal(proto.CodeModesCodexVersion, i18n.T(i18n.Default, "rpc-errors.modes_codex_version"), nil), true
	}
	return codexTerminal{pane: a.PaneID, rollout: path, home: codexHomeOf(path)}, nil, true
}

// codexTerminalRow projects the current settings and the choices supported by the terminal thread's model.
func (n *Node) codexTerminalRow(id string, s *proto.Session, m quickMeta) {
	t, why, ok := n.codexOnTerminal(id)
	if !ok || why != nil || s.Capabilities == nil {
		return
	}
	s.Capabilities.Model = proto.Bool(true)
	settings := n.codexTerminalSettingsOf(id, t, m)
	s.Model, s.Effort, s.ServiceTier = cmp.Or(settings.model, s.Model), settings.effort, settings.tier
	s.Efforts = codexEffortLevels
	if e, listed := codexModelEffortsIn(t.home, s.Model); listed {
		s.Efforts = e.levels
		s.ServiceTiers = codexSpeedChoices(e.tiers)
	}
	if len(s.Efforts) == 0 {
		s.Effort = ""
	}
}

// codexTerminalSettingsOf uses native settings as soon as the rollout records a model. A rollout with only startup
// metadata has no turn settings yet, so read the launcher's options in the home identified by that rollout.
func (n *Node) codexTerminalSettingsOf(id string, t codexTerminal, m quickMeta) codexTerminalSettings {
	settings := codexTerminalSettings{model: m.model, effort: m.effort, tier: m.serviceTier}
	if settings.model != "" {
		return settings
	}
	st, _ := n.state.get(id)
	settings.tier = cmp.Or(settings.tier, st.CodexStartTier)
	a, _ := n.agent(id)
	argv := a.ForegroundArgv
	var launcherModel string
	if len(argv) == 0 || progName(argv[0]) != "codex" {
		if launcher, ok := n.managedLauncher(st.Launcher); ok && launcher.AgentType() == "codex" {
			argv = append([]string{"codex"}, launcher.Args...)
			launcherModel = launcher.Model
		} else {
			argv, _ = n.codexArgv(st.Launcher)
			for len(argv) > 0 && shellcmd.AssignRe.MatchString(argv[0]) {
				argv = argv[1:]
			}
		}
	}
	settings.model = cmp.Or(st.Model, modelArg(argv), launcherModel)
	settings.effort = cmp.Or(settings.effort, st.ModelEffort)
	if t.home == "" {
		return settings
	}
	cfg, err := codexSourceConfig(argv, []string{"CODEX_HOME=" + t.home}, cmp.Or(st.Cwd, a.Cwd, m.start), n.home)
	if err != nil {
		return settings
	}
	model, _ := cfg["model"].(string)
	effort, _ := cfg["model_reasoning_effort"].(string)
	tier, _ := cfg["service_tier"].(string)
	settings.model = cmp.Or(settings.model, model)
	settings.effort = cmp.Or(settings.effort, effort)
	settings.tier = cmp.Or(settings.tier, tier, "default")
	if settings.tier == "fast" {
		settings.tier = "priority"
	}
	if settings.model == "" {
		for _, entry := range readCodexHomeModels(t.home) {
			if entry.Default {
				settings.model = entry.ID
				break
			}
		}
	}
	if settings.effort == "" {
		if entry, listed := codexModelEffortsIn(t.home, settings.model); listed {
			settings.effort = entry.def
		}
	}
	return settings
}

// setCodexTerminalSettings switches the Codex in t to model and / or effort ("" leaves one as it is) by typing Codex's
// own /model into its box.
func (n *Node) setCodexTerminalSettings(ctx context.Context, id string, t codexTerminal, model, effort string) error {
	model, effort = strings.TrimSpace(model), strings.TrimSpace(effort)
	if model == "" && effort == "" {
		return nil
	}
	if model != "" && !proto.ValidModelID(model) {
		return fmt.Errorf("invalid model %q", model)
	}
	n.forgetMeta(n.transcriptPath(id)) // a change made at the computer a moment ago counts
	now := n.codexTerminalSettingsOf(id, t, n.metaOf(id))
	target, listed := codexModelEffortsIn(t.home, cmp.Or(model, now.model))
	switch {
	case effort != "":
		levels := codexEffortLevels // a model the cache does not name takes any of Codex's levels, as on the app-server
		if listed {
			levels = target.levels
		}
		if err := codexEffortRefusal(levels, effort); err != nil {
			return err
		}
	case listed && slices.Contains(target.levels, now.effort):
		// A model alone: Codex would put the new model on that model's default level. The app-server route keeps the
		// thread's effort when the new model takes it, and so does this one.
		effort = now.effort
	}
	if (model == "" || model == now.model) && (effort == "" || effort == now.effort) {
		return nil // what it runs with already
	}
	cmd, err := codexModelArgsCommand(model, effort)
	if err != nil {
		return err
	}
	return n.applyCodexTerminalSettings(ctx, id, t, cmd, codexTerminalSettings{model: model, effort: effort})
}

// applyCodexTerminalSettings retries only Codex's explicit temporary startup refusal.
func (n *Node) applyCodexTerminalSettings(ctx context.Context, id string, t codexTerminal, cmd string, target codexTerminalSettings) error {
	for try := 0; ; try++ {
		err := n.typeCodexSettings(ctx, id, t, cmd, target)
		var starting codexStarting
		if !errors.As(err, &starting) {
			if err == nil {
				n.forgetMeta(n.transcriptPath(id)) // the row shows what Codex just recorded, not a read from before it
				n.notifySessions()
			}
			return err
		}
		if try > 0 {
			return refusal(proto.CodeModesCodexRefused, starting.words, nil)
		}
		select { // nothing was applied: typed once more after a moment
		case <-ctx.Done():
			return refusal(proto.CodeModesCodexRefused, starting.words, nil)
		case <-time.After(codexStartingRetry):
		}
	}
}

// typeCodexSettings types cmd into the Codex in t after the checks a message gets, then waits for the outcome: nil once
// the rollout records the requested settings, Codex's refusal once the screen shows one, else,
// after codexSettingsWait, the unconfirmed error. The session's typing lock is held only while typing.
func (n *Node) typeCodexSettings(ctx context.Context, id string, t codexTerminal, cmd string, target codexTerminalSettings) error {
	unlock := n.typing.lock(id) // one at a time with the messages: a send in flight types into the same box
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()
	if err := n.checkPane(ctx, t.pane, "codex", ""); err != nil {
		return err // a draft in the box, a dialog on the screen, the agent gone: nothing typed
	}
	before, _ := n.readPane(ctx, t.pane, "visible", 60)
	var from int64 // where the rollout ends now: only what Codex writes after it is this command's record
	if st, err := os.Stat(t.rollout); err == nil {
		from = st.Size()
	}
	if err := n.prompt(ctx, t.pane, cmd); err != nil {
		if r := deliveryRefusal(err); r != nil {
			return r
		}
		return uncertain(err)
	}
	if err := n.codexSubmitted(ctx, t.pane); err != nil {
		return err // the command may still be in the box: never typed again
	}
	n.typing.typed(t.pane, cmd)
	release() // typed: a message may follow now
	for deadline := time.Now().Add(codexSettingsWait); ; {
		if codexSettingsRecorded(t.rollout, from, target) {
			return nil
		}
		if after, err := n.readPane(ctx, t.pane, "visible", 60); err == nil {
			if err := codexTerminalOutcome(before, after, target); err != nil {
				if codexSettingsRecorded(t.rollout, from, target) {
					return nil // recorded meanwhile: the record, not a line on the screen, says the change took
				}
				return err
			}
		}
		if !time.Now().Before(deadline) {
			return codexSettingsUnconfirmed()
		}
		select {
		case <-ctx.Done():
			return codexSettingsUnconfirmed()
		case <-time.After(codexSettingsLook):
		}
	}
}

// forgetMeta has the next look at the transcript at path read what was written since the last read, even within
// metaMinInterval.
func (n *Node) forgetMeta(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.metas[path]; ok {
		m.readAt = time.Time{}
		n.metas[path] = m
	}
}

// codexModelArgsCommand is the /model line that switches a Codex to model and / or effort ("" leaves either out): a
// model id held to the rule every model is (proto.ValidModelID: one word, no leading dash) and a level that is one
// lower-case word (whether the model takes it is checked before, codexEffortRefusal). Both empty is no command: a bare
// /model opens Codex's picker.
func codexModelArgsCommand(model, effort string) (string, error) {
	if model == "" && effort == "" {
		return "", errors.New("no model or effort to switch to")
	}
	cmd := "/model"
	if model != "" {
		if !proto.ValidModelID(model) {
			return "", fmt.Errorf("invalid model %q", model)
		}
		cmd += " " + model
	}
	if effort != "" {
		if strings.Trim(effort, "abcdefghijklmnopqrstuvwxyz") != "" {
			return "", fmt.Errorf("invalid effort %q", effort)
		}
		cmd += " --reasoning=" + effort
	}
	return cmd, nil
}

// codexStarting is Codex's answer to a /model typed while its session is still starting: tried once more.
type codexStarting struct{ words string }

func (e codexStarting) Error() string { return e.words }

// codexTerminalOutcome is Codex's refusal of the /model typed between the screens before and after: a new line of
// after that starts with Codex's words that the model or level typed is unavailable, that the command takes no
// arguments now (Codex's Reserve) or an argument it does not know, that /model waits for the running task, or that the
// session is still starting (codexStarting). Its words are Codex's (Codex 0.169 to 0.171). nil when none shows: the
// rollout says whether the change took.
//
// A line is new when after has more copies of it than before: Codex prints the same refusal again for the same pick,
// below the first one still on the screen. The words count only at the start of a line, so a tool's output or the
// agent's text that quotes them is not taken for Codex's answer.
func codexTerminalOutcome(before, after string, target codexTerminalSettings) error {
	onScreen := map[string]int{}
	for _, l := range strings.Split(before, "\n") {
		onScreen[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(after, "\n") {
		line := strings.TrimSpace(l)
		if line == "" {
			continue
		}
		if onScreen[line] > 0 { // an earlier answer still on the screen is not this command's
			onScreen[line]--
			continue
		}
		words := strings.TrimSpace(strings.TrimLeft(line, "■•└ "))
		switch {
		case strings.HasPrefix(words, "Session is still starting; try /model"):
			return codexStarting{words}
		case target.model != "" && (strings.HasPrefix(words, "Model '"+target.model+"' is unavailable") ||
			strings.HasPrefix(words, "Your ChatGPT model list doesn't include `"+target.model+"`")),
			target.effort != "" && strings.HasPrefix(words, "Reasoning level '"+target.effort+"' is unavailable"),
			target.tier != "" && strings.HasPrefix(words, "Speed '") && strings.Contains(words, "' is unavailable"),
			strings.HasPrefix(words, "Model arguments are unavailable"),
			strings.HasPrefix(words, "Unexpected argument '"),
			strings.HasPrefix(words, "'/model' is disabled while a task is in progress"):
			return refusal(proto.CodeModesCodexRefused, words, nil)
		}
	}
	return nil
}

// codexSettingsRecorded says whether the rollout at path has, past offset from, Codex's record of the requested
// settings (empty fields are ignored): the thread_settings_applied it writes as soon as it applies a /model, and
// again when a turn starts.
func codexSettingsRecorded(path string, from int64, target codexTerminalSettings) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return false
	}
	sc := bufio.NewScanner(io.LimitReader(f, codexSettingsMost))
	sc.Buffer(make([]byte, 64<<10), 8<<20) // the line carries the thread's instructions
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"thread_settings_applied"`)) {
			continue
		}
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				Type     string                          `json:"type"`
				Settings *transcript.CodexThreadSettings `json:"thread_settings"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "event_msg" || l.Payload.Type != "thread_settings_applied" || l.Payload.Settings == nil {
			continue
		}
		s := l.Payload.Settings
		if (target.model == "" || s.Model == target.model) &&
			(target.effort == "" || s.ReasoningEffort != nil && *s.ReasoningEffort == target.effort) &&
			(target.tier == "" || codexServiceTier(s.ServiceTier) == target.tier) {
			return true
		}
	}
	return false
}

// codexSettingsUnconfirmed is the answer when the command went in and the rollout did not show the change in time: it
// may yet take, so it is not called a failure, and nothing is typed again.
func codexSettingsUnconfirmed() error {
	data, _ := json.Marshal(map[string]any{"delivered": deliveredUnknown})
	return &rpc.Error{Code: proto.CodeModesCodexUnconfirmed, Message: i18n.T(i18n.Default, "rpc-errors.modes_codex_unconfirmed"), Data: data}
}

// codexHomeOf is the Codex home a rollout is in (<home>/sessions/YYYY/MM/DD/rollout-….jsonl), "" for one elsewhere.
func codexHomeOf(rollout string) string {
	sessions := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(rollout))))
	if filepath.Base(sessions) != "sessions" {
		return ""
	}
	return filepath.Dir(sessions)
}

// codexSessionVersions caches the Codex version each rollout's session_meta names (codexRolloutVersion): it never
// changes, and the session list asks for it on every refresh of a terminal-route Codex row.
var codexSessionVersions = struct {
	sync.Mutex
	byPath map[string]string
}{byPath: map[string]string{}}

// codexSessionVersion is the Codex version that started the session whose rollout is path, "" when it names none.
func codexSessionVersion(path string) string {
	codexSessionVersions.Lock()
	v, ok := codexSessionVersions.byPath[path]
	codexSessionVersions.Unlock()
	if ok {
		return v
	}
	if v = codexRolloutVersion(path); v != "" { // none yet is not kept: a new rollout may not have its session_meta yet
		codexSessionVersions.Lock()
		codexSessionVersions.byPath[path] = v
		codexSessionVersions.Unlock()
	}
	return v
}

// codexHomeEfforts caches what each Codex home's models_cache.json says about its models' efforts, read again when the
// file changes: the session list asks on every refresh of a terminal-route Codex row.
var codexHomeEfforts = struct {
	sync.Mutex
	byHome map[string]codexHomeEffortsEntry
}{byHome: map[string]codexHomeEffortsEntry{}}

type codexHomeEffortsEntry struct {
	mod    time.Time
	size   int64
	models map[string]codexModelEfforts
}

// codexModelEffortsIn is what home's models_cache.json says about model's efforts (hidden models too); listed is false
// when it names no such model, or home has no cache to read.
func codexModelEffortsIn(home, model string) (e codexModelEfforts, listed bool) {
	if home == "" || model == "" {
		return codexModelEfforts{}, false
	}
	st, err := os.Stat(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return codexModelEfforts{}, false
	}
	codexHomeEfforts.Lock()
	defer codexHomeEfforts.Unlock()
	cached, ok := codexHomeEfforts.byHome[home]
	if !ok || !cached.mod.Equal(st.ModTime()) || cached.size != st.Size() {
		models, _ := readCodexHomeModelEfforts(home)
		cached = codexHomeEffortsEntry{mod: st.ModTime(), size: st.Size(), models: models}
		codexHomeEfforts.byHome[home] = cached
	}
	e, listed = cached.models[model]
	return e, listed
}
