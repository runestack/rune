package agent

import "os"

// writeFile is a tiny helper used only by tests. Lives in a non-_test
// file so that test files can share it without import gymnastics, but
// guarded by the _test.go suffix on its sole user.
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
