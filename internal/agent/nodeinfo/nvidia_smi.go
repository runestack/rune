package nodeinfo

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// nvidiaSMIBinary is the probe binary. Not configurable: it is looked up
// on PATH and every argument below is a literal, so there is no
// argument-injection surface.
const nvidiaSMIBinary = "nvidia-smi"

// nvidiaSMIQuery is the field list, in the order parseNVIDIACSV expects.
const nvidiaSMIQuery = "uuid,index,name,memory.total,driver_version"

// maxProbeOutputBytes and maxProbeRows bound what the probe will accept.
// nvidia-smi output is untrusted input: a host with many devices — or a
// per-process query a hostile container can pad with CUDA processes —
// must not become unbounded memory here. On exceeding either bound the
// probe is REJECTED rather than truncated: a truncated inventory is a
// silently wrong answer, and DeviceProbeError is where the failure
// belongs.
const (
	maxProbeOutputBytes = 1 << 20 // 1 MiB
	maxProbeRows        = 256
)

// NVIDIASMIProvider probes with `nvidia-smi`.
//
// exec, not NVML: go-nvml is a cgo binding, and this tree builds both
// binaries with CGO_ENABLED=0 across six OS/arch targets with no `import
// "C"` anywhere. Turning cgo on to read a GPU would break the
// cross-compile matrix, end the static single binary, and change the
// shipped artifact for every user who does not have a GPU. A purego
// dlopen of NVML would avoid the fork-per-call cost and can land behind
// this same interface, invisible to callers.
func NVIDIASMIProvider() DeviceProvider { return nvidiaSMIProvider{} }

type nvidiaSMIProvider struct{}

func (nvidiaSMIProvider) Name() string { return "nvidia-smi" }

func (p nvidiaSMIProvider) Probe(ctx context.Context) ([]types.GPUDevice, error) {
	path, err := exec.LookPath(nvidiaSMIBinary)
	if err != nil {
		// The single most common first-run state, and it must read as
		// itself rather than as "no devices".
		return nil, fmt.Errorf("nvidia-smi not found on PATH")
	}

	// NOTE: the context is passed for cancellation propagation only. It
	// does NOT bound this call — a process blocked in an uninterruptible
	// driver ioctl cannot be killed, so cmd.Wait() outlives any deadline.
	// The bound that actually holds is the subsystem's: run the probe in
	// a goroutine and abandon it.
	cmd := exec.CommandContext(ctx, path,
		"--query-gpu="+nvidiaSMIQuery,
		"--format=csv,noheader,nounits",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("nvidia-smi: %s", firstLine(msg))
		}
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	if stdout.Len() > maxProbeOutputBytes {
		return nil, fmt.Errorf("nvidia-smi returned %d bytes, over the %d-byte cap",
			stdout.Len(), maxProbeOutputBytes)
	}

	devices, err := parseNVIDIACSV(stdout.String())
	if err != nil {
		return nil, err
	}
	return devices, nil
}

// parseNVIDIACSV turns `--format=csv,noheader,nounits` rows into devices.
// Field order is nvidiaSMIQuery's; a row that does not match is an error,
// not a skip, because CSV drift on a new driver version is one of the
// causes DeviceProbeError exists to name.
func parseNVIDIACSV(out string) ([]types.GPUDevice, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 5

	var devices []types.GPUDevice
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi CSV (expected %q): %w", nvidiaSMIQuery, err)
		}
		if len(devices) >= maxProbeRows {
			return nil, fmt.Errorf("nvidia-smi reported more than %d devices", maxProbeRows)
		}

		uuid := strings.TrimSpace(rec[0])
		if uuid == "" {
			return nil, fmt.Errorf("nvidia-smi CSV: device with empty uuid")
		}
		index, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi CSV: index %q: %w", rec[1], err)
		}
		// `nounits` reports memory.total in MiB.
		mib, err := strconv.ParseInt(strings.TrimSpace(rec[3]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi CSV: memory.total %q: %w", rec[3], err)
		}

		devices = append(devices, types.GPUDevice{
			UUID:          uuid,
			Index:         index,
			Vendor:        "nvidia",
			Product:       strings.TrimSpace(rec[2]),
			VRAMBytes:     mib * 1024 * 1024,
			DriverVersion: strings.TrimSpace(rec[4]),
		})
	}
	return devices, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// SelectProvider maps the --gpu-provider value to an implementation.
//
// "auto" is the default and resolves to nvidia-smi only when the binary
// is on PATH; a machine without it selects the null provider and does no
// work at all. "none" is the operator's guarantee that nothing is probed.
func SelectProvider(name string) (DeviceProvider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto":
		if _, err := exec.LookPath(nvidiaSMIBinary); err != nil {
			return NullProvider(), nil
		}
		return NVIDIASMIProvider(), nil
	case "none":
		return NullProvider(), nil
	case "nvidia-smi":
		return NVIDIASMIProvider(), nil
	default:
		return nil, fmt.Errorf("unknown gpu provider %q (want auto, none or nvidia-smi)", name)
	}
}
