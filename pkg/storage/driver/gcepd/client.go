package gcepd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// gceClient is the minimal Compute Engine API surface gcepd needs.
// Implemented by sdkClient (production, backed by google.golang.org/api)
// and stubbed in tests.
//
// Disk/attach/detach/resize/snapshot calls all return a long-running
// Operation under the hood; the production client drives each to
// completion before returning, so the driver never deals with operation
// polling. Per-call auth (an optional service-account key) is carried on
// the context via withCreds / credsFromContext, mirroring how the
// do-volume driver stashes its bearer token.
type gceClient interface {
	insertDisk(ctx context.Context, project, zone string, spec diskSpec) error
	getDisk(ctx context.Context, project, zone, name string) (*gceDisk, error)
	deleteDisk(ctx context.Context, project, zone, name string) error
	resizeDisk(ctx context.Context, project, zone, name string, sizeGB int64) error
	attachDisk(ctx context.Context, project, zone, instance string, spec attachSpec) error
	detachDisk(ctx context.Context, project, zone, instance, deviceName string) error
	createSnapshot(ctx context.Context, project, zone, diskName, snapshotName, description string) error
	deleteSnapshot(ctx context.Context, project, snapshotName string) error
	getInstance(ctx context.Context, project, zone, name string) (*gceInstance, error)
}

// gceDisk mirrors the relevant fields of a Persistent Disk.
type gceDisk struct {
	Name     string
	SelfLink string
	SizeGB   int64
	Status   string   // CREATING | READY | FAILED | DELETING | ...
	Users    []string // selfLinks of instances the disk is attached to
}

// gceInstance is the slice of a GCE instance the driver cares about.
type gceInstance struct {
	Name     string
	SelfLink string
}

// diskSpec is the input to insertDisk.
type diskSpec struct {
	Name           string
	SizeGB         int64
	DiskType       string // short name, e.g. "pd-balanced"
	SourceSnapshot string // snapshot name; empty for a blank disk
	Labels         map[string]string
}

// attachSpec is the input to attachDisk.
type attachSpec struct {
	Source     string // disk selfLink
	DeviceName string
	ReadOnly   bool
}

// credsCtxKey carries an optional service-account JSON key for the call.
type credsCtxKey struct{}

func withCreds(ctx context.Context, credentialsJSON string) context.Context {
	return context.WithValue(ctx, credsCtxKey{}, credentialsJSON)
}

func credsFromContext(ctx context.Context) string {
	v, _ := ctx.Value(credsCtxKey{}).(string)
	return v
}

// errNotFound is the driver-internal sentinel for a GCE 404. The driver
// translates it into driver.ErrNotFound / nil at the boundary.
var errNotFound = errors.New("gcepd: gce resource not found")

func isGCENotFound(err error) bool {
	if errors.Is(err, errNotFound) {
		return true
	}
	var ae *googleapi.Error
	if errors.As(err, &ae) {
		return ae.Code == 404
	}
	return false
}

// sdkClient is the production gceClient backed by google.golang.org/api.
// compute.Service instances are built lazily and cached per credential
// set (project/zone are per-call method arguments), so a single driver
// instance serves StorageClasses spanning projects/zones without
// rebuilding the service every call.
type sdkClient struct {
	mu       sync.Mutex
	services map[string]*compute.Service

	// pollInterval governs operation polling. Default 2s.
	pollInterval time.Duration

	// newService is overridable in tests; defaults to compute.NewService.
	newService func(ctx context.Context, opts ...option.ClientOption) (*compute.Service, error)
}

func newSDKClient() *sdkClient {
	return &sdkClient{
		services:     make(map[string]*compute.Service),
		pollInterval: 2 * time.Second,
		newService:   compute.NewService,
	}
}

func (c *sdkClient) svc(ctx context.Context) (*compute.Service, error) {
	creds := credsFromContext(ctx)
	key := "adc"
	if creds != "" {
		sum := sha256.Sum256([]byte(creds))
		key = hex.EncodeToString(sum[:8])
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.services[key]; ok {
		return s, nil
	}
	opts := []option.ClientOption{option.WithScopes(compute.ComputeScope)}
	if creds != "" {
		opts = append(opts, option.WithCredentialsJSON([]byte(creds)))
	}
	s, err := c.newService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcepd: build compute service: %w", err)
	}
	c.services[key] = s
	return s, nil
}

func (c *sdkClient) interval() time.Duration {
	if c.pollInterval <= 0 {
		return 2 * time.Second
	}
	return c.pollInterval
}

// waitZoneOp / waitGlobalOp drive a long-running Operation to completion.
func (c *sdkClient) waitZoneOp(ctx context.Context, s *compute.Service, project, zone, op string) error {
	for {
		o, err := s.ZoneOperations.Get(project, zone, op).Context(ctx).Do()
		if err != nil {
			return err
		}
		if o.Status == "DONE" {
			return opError(o)
		}
		if err := sleepCtx(ctx, c.interval()); err != nil {
			return err
		}
	}
}

func (c *sdkClient) waitGlobalOp(ctx context.Context, s *compute.Service, project, op string) error {
	for {
		o, err := s.GlobalOperations.Get(project, op).Context(ctx).Do()
		if err != nil {
			return err
		}
		if o.Status == "DONE" {
			return opError(o)
		}
		if err := sleepCtx(ctx, c.interval()); err != nil {
			return err
		}
	}
}

func opError(o *compute.Operation) error {
	if o.Error == nil || len(o.Error.Errors) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(o.Error.Errors))
	for _, e := range o.Error.Errors {
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
	}
	return fmt.Errorf("gcepd: operation %s failed: %s", o.Name, strings.Join(msgs, "; "))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *sdkClient) insertDisk(ctx context.Context, project, zone string, spec diskSpec) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	disk := &compute.Disk{
		Name:   spec.Name,
		SizeGb: spec.SizeGB,
		Type:   fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", project, zone, spec.DiskType),
		Labels: spec.Labels,
	}
	if spec.SourceSnapshot != "" {
		disk.SourceSnapshot = fmt.Sprintf("projects/%s/global/snapshots/%s", project, spec.SourceSnapshot)
	}
	op, err := s.Disks.Insert(project, zone, disk).Context(ctx).Do()
	if err != nil {
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) getDisk(ctx context.Context, project, zone, name string) (*gceDisk, error) {
	s, err := c.svc(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.Disks.Get(project, zone, name).Context(ctx).Do()
	if err != nil {
		if isGCENotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	return &gceDisk{Name: d.Name, SelfLink: d.SelfLink, SizeGB: d.SizeGb, Status: d.Status, Users: d.Users}, nil
}

func (c *sdkClient) deleteDisk(ctx context.Context, project, zone, name string) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	op, err := s.Disks.Delete(project, zone, name).Context(ctx).Do()
	if err != nil {
		if isGCENotFound(err) {
			return errNotFound
		}
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) resizeDisk(ctx context.Context, project, zone, name string, sizeGB int64) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	op, err := s.Disks.Resize(project, zone, name, &compute.DisksResizeRequest{SizeGb: sizeGB}).Context(ctx).Do()
	if err != nil {
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) attachDisk(ctx context.Context, project, zone, instance string, spec attachSpec) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	mode := "READ_WRITE"
	if spec.ReadOnly {
		mode = "READ_ONLY"
	}
	ad := &compute.AttachedDisk{Source: spec.Source, DeviceName: spec.DeviceName, Mode: mode, AutoDelete: false}
	op, err := s.Instances.AttachDisk(project, zone, instance, ad).Context(ctx).Do()
	if err != nil {
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) detachDisk(ctx context.Context, project, zone, instance, deviceName string) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	op, err := s.Instances.DetachDisk(project, zone, instance, deviceName).Context(ctx).Do()
	if err != nil {
		if isGCENotFound(err) {
			return errNotFound
		}
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) createSnapshot(ctx context.Context, project, zone, diskName, snapshotName, description string) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	snap := &compute.Snapshot{Name: snapshotName, Description: description}
	op, err := s.Disks.CreateSnapshot(project, zone, diskName, snap).Context(ctx).Do()
	if err != nil {
		return err
	}
	return c.waitZoneOp(ctx, s, project, zone, op.Name)
}

func (c *sdkClient) deleteSnapshot(ctx context.Context, project, snapshotName string) error {
	s, err := c.svc(ctx)
	if err != nil {
		return err
	}
	op, err := s.Snapshots.Delete(project, snapshotName).Context(ctx).Do()
	if err != nil {
		if isGCENotFound(err) {
			return errNotFound
		}
		return err
	}
	return c.waitGlobalOp(ctx, s, project, op.Name)
}

func (c *sdkClient) getInstance(ctx context.Context, project, zone, name string) (*gceInstance, error) {
	s, err := c.svc(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := s.Instances.Get(project, zone, name).Context(ctx).Do()
	if err != nil {
		if isGCENotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	return &gceInstance{Name: inst.Name, SelfLink: inst.SelfLink}, nil
}
