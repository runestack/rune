// Volume live-usage enrichment (dashboard "Used" column).
package service

import (
	"context"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// volumeUsageTimeout bounds the whole usage fan-out for one RPC. The local
// drivers do a bounded directory walk; anything slower degrades to "no
// usage" rather than stalling the list.
const volumeUsageTimeout = 2 * time.Second

// usageDriver returns a memoized driver instance for the given driver name,
// constructed with the runefile [storage.drivers] config the service holds.
// Driver instances are stateless config holders, so caching one per name is
// safe and avoids re-parsing config on every list RPC.
func (s *VolumeService) usageDriver(name string) (driver.Driver, error) {
	s.driverMu.Lock()
	defer s.driverMu.Unlock()
	if s.drivers == nil {
		s.drivers = make(map[string]driver.Driver)
	}
	if d, ok := s.drivers[name]; ok {
		return d, nil
	}
	d, err := driver.New(name, s.driverConfigs[name])
	if err != nil {
		return nil, err
	}
	s.drivers[name] = d
	return d, nil
}

// enrichVolumesUsage attaches live usage (UsedBytes/CapacityBytes) to every
// provisioned volume whose driver implements the UsageReporter capability.
// Best-effort: failures leave the fields at 0 ("unknown"). Capacity falls
// back to the parsed spec size when the driver can't report a real device
// capacity (directory-backed local volumes).
func (s *VolumeService) enrichVolumesUsage(ctx context.Context, vols []*types.Volume) {
	if len(vols) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, volumeUsageTimeout)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, v := range vols {
		if v == nil || v.Handle == "" {
			continue
		}
		sc, err := s.scRepo.Get(ctx, v.StorageClassName)
		if err != nil || sc == nil {
			continue
		}
		d, err := s.usageDriver(sc.Driver)
		if err != nil {
			continue
		}
		ur, ok := d.(driver.UsageReporter)
		if !ok {
			// Driver can't measure usage; still surface the declared
			// capacity so the UI can show size without a percent.
			if n, perr := types.ParseMemory(v.Size); perr == nil && n > 0 {
				v.CapacityBytes = uint64(n)
			}
			continue
		}

		// Pre-merge parameters the way the controller does: class first,
		// volume overlay second.
		params := make(map[string]string, len(sc.Parameters)+len(v.Parameters))
		for k, val := range sc.Parameters {
			params[k] = val
		}
		for k, val := range v.Parameters {
			params[k] = val
		}
		opctx := driver.OpContext{StorageClass: sc, Volume: v, Parameters: params}

		wg.Add(1)
		sem <- struct{}{}
		go func(v *types.Volume) {
			defer wg.Done()
			defer func() { <-sem }()
			used, capacity, err := ur.Usage(ctx, opctx, driver.VolumeHandle(v.Handle))
			if err != nil {
				s.logger.Debug("volume usage unavailable",
					log.Str("volume", v.Name), log.Err(err))
				return
			}
			v.UsedBytes = used
			if capacity == 0 {
				if n, perr := types.ParseMemory(v.Size); perr == nil && n > 0 {
					capacity = uint64(n)
				}
			}
			v.CapacityBytes = capacity
		}(v)
	}
	wg.Wait()
}
