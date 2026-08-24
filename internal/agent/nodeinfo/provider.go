package nodeinfo

import (
	"context"

	"github.com/runestack/rune/pkg/types"
)

// DeviceProvider reports the GPU devices physically present on this node.
//
// Inventory is a dependency rather than a direct driver call so that code
// which reasons about capacity stays testable on GPU-less CI, which is
// all of CI. Implementations: nullProvider (the default, and the only one
// a machine without a driver ever selects), nvidiaSMIProvider, and a
// static fixture provider for tests.
//
// The contract that matters: Probe returning (nil, nil) is the NORMAL
// GPU-less result, not an error path. An error means the probe could not
// answer, which is a different state and is reported as one.
type DeviceProvider interface {
	// Name identifies the provider in logs and in the node record.
	Name() string

	// Probe returns the devices present, or an error explaining why the
	// question could not be answered.
	Probe(ctx context.Context) ([]types.GPUDevice, error)
}

// nullProvider answers "no devices" without touching anything. It is the
// default, so a machine with no GPU support configured does no work at
// all beyond one empty answer.
type nullProvider struct{}

func (nullProvider) Name() string { return "none" }

func (nullProvider) Probe(context.Context) ([]types.GPUDevice, error) { return nil, nil }

// NullProvider returns the no-op provider.
func NullProvider() DeviceProvider { return nullProvider{} }

// StaticProvider returns a provider that answers with a fixed device
// list — the seam that lets capacity logic be tested on a laptop.
func StaticProvider(name string, devices []types.GPUDevice, err error) DeviceProvider {
	return staticProvider{name: name, devices: devices, err: err}
}

type staticProvider struct {
	name    string
	devices []types.GPUDevice
	err     error
}

func (p staticProvider) Name() string {
	if p.name == "" {
		return "static"
	}
	return p.name
}

func (p staticProvider) Probe(context.Context) ([]types.GPUDevice, error) {
	return p.devices, p.err
}
