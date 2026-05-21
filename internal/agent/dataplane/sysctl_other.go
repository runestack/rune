//go:build !linux

package dataplane

import "github.com/runestack/rune/pkg/log"

func ensureNonLocalBind(log.Logger) {}
func readNonLocalBind() bool         { return true }
