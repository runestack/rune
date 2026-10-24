package volumes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/store"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// parkedFile, under MountRoot, lists the disks left attached to this node
// that nothing tracks. It holds identities only, never credentials; the
// driver context is rebuilt from the store when the disk is retried.
const parkedFile = ".parked.json"

// parkedWarnEvery throttles the repeat warning for a disk that stays
// parked, so a long-buried disk is visible without flooding the log.
const parkedWarnEvery = 10 * time.Minute

type parkedRecord struct {
	Key          string `json:"key"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	VolumeID     string `json:"volumeId,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	Handle       string `json:"handle"`
	Device       string `json:"device,omitempty"`
	Target       string `json:"target,omitempty"`
}

type parkedDisk struct {
	rec parkedRecord
	// m is nil for a disk loaded from parkedFile until the retry tick
	// rebuilds its driver context.
	m        *trackedMount
	lastWarn time.Time
}

func recordFor(id string, m trackedMount) parkedRecord {
	return parkedRecord{
		Key: id, Namespace: m.VolumeNS, Name: m.VolumeName, VolumeID: m.VolumeID,
		StorageClass: m.StorageClass, Handle: string(m.Handle),
		Device: string(m.Device), Target: string(m.Target),
	}
}

// park records a disk whose teardown left it attached, and says so.
func (s *Subsystem) park(id string, m trackedMount, cause error) {
	s.stateMu.Lock()
	p, again := s.parked[m.Handle]
	if !again {
		p = &parkedDisk{}
		s.parked[m.Handle] = p
	}
	p.rec, p.m = recordFor(id, m), &m
	warn := time.Since(p.lastWarn) >= parkedWarnEvery
	if warn {
		p.lastWarn = time.Now()
	}
	reason := "volume torn down but its disk is still attached to this node; retrying"
	if cause != nil {
		reason += ": " + cause.Error()
	}
	s.lastErr[id] = mountFailure{Err: errors.New(reason), When: time.Now()}
	s.stateMu.Unlock()
	s.saveParked()
	if warn {
		s.log.Warn("Volume disk left attached; will retry teardown",
			log.Str("volume_id", id), log.Str("handle", string(m.Handle)), log.Err(cause))
	}
}

// unpark drops a disk that is in use again.
func (s *Subsystem) unpark(h driver.VolumeHandle) {
	s.stateMu.Lock()
	_, ok := s.parked[h]
	delete(s.parked, h)
	s.stateMu.Unlock()
	if ok {
		s.saveParked()
	}
}

// release tears a volume down and parks its disk if it stays attached.
func (s *Subsystem) release(ctx context.Context, id string, m trackedMount) error {
	detached, err := s.tearDown(ctx, id, m)
	if !detached {
		s.park(id, m, err)
		return err
	}
	s.unpark(m.Handle)
	s.stateMu.Lock()
	delete(s.lastErr, id)
	s.stateMu.Unlock()
	return err
}

// retryParked retries teardown for every parked disk that is not in use
// again. A disk still buried under another disk's mount waits for that
// mount to go.
func (s *Subsystem) retryParked(ctx context.Context) {
	// A disk in use again was unparked by its bring-up.
	s.stateMu.Lock()
	todo := make([]*parkedDisk, 0, len(s.parked))
	for _, p := range s.parked {
		todo = append(todo, p)
	}
	s.stateMu.Unlock()
	for _, p := range todo {
		// Still bound here: the volume is wanted and its bring-up is what
		// needs retrying (the regular reconcile does that). Tearing it
		// down would pull the disk from under running containers.
		if s.stillWanted(ctx, p.rec) {
			s.warnParked(p, "Parked disk is still bound to this node; leaving it for bring-up", nil)
			continue
		}
		if p.m == nil {
			m, err := s.rebuildParked(ctx, p.rec)
			if err != nil {
				s.warnParked(p, "Cannot rebuild the driver context for a parked disk", err)
				continue
			}
			p.m = &m
		}
		if s.buried(p.m.Device) {
			s.warnParked(p, "Parked disk is still mounted under another disk; waiting", nil)
			continue
		}
		if err := s.release(ctx, p.rec.Key, *p.m); err != nil {
			s.warnParked(p, "Retry of parked volume teardown failed", err)
		}
	}
}

// stillWanted reports whether the store still binds this disk to this
// node. A lookup failure other than "gone" counts as wanted, so an
// unreachable store never triggers a teardown.
func (s *Subsystem) stillWanted(ctx context.Context, rec parkedRecord) bool {
	var vol types.Volume
	if err := s.cfg.Store.Get(ctx, types.ResourceTypeVolume, rec.Namespace, rec.Name, &vol); err != nil {
		return !store.IsNotFoundError(err)
	}
	return vol.Handle == rec.Handle && s.shouldMount(&vol)
}

func (s *Subsystem) warnParked(p *parkedDisk, msg string, err error) {
	s.stateMu.Lock()
	warn := time.Since(p.lastWarn) >= parkedWarnEvery
	if warn {
		p.lastWarn = time.Now()
	}
	s.stateMu.Unlock()
	if warn {
		s.log.Warn(msg, log.Str("volume_id", p.rec.Key), log.Str("handle", p.rec.Handle), log.Err(err))
	}
}

// rebuildParked recreates what teardown needs for a disk loaded from
// parkedFile. The live volume row is preferred; if it is gone or now
// names another disk, the record's own identity stands in for it.
func (s *Subsystem) rebuildParked(ctx context.Context, rec parkedRecord) (trackedMount, error) {
	vol := &types.Volume{}
	if err := s.cfg.Store.Get(ctx, types.ResourceTypeVolume, rec.Namespace, rec.Name, vol); err != nil || vol.Handle != rec.Handle {
		vol = &types.Volume{ID: rec.VolumeID, Name: rec.Name, Namespace: rec.Namespace,
			StorageClassName: rec.StorageClass, Handle: rec.Handle}
	}
	drv, err := s.cfg.Lookup(ctx, vol)
	if err != nil {
		return trackedMount{}, fmt.Errorf("lookup driver: %w", err)
	}
	raw, err := s.buildOpContextRaw(ctx, vol)
	if err != nil {
		return trackedMount{}, err
	}
	rawParams := mergeParameters(raw.Parameters, nil)
	opctx, err := s.resolveSecretsOnOpContext(ctx, vol, raw)
	if err != nil {
		return trackedMount{}, err
	}
	return trackedMount{
		Driver: drv, Handle: driver.VolumeHandle(rec.Handle), Device: driver.DevicePath(rec.Device),
		Target: driver.MountTarget(rec.Target), OpCtx: opctx, RawParameters: rawParams,
		VolumeNS: rec.Namespace, VolumeName: rec.Name, VolumeID: rec.VolumeID, StorageClass: rec.StorageClass,
	}, nil
}

func (s *Subsystem) parkedPath() string { return filepath.Join(s.cfg.MountRoot, parkedFile) }

// saveParked rewrites parkedFile from the in-memory set. The snapshot
// and the write happen under saveMu, so concurrent parks cannot write an
// older set over a newer one.
func (s *Subsystem) saveParked() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.stateMu.RLock()
	recs := make([]parkedRecord, 0, len(s.parked))
	for _, p := range s.parked {
		recs = append(recs, p.rec)
	}
	s.stateMu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].Handle < recs[j].Handle })
	if err := writeParked(s.parkedPath(), recs); err != nil {
		s.log.Warn("Cannot record parked volume disks; a restart will not retry them",
			log.Str("path", s.parkedPath()), log.Err(err))
	}
}

func writeParked(path string, recs []parkedRecord) error {
	if len(recs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Make the rename itself durable.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// loadParked restores the parked set recorded by a previous run.
func (s *Subsystem) loadParked() {
	b, err := os.ReadFile(s.parkedPath())
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("Cannot read parked volume disks", log.Str("path", s.parkedPath()), log.Err(err))
		}
		return
	}
	var recs []parkedRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		s.log.Warn("Cannot parse parked volume disks", log.Str("path", s.parkedPath()), log.Err(err))
		return
	}
	s.stateMu.Lock()
	for _, r := range recs {
		s.parked[driver.VolumeHandle(r.Handle)] = &parkedDisk{rec: r}
	}
	s.stateMu.Unlock()
	if len(recs) > 0 {
		s.log.Warn("Volume disks left attached by a previous run; will retry teardown unless in use",
			log.Int("disks", len(recs)))
	}
}
