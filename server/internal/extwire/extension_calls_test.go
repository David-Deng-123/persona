package extwire_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/buswire"
	"github.com/nwverse/persona/server/internal/extwire"
)

func TestExtensionCallsWire(t *testing.T) {
	checkMethodWire[wire.AuthRelayCompleteParams, wire.AuthRelayResult](t, extwire.AuthRelayCompleteMethod, "auth_relay.complete",
		`{"contribution":"relay","url":"https://example.com/authorize"}`,
		`{"status":"manual","app":"Example","reason":"not_signed_in"}`)
	checkMethodWire[wire.SecretsDescribeParams, wire.SecretsDescribeResult](t, extwire.SecretsDescribeMethod, "secrets.describe",
		`{"contribution":"provider","reference":"op://vault/item/field"}`,
		`{"title":"Example item","detail":"Example field"}`)
	checkMethodWire[wire.SecretsResolveParams, wire.SecretsResolveResult](t, extwire.SecretsResolveMethod, "secrets.resolve",
		`{"contribution":"provider","reference":"op://vault/item/field","purpose":{"kind":"agent_run","id":"EXAMPLE_VALUE","machine":"example-machine","session":"example-session"}}`,
		`{"value":"example-value"}`)
	checkMethodWire[wire.SecretStoreParams, wire.SecretStoreResult](t, extwire.SecretStoreMethod, "secrets.store",
		`{"contribution":"provider","phase":"commit","target":{"operation":"create","vault":"vault","title":"Example item","fields":[{"name":"field","type":"STRING"}]},"machine":"example-machine","session":"example-session","key_id":"example-key","sealed":"AAE="}`,
		`{"target":{"operation":"create","vault":"vault","title":"Example item","fields":[{"name":"field","type":"STRING"}]},"vault_name":"Example vault","references":{"field":"op://vault/item/field"}}`)
	checkMethodWire[wire.UIImageParams, wire.UIImage](t, extwire.UIImageMethod, "ui.image",
		`{"contribution":"camera","ref":"snapshot","width":640,"placement":"nav_page"}`,
		`{"mime":"image/png","data":"AAE=","max_age":10}`)
	checkMethodWire[wire.SettingsCheckParams, wire.SettingsCheckResult](t, extwire.SettingsCheckMethod, "settings.check",
		`{"config":{"endpoint":"https://example.com"},"secrets":{"example":"EXAMPLE_VALUE"},"proxy":"http://example.com:8080"}`,
		`{"ok":false,"error":"Please check the endpoint"}`)
	checkMethodWire[wire.ShortcutRunParams, wire.ShortcutResult](t, extwire.ShortcutRunMethod, "shortcut.run",
		`{"contribution":"ping","values":{},"ui_version":7,"tz":"Asia/Shanghai","locale":"en","platform":"ios"}`,
		`{"text":"Example result","view":{"title":"Example view","sections":[]}}`)
	checkMethodWire[wire.OffloadJobParams, buswire.None](t, extwire.OffloadCancelMethod, "offload.cancel",
		`{"contribution":"archive","locale":"en","machine":{"id":"example-machine","name":"Example computer","os":"linux","online":true},"job":"job-1"}`,
		`{}`)
	checkMethodWire[wire.MemoryEntriesParams, json.RawMessage](t, extwire.MemoryEntriesMethod, "memory.entries",
		`{"contribution":"library","limit":200,"after":"example-cursor"}`,
		`{"entries":[],"tagged":true,"has_more":true,"after":"next-page","standing_bytes":4096}`)
	checkMethodWire[buswire.None, wire.AuthStatus](t, extwire.AuthStatusMethod, "auth.status",
		`{}`, `{"state":"signed_in","account":"me@example.com","message":"Signed in","expires_at":"2030-01-02T03:04:05Z"}`)
	checkMethodWire[buswire.None, wire.AuthStatus](t, extwire.AuthLogoutMethod, "auth.logout",
		`{}`, `{"state":"signed_out"}`)
	checkMethodWire[extwire.UIPageSessionParams, buswire.None](t, extwire.UIPageDisposeMethod, "ui.page.dispose",
		`{"session":"page-1"}`, `{}`)
	checkMethodWire[extwire.UIPageSessionParams, buswire.None](t, extwire.UIPageSuspendMethod, "ui.page.suspend",
		`{"session":"page-1"}`, `{}`)
	checkMethodWire[buswire.None, extwire.UIPageSupportedResult](t, extwire.UIPageSupportedMethod, "ui.page.supported",
		`{}`, `{"contributions":["overview","detail"]}`)
	checkMethodWire[extwire.AssistantCredentialsParams, extwire.AssistantCredentialsResult](t, extwire.AssistantCredentialsMethod, "assistant.credentials",
		`{"contribution":"provider"}`, `{"env":{"EXAMPLE_REGION":"example","EXAMPLE_USER":"me@example.com"}}`)
	checkMethodWire[extwire.AssistantSummaryParams, extwire.AssistantSummaryResult](t, extwire.AssistantSummaryMethod, "assistant.summary",
		`{"contribution":"provider","summary":"Title \u0026 details\nNext line"}`, `{"summary":"Rewritten title"}`)
}

func TestAssistantCallsOmitEmptyContribution(t *testing.T) {
	checkMethodWire[extwire.AssistantCredentialsParams, extwire.AssistantCredentialsResult](t, extwire.AssistantCredentialsMethod, "assistant.credentials",
		`{}`, `{"env":{}}`)
	checkMethodWire[extwire.AssistantSummaryParams, extwire.AssistantSummaryResult](t, extwire.AssistantSummaryMethod, "assistant.summary",
		`{"summary":""}`, `{"summary":""}`)
}

func TestUIPageCallsIgnoreAcknowledgmentBody(t *testing.T) {
	for _, method := range []buswire.Method[extwire.UIPageSessionParams, buswire.None]{
		extwire.UIPageDisposeMethod,
		extwire.UIPageSuspendMethod,
	} {
		t.Run(method.Name, func(t *testing.T) {
			caller := extensionCallFunc(func(_ context.Context, _ string, _ any, out any) error {
				if out != nil {
					t.Fatalf("acknowledgment decode destination = %T, want nil", out)
				}
				return context.Canceled
			})
			if _, err := buswire.Call(t.Context(), caller, method, extwire.UIPageSessionParams{Session: "page-1"}); err != context.Canceled {
				t.Fatalf("call error = %v, want %v", err, context.Canceled)
			}
		})
	}
}

type extensionCallFunc func(context.Context, string, any, any) error

func (f extensionCallFunc) Call(ctx context.Context, method string, params, out any) error {
	return f(ctx, method, params, out)
}

func checkMethodWire[P, R any](t *testing.T, method buswire.Method[P, R], name, request, reply string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if method.Name != name {
			t.Fatalf("method = %q, want %q", method.Name, name)
		}
		checkJSONRoundTrip[P](t, request)
		checkJSONRoundTrip[R](t, reply)
	})
}

func checkJSONRoundTrip[T any](t *testing.T, want string) {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(want), &value); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(value)
	if err != nil || string(got) != want {
		t.Errorf("%T wire = %s, %v, want %s", value, got, err, want)
	}
}
