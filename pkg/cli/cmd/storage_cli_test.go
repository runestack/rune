// Tests for the storage CLI noun-trees and generic-verb integration.
package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// TestStorageResourceAliases verifies the storage resource aliases added in
// RUNE-072 resolve to the canonical names handled by handleVolumeGet /
// handleStorageClassGet. Mirrors the alias pattern used by services/secrets.
func TestStorageResourceAliases(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"volume", "volume"},
		{"volumes", "volume"},
		{"vol", "volume"},
		{"storageclass", "storageclass"},
		{"storageclasses", "storageclass"},
		{"sc", "storageclass"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := resolveResourceType(tc.input)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestParseLabelSelectorString covers the small selector parser shared by the
// storage noun-tree subcommands. Empty and malformed segments are dropped
// silently so muscle-memory invocations keep working.
func TestParseLabelSelectorString(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{"empty", "", nil},
		{"single", "app=web", map[string]string{"app": "web"}},
		{"multi", "app=web,tier=frontend", map[string]string{"app": "web", "tier": "frontend"}},
		{"trims-whitespace", "  app  =  web  ", map[string]string{"app": "web"}},
		{"drops-malformed", "app=web,,malformed,=novalue,key=", map[string]string{"app": "web", "key": ""}},
		{"only-key-no-eq", "only-key", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLabelSelectorString(tc.input)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestNewStorageCommands ensures the noun-tree groups expose the expected
// subcommands so accidental command renames don't slip through. It also
// asserts the documented aliases (`ls`, `remove`, `rm`) so the symmetry with
// `rune secret` is preserved.
func TestNewStorageCommands(t *testing.T) {
	t.Run("storageclass", func(t *testing.T) {
		cmd := newStorageClassCmd()
		assert.Contains(t, cmd.Aliases, "sc")
		names := subcommandNames(cmd)
		// `create` was removed in v0.0.1-dev.46 — StorageClass cast
		// files are deployed via `rune cast`, matching every other
		// resource. Assert it's gone so it doesn't reappear by accident.
		for _, want := range []string{"list", "get", "delete", "set-default"} {
			assert.Contains(t, names, want, "storageclass missing %q", want)
		}
		assert.NotContains(t, names, "create", "storageclass create was removed; use `rune cast` instead")
		assertHasAlias(t, cmd, "list", "ls")
		assertHasAlias(t, cmd, "delete", "remove")
		assertHasAlias(t, cmd, "delete", "rm")
	})

	t.Run("volume", func(t *testing.T) {
		cmd := newVolumeCmd()
		assert.Contains(t, cmd.Aliases, "vol")
		names := subcommandNames(cmd)
		// `create` was removed in v0.0.1-dev.46 — Volume cast files are
		// deployed via `rune cast`. Assert it's gone.
		for _, want := range []string{"list", "get", "delete", "detach", "retry-provision", "restore"} {
			assert.Contains(t, names, want, "volume missing %q", want)
		}
		assert.NotContains(t, names, "create", "volume create was removed; use `rune cast` instead")
		assertHasAlias(t, cmd, "list", "ls")
		assertHasAlias(t, cmd, "delete", "remove")
		assertHasAlias(t, cmd, "delete", "rm")
	})

	t.Run("snapshot", func(t *testing.T) {
		cmd := newSnapshotCmd()
		assert.Contains(t, cmd.Aliases, "snap")
		names := subcommandNames(cmd)
		for _, want := range []string{"list", "get", "create", "delete", "restore"} {
			assert.Contains(t, names, want, "snapshot missing %q", want)
		}
	})
}

// subcommandNames returns the Use-name of each direct subcommand (first
// whitespace-delimited token) for a Cobra command group.
func subcommandNames(parent *cobra.Command) []string {
	out := make([]string, 0, len(parent.Commands()))
	for _, c := range parent.Commands() {
		out = append(out, c.Name())
	}
	return out
}

// assertHasAlias finds the named subcommand and asserts it carries the
// expected alias.
func assertHasAlias(t *testing.T, parent *cobra.Command, subName, alias string) {
	t.Helper()
	for _, c := range parent.Commands() {
		if c.Name() == subName {
			assert.Contains(t, c.Aliases, alias, "%s.%s missing alias %q", parent.Name(), subName, alias)
			return
		}
	}
	t.Fatalf("%s: subcommand %q not found", parent.Name(), subName)
}
