package node

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nwverse/persona/server/internal/proto"
)

const accountHelper = `#!/usr/bin/env bash
set -euo pipefail
touch "$HOME/helper-ran"
[ $# -ge 3 ] || { echo "missing account" >&2; exit 2; }
account=$1 port=$2 profile=${3}; shift 3
root="$HOME/.config/claude-accounts/$account"
[ -f "$root/credentials.json" ] || exit 1
helper_dir="$(dirname "$(readlink -f "$0")")"
python3 "$helper_dir/prepare.py" "$root"
export HTTPS_PROXY="http://127.0.0.1:$port"
export root="$HOME/unused" CLAUDE_CONFIG_DIR="$root/config"
unset ANTHROPIC_BASE_URL
exec "$HOME/bin/claude" --settings \
  "$HOME/.claude/profiles/$profile.json" "$@"
`

func TestForwardedLauncherMatchesConfigDir(t *testing.T) {
	home, real, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeScript(t, filepath.Join(real, "claude-account"), accountHelper, 0o755)
	names := []string{"claude-work", "claude-home"}
	for i, account := range []string{"work", "home"} {
		path := filepath.Join(real, names[i])
		writeScript(t, path, `#!/usr/bin/env bash
exec "$(dirname "$(readlink -f "$0")")/claude-account" `+account+` 9000 `+account+` "$@"
`, 0o755)
		if err := os.Symlink(path, filepath.Join(bin, names[i])); err != nil {
			t.Fatal(err)
		}
	}
	list, settings, _ := scanPath(bin, nil).result(nil)
	if got := launcherNames(list); !slices.Equal(got, []string{"claude-home:path", "claude-work:path"}) {
		t.Fatalf("scan returned %v", got)
	}
	for i, account := range []string{"work", "home"} {
		dir := filepath.Join(home, ".config", "claude-accounts", account, "config")
		if got := matchLauncher(list, "", "", dir); got != names[i] {
			t.Errorf("match for %s = %q", account, got)
		}
		if got, want := settings[names[i]], filepath.Join(home, ".claude", "profiles", account+".json"); got != want {
			t.Errorf("settings for %s = %q, want %q", account, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "helper-ran")); !os.IsNotExist(err) {
		t.Fatalf("scan executed helper setup: %v", err)
	}
}

func TestForwardedLauncherRejectsDynamicScripts(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "claude-work")
	helper := filepath.Join(home, "claude-account")
	const direct = `#!/bin/bash
export CLAUDE_CONFIG_DIR="$HOME/.claude-work"
exec claude "$@"
`
	const forward = `#!/bin/bash
exec "$(dirname "$0")/claude-account" "$@"
`
	for _, tc := range []struct{ name, wrapper, helper string }{
		{"nested hop", forward, forward},
		{"unknown argument", `#!/bin/sh
exec "$HOME/claude-account" "$(account)" "$@"`, direct},
		{"unknown path", forward, `#!/bin/sh
export CLAUDE_CONFIG_DIR="$(account)"
exec claude "$@"`},
		{"conditional export", forward, `#!/bin/sh
[ -n "$ACCOUNT" ] && export CLAUDE_CONFIG_DIR="$HOME/.claude-work"
exec claude "$@"`},
		{"sourced environment", forward, `#!/bin/sh
. "$HOME/account.env"
` + direct},
		{"other endpoint", forward, `#!/bin/sh
export ANTHROPIC_BASE_URL=https://gateway.example.com
` + direct},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeScript(t, helper, tc.helper, 0o755)
			if _, ok := forwardedClaudeWrapper(path, tc.wrapper, home); ok {
				t.Fatal("inferred a launcher from unsupported script")
			}
		})
	}
}

func TestInferLauncherRejectsSharedDefaultConfigDir(t *testing.T) {
	const id = "example-session"
	dir := t.TempDir()
	n := &Node{cdirs: &claudeDirs{
		bySession: map[string]procEnv{id: {ConfigDir: dir}},
		scanned:   time.Now(),
	}}
	n.managed.byID = map[string]proto.ManagedLauncher{
		"managed": {Name: "managed", Env: "CLAUDE_CONFIG_DIR=" + dir},
	}
	launcherCache.Lock()
	oldAt, oldList := launcherCache.at, launcherCache.list
	launcherCache.at, launcherCache.list = time.Now(), []proto.Launcher{{Name: "claude-work", ConfigDir: dir}}
	launcherCache.Unlock()
	t.Cleanup(func() {
		launcherCache.Lock()
		launcherCache.at, launcherCache.list = oldAt, oldList
		launcherCache.Unlock()
	})
	if got := n.inferLauncher(id); got != "" {
		t.Errorf("matched %q despite two launchers sharing the directory", got)
	}
}
