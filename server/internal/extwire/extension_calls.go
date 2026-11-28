package extwire

import (
	"encoding/json"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/buswire"
)

// These methods run in the extension process.
const (
	MethodAuthRelayComplete = "auth_relay.complete"
	MethodSecretsDescribe   = "secrets.describe"
	MethodSecretsResolve    = "secrets.resolve"
	MethodSecretsStore      = "secrets.store"
	MethodUIImage           = "ui.image"
	MethodSettingsCheck     = "settings.check"
	MethodShortcutRun       = "shortcut.run"
	MethodOffloadCancel     = "offload.cancel"
	MethodMemoryEntries     = "memory.entries"
	MethodAuthStatus        = "auth.status"
	MethodAuthLogout        = "auth.logout"

	MethodUIPageDispose        = "ui.page.dispose"
	MethodUIPageSuspend        = "ui.page.suspend"
	MethodUIPageSupported      = "ui.page.supported"
	MethodAssistantCredentials = "assistant.credentials"
	MethodAssistantSummary     = "assistant.summary"
)

var (
	AuthRelayCompleteMethod = buswire.Method[wire.AuthRelayCompleteParams, wire.AuthRelayResult]{Name: MethodAuthRelayComplete}
	SecretsDescribeMethod   = buswire.Method[wire.SecretsDescribeParams, wire.SecretsDescribeResult]{Name: MethodSecretsDescribe}
	SecretsResolveMethod    = buswire.Method[wire.SecretsResolveParams, wire.SecretsResolveResult]{Name: MethodSecretsResolve}
	SecretStoreMethod       = buswire.Method[wire.SecretStoreParams, wire.SecretStoreResult]{Name: MethodSecretsStore}
	UIImageMethod           = buswire.Method[wire.UIImageParams, wire.UIImage]{Name: MethodUIImage}
	SettingsCheckMethod     = buswire.Method[wire.SettingsCheckParams, wire.SettingsCheckResult]{Name: MethodSettingsCheck}
	ShortcutRunMethod       = buswire.Method[wire.ShortcutRunParams, wire.ShortcutResult]{Name: MethodShortcutRun}
	OffloadCancelMethod     = buswire.Method[wire.OffloadJobParams, buswire.None]{Name: MethodOffloadCancel}
	// The caller checks the raw page size and shape before decoding entries.
	MemoryEntriesMethod = buswire.Method[wire.MemoryEntriesParams, json.RawMessage]{Name: MethodMemoryEntries}

	AuthStatusMethod = buswire.Method[buswire.None, wire.AuthStatus]{Name: MethodAuthStatus}
	AuthLogoutMethod = buswire.Method[buswire.None, wire.AuthStatus]{Name: MethodAuthLogout}

	UIPageDisposeMethod   = buswire.Method[UIPageSessionParams, buswire.None]{Name: MethodUIPageDispose}
	UIPageSuspendMethod   = buswire.Method[UIPageSessionParams, buswire.None]{Name: MethodUIPageSuspend}
	UIPageSupportedMethod = buswire.Method[buswire.None, UIPageSupportedResult]{Name: MethodUIPageSupported}

	AssistantCredentialsMethod = buswire.Method[AssistantCredentialsParams, AssistantCredentialsResult]{Name: MethodAssistantCredentials}
	AssistantSummaryMethod     = buswire.Method[AssistantSummaryParams, AssistantSummaryResult]{Name: MethodAssistantSummary}
)

// UIPageSessionParams identifies the mounted page to suspend or dispose.
type UIPageSessionParams struct {
	Session string `json:"session"`
}

// UIPageSupportedResult lists the contributions that support component pages.
type UIPageSupportedResult struct {
	Contributions []string `json:"contributions"`
}

// AssistantCredentialsParams optionally targets the provider for a command gate.
type AssistantCredentialsParams struct {
	Contribution string `json:"contribution,omitempty"`
}

// AssistantCredentialsResult carries the gate's environment variables.
type AssistantCredentialsResult struct {
	Env map[string]string `json:"env"`
}

// AssistantSummaryParams carries the text to rewrite and the optional target.
type AssistantSummaryParams struct {
	Contribution string `json:"contribution,omitempty"`
	Summary      string `json:"summary"`
}

// AssistantSummaryResult carries the rewritten text.
type AssistantSummaryResult struct {
	Summary string `json:"summary"`
}
