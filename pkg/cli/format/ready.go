package format

import "github.com/runestack/rune/pkg/types"

// InstanceReady is the READY column for an instance: whether a Running
// instance is receiving traffic. A failing readiness probe withdraws it while
// Status stays Running; "-" for anything not Running.
func InstanceReady(instance *types.Instance) string {
	if instance.Status != types.InstanceStatusRunning {
		return "-"
	}
	if instance.IsReady() {
		return "yes"
	}
	return "no"
}
