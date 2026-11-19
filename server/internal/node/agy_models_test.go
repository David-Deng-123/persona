package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona/server/internal/proto"
)

func TestAgyModelList(t *testing.T) {
	output := "Loading models...\r\nprogress message\tignored\r\n" +
		"\tmissing ID\r\nmissing-name\t\r\n" +
		"example-high\tExample High\r\nexample-high\tDuplicate\r\n" +
		"example\t\tExample Model\textra column\r\nexample-low\tExample Low\r\n" +
		"example-max\tExample Max\r\nexample-xhigh\tExample Extra High\r\nplain\tPlain Model"
	efforts := []string{"low", "high", "xhigh", "max"}
	want := []proto.AgentModel{
		{ID: "example-high", Name: "Example High", Efforts: efforts, DefaultEffort: "high"},
		{ID: "example", Name: "Example Model", Efforts: efforts},
		{ID: "example-low", Name: "Example Low", Efforts: efforts, DefaultEffort: "low"},
		{ID: "example-max", Name: "Example Max", Efforts: efforts, DefaultEffort: "max"},
		{ID: "example-xhigh", Name: "Example Extra High", Efforts: efforts, DefaultEffort: "xhigh"},
		{ID: "plain", Name: "Plain Model"},
	}
	got, err := agyModelList(strings.NewReader(output))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("agy models = %+v, %v; want %+v", got, err, want)
	}
}

func fakeAgyModels(t *testing.T, home string) (catalog, calls string) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	catalog, calls = filepath.Join(home, "catalog.txt"), filepath.Join(home, "agy-calls.txt")
	t.Setenv("AGY_FIXTURE_CATALOG", catalog)
	t.Setenv("AGY_FIXTURE_CALLS", calls)
	t.Setenv("AGY_FIXTURE_FAILURE", "")
	t.Setenv("AGY_FIXTURE_STDERR", "")
	withShellLaunchers(t)
	launcher := filepath.Join(bin, "agy")
	writeFile(t, launcher, `#!/bin/sh
printf '%s\n' "$*" >> "$AGY_FIXTURE_CALLS"
[ "$#" = 1 ] && [ "$1" = models ] || exit 2
cat "$AGY_FIXTURE_CATALOG"
if [ -n "$AGY_FIXTURE_FAILURE" ]; then
  if [ -n "$AGY_FIXTURE_STDERR" ]; then
    printf '%s\n' "$AGY_FIXTURE_FAILURE" >&2
  else
    printf '%s\n' "$AGY_FIXTURE_FAILURE"
  fi
  exit 1
fi
`)
	if err := os.Chmod(launcher, 0o700); err != nil {
		t.Fatal(err)
	}
	// Let launchers.list discover the executable from PATH.
	launcherCache.Lock()
	oldCodex, oldSettings, oldScripts := launcherCache.codex, launcherCache.settings, launcherCache.scripts
	launcherCache.list = nil
	launcherCache.Unlock()
	t.Cleanup(func() {
		launcherCache.Lock()
		launcherCache.codex, launcherCache.settings, launcherCache.scripts = oldCodex, oldSettings, oldScripts
		launcherCache.Unlock()
	})
	return catalog, calls
}

func TestAgyLauncherModelsDiscoveryAndRefresh(t *testing.T) {
	n, home := testNode(t)
	catalog, calls := fakeAgyModels(t, home)
	writeFile(t, catalog, "Loading models...\nexample-high\tExample High\nexample-high\tDuplicate\n")
	ask := func(refresh bool) (proto.LauncherModels, error) {
		t.Helper()
		raw, err := nodeCall(t, n, "launchers.models", map[string]any{"name": agyAgent, "host": proto.EngineHerdr, "refresh": refresh})
		var res proto.LauncherModels
		if err == nil {
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatal(err)
			}
		}
		return res, err
	}
	check := func(refresh bool, id, name string) {
		t.Helper()
		list, err := ask(refresh)
		if err != nil || list.Agent != agyAgent || list.Locked != "" || len(list.Models) != 1 ||
			list.Models[0].ID != id || list.Models[0].Name != name {
			t.Fatalf("launchers.models (refresh=%v) = %+v, %v", refresh, list, err)
		}
	}
	check(false, "example-high", "Example High")
	writeFile(t, catalog, "example-low\tExample Low\n")
	check(false, "example-high", "Example High")
	check(true, "example-low", "Example Low")
	for _, stream := range []string{"", "stderr"} {
		t.Setenv("AGY_FIXTURE_FAILURE", "Please sign in")
		t.Setenv("AGY_FIXTURE_STDERR", stream)
		if _, err := ask(true); err == nil || !strings.Contains(err.Error(), "Please sign in") {
			t.Fatalf("failed discovery (%q) = %v", stream, err)
		}
	}
	check(false, "example-low", "Example Low")
	t.Setenv("AGY_FIXTURE_FAILURE", "")
	writeFile(t, catalog, "Loading models...\n")
	if _, err := ask(true); err == nil || !strings.Contains(err.Error(), "returned no models") {
		t.Fatalf("empty discovery = %v", err)
	}
	got, err := os.ReadFile(calls)
	if err != nil || string(got) != strings.Repeat("models\n", 5) {
		t.Fatalf("agy calls = %q, %v", got, err)
	}
}

func TestAgyLauncherModelsUsesConfiguredLauncher(t *testing.T) {
	n, home := testNode(t)
	catalog, calls := fakeAgyModels(t, home)
	wrapper := filepath.Join(home, "bin", "my-agy")
	writeFile(t, wrapper, "#!/bin/sh\nprintf 'wrapper\\n' >> \"$AGY_FIXTURE_CALLS\"\nexec agy \"$@\"\n")
	if err := os.Chmod(wrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, catalog, "custom-medium\tConfigured Model\n")
	raw, err := nodeCall(t, n, "launchers.models", map[string]any{
		"name": "my-agy", "host": proto.EngineHerdr, "agent_type": agyAgent, "configured": true,
	})
	if err != nil || !strings.Contains(string(raw), "\"name\":\"Configured Model\"") {
		t.Fatalf("configured launcher catalog = %s, %v", raw, err)
	}
	got, err := os.ReadFile(calls)
	if err != nil || string(got) != "wrapper\nmodels\n" {
		t.Fatalf("agy calls = %q, %v", got, err)
	}
}

func TestAgyManagedDiscoveryUsesModelsSubcommand(t *testing.T) {
	got := launchArgv(agyAgent, []string{"models"}, []string{"--model", "fixed"}, "", "configured",
		[]string{"GEMINI_API_KEY=fixture"}, "")
	if !slices.Equal(got, []string{"agy", "models"}) {
		t.Fatalf("managed discovery argv = %q", got)
	}
}
