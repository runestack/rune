package service

import (
	"context"
	"strings"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDescribeTestService(t *testing.T) (*DescribeService, *store.TestStore) {
	t.Helper()
	st := store.NewTestStore()
	return NewDescribeService(st, nil, log.GetDefaultLogger()), st
}

func putInstance(t *testing.T, st *store.TestStore, in *types.Instance) {
	t.Helper()
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeInstance, in.Namespace, in.ID, in))
}

func TestDescribe_Instance_StatusAndReason(t *testing.T) {
	svc, st := newDescribeTestService(t)
	putInstance(t, st, &types.Instance{
		ID: "i-1", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		NodeID: "local", Runner: types.RunnerTypeDocker,
		Status: types.InstanceStatusStalled, StatusReason: "VolumeNotReady",
		StatusMessage: "volume shared/flo-data-flo-0 is not ready",
	})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "flo-0", Namespace: "shared",
	})
	require.NoError(t, err)
	r := resp.Result
	assert.Equal(t, "Instance", r.Kind)
	assert.Equal(t, "flo-0", r.Name)
	assert.Equal(t, "Stalled", r.Status)
	assert.Equal(t, "VolumeNotReady", r.Reason)
	assert.Contains(t, r.Message, "not ready")
	// Identity carries the ID.
	var gotID string
	for _, kv := range r.Identity {
		if kv.Key == "ID" {
			gotID = kv.Value
		}
	}
	assert.Equal(t, "i-1", gotID)
}

func TestDescribe_Instance_VolumeMountResolved(t *testing.T) {
	svc, st := newDescribeTestService(t)
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeVolume, "shared", "flo-data-flo-0",
		&types.Volume{ID: "v-1", Name: "flo-data-flo-0", Namespace: "shared",
			Status: types.VolumeStatusStalled, StatusReason: "ProvisionRetriesExhausted"}))
	putInstance(t, st, &types.Instance{
		ID: "i-1", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		Status: types.InstanceStatusPending,
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "flo-data", VolumeName: "flo-data-flo-0", VolumeNamespace: "shared", MountPath: "/data"},
			},
		},
	})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "flo-0", Namespace: "shared",
	})
	require.NoError(t, err)

	var volRef *generated.DescribeReference
	for _, ref := range resp.Result.References {
		if ref.Relation == "volumeMount" {
			volRef = ref
		}
	}
	require.NotNil(t, volRef, "expected a volumeMount reference")
	assert.False(t, volRef.Unresolved)
	assert.Equal(t, "Stalled", volRef.Status)
	assert.Equal(t, "ProvisionRetriesExhausted", volRef.StatusReason)
}

// A missing referenced volume must NOT fail the RPC — it surfaces as an
// unresolved reference inside the result.
func TestDescribe_Instance_VolumeMountUnresolved(t *testing.T) {
	svc, st := newDescribeTestService(t)
	putInstance(t, st, &types.Instance{
		ID: "i-1", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		Status: types.InstanceStatusPending,
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "flo-data", VolumeName: "ghost-volume", VolumeNamespace: "shared", MountPath: "/data"},
			},
		},
	})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "flo-0", Namespace: "shared",
	})
	require.NoError(t, err, "missing reference must not fail the RPC")

	var volRef *generated.DescribeReference
	for _, ref := range resp.Result.References {
		if ref.Relation == "volumeMount" {
			volRef = ref
		}
	}
	require.NotNil(t, volRef)
	assert.True(t, volRef.Unresolved)
	assert.Contains(t, volRef.Detail, "unresolved")
}

// When a slot has both a Deleted tombstone and a live record, describe
// must pick the live one.
func TestDescribe_Instance_PrefersLiveOverTombstone(t *testing.T) {
	svc, st := newDescribeTestService(t)
	putInstance(t, st, &types.Instance{
		ID: "i-old", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		Status: types.InstanceStatusDeleted, StatusMessage: "Marked for deletion",
		UpdatedAt: time.Now().Add(-time.Hour),
	})
	putInstance(t, st, &types.Instance{
		ID: "i-live", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		Status: types.InstanceStatusRunning, UpdatedAt: time.Now(),
	})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "flo-0", Namespace: "shared",
	})
	require.NoError(t, err)
	assert.Equal(t, "Running", resp.Result.Status)
	for _, kv := range resp.Result.Identity {
		if kv.Key == "ID" {
			assert.Equal(t, "i-live", kv.Value)
		}
	}
}

func TestDescribe_Instance_NotFound(t *testing.T) {
	svc, _ := newDescribeTestService(t)
	_, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "nope", Namespace: "shared",
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestDescribe_Service_ReplicaRollup(t *testing.T) {
	svc, st := newDescribeTestService(t)
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeService, "shared", "flo",
		&types.Service{ID: "s-1", Name: "flo", Namespace: "shared", Scale: 2,
			Status: types.ServiceStatusPending, StatusReason: "InstanceCreateFailing"}))
	putInstance(t, st, &types.Instance{ID: "i-1", Name: "flo-0", Namespace: "shared",
		ServiceName: "flo", Status: types.InstanceStatusRunning})
	putInstance(t, st, &types.Instance{ID: "i-2", Name: "flo-1", Namespace: "shared",
		ServiceName: "flo", Status: types.InstanceStatusPending, StatusReason: "VolumeNotReady"})
	// A Deleted tombstone for the slot must not inflate the rollup.
	putInstance(t, st, &types.Instance{ID: "i-old", Name: "flo-0", Namespace: "shared",
		ServiceName: "flo", Status: types.InstanceStatusDeleted})

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "service", Name: "flo", Namespace: "shared",
	})
	require.NoError(t, err)
	assert.Equal(t, "Service", resp.Result.Kind)

	var replicas string
	for _, kv := range resp.Result.Identity {
		if kv.Key == "Replicas" {
			replicas = kv.Value
		}
	}
	assert.Contains(t, replicas, "desired=2")
	assert.Contains(t, replicas, "ready=1")

	instRefs := 0
	for _, ref := range resp.Result.References {
		if ref.Relation == "instance" {
			instRefs++
		}
	}
	assert.Equal(t, 2, instRefs)
}

func TestDescribe_Service_DiscoverySection(t *testing.T) {
	svc, st := newDescribeTestService(t)
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeService, "prod", "api",
		&types.Service{
			ID: "s-api", Name: "api", Namespace: "prod", Scale: 1,
			Status:    types.ServiceStatusRunning,
			Discovery: &types.ServiceDiscovery{VIP: "10.96.0.7", Mode: "load-balanced"},
			Ports: []types.ServicePort{
				{Name: "client", Port: 9000},
				{Name: "dashboard", Port: 9002, HostPort: 19002},
			},
		}))

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "service", Name: "api", Namespace: "prod",
	})
	require.NoError(t, err)

	var disc *generated.DescribeSection
	for _, sec := range resp.Result.Sections {
		if sec.Title == "Discovery" {
			disc = sec
		}
	}
	require.NotNil(t, disc, "expected a Discovery section")

	joined := strings.Join(disc.Lines, "\n")
	assert.Contains(t, joined, "10.96.0.7")
	assert.Contains(t, joined, "api.prod.rune")
	assert.Contains(t, joined, "api.prod.rune:9000 (client)")
	assert.Contains(t, joined, "api.prod.rune:9002 (dashboard)")
	// hostPort surfaces the dev loopback hint.
	assert.Contains(t, joined, "127.0.0.1:19002")
}

func TestDescribe_Service_DiscoveryOmittedWhenEmpty(t *testing.T) {
	svc, st := newDescribeTestService(t)
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeService, "prod", "bare",
		&types.Service{ID: "s-bare", Name: "bare", Namespace: "prod", Scale: 1,
			Status: types.ServiceStatusRunning}))

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "service", Name: "bare", Namespace: "prod",
	})
	require.NoError(t, err)
	for _, sec := range resp.Result.Sections {
		assert.NotEqual(t, "Discovery", sec.Title, "no VIP and no ports → no Discovery section")
	}
}

func TestDescribe_Volume_StorageClassRef(t *testing.T) {
	svc, st := newDescribeTestService(t)
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeStorageClass, "", "do-lon1",
		&types.StorageClass{Name: "do-lon1", Driver: "do-volume"}))
	require.NoError(t, st.Create(context.Background(), types.ResourceTypeVolume, "shared", "flo-data",
		&types.Volume{ID: "v-1", Name: "flo-data", Namespace: "shared", Size: "10Gi",
			StorageClassName: "do-lon1", Status: types.VolumeStatusBound,
			CreatedAt: time.Now().UTC()}))

	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "volume", Name: "flo-data", Namespace: "shared",
	})
	require.NoError(t, err)
	assert.Equal(t, "Volume", resp.Result.Kind)

	var scRef *generated.DescribeReference
	for _, ref := range resp.Result.References {
		if ref.Relation == "storageClass" {
			scRef = ref
		}
	}
	require.NotNil(t, scRef)
	assert.False(t, scRef.Unresolved)
	assert.Contains(t, scRef.Detail, "do-volume")
}

func TestDescribe_Node_Unimplemented(t *testing.T) {
	svc, _ := newDescribeTestService(t)
	_, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "node", Name: "local"})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// When a real EventLog is wired, describe folds the resource's recent
// events into result.Events. End-to-end check of the Phase 2 glue.
func TestDescribe_FoldsEventsWhenEventLogWired(t *testing.T) {
	st := store.NewTestStore()
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec, err := events.NewRecorder(db, log.GetDefaultLogger(), events.Options{})
	require.NoError(t, err)

	putInstance(t, st, &types.Instance{
		ID: "i-1", Name: "flo-0", Namespace: "shared", ServiceName: "flo",
		Status: types.InstanceStatusStalled, StatusReason: "VolumeNotReady",
		StatusMessage: "volume not ready",
	})
	require.NoError(t, rec.Emit(context.Background(), types.Event{
		Namespace: "shared", Kind: "Instance", Name: "flo-0", UID: "i-1",
		Level: types.EventLevelError, Reason: "VolumeNotReady",
		Message: "first failure",
	}))
	require.NoError(t, rec.Emit(context.Background(), types.Event{
		Namespace: "shared", Kind: "Instance", Name: "flo-0", UID: "i-1",
		Level: types.EventLevelError, Reason: "VolumeNotReady",
		Message: "first failure",
	}))

	svc := NewDescribeService(st, rec, log.GetDefaultLogger())
	resp, err := svc.Describe(context.Background(), &generated.DescribeRequest{
		Kind: "instance", Name: "flo-0", Namespace: "shared",
	})
	require.NoError(t, err)
	require.Len(t, resp.Result.Events, 1, "identical emits fold into one")
	assert.Contains(t, resp.Result.Events[0].Message, "×2",
		"folded count surfaces in the rendered message")
}

func TestDescribe_BadKind(t *testing.T) {
	svc, _ := newDescribeTestService(t)
	_, err := svc.Describe(context.Background(), &generated.DescribeRequest{Kind: "banana", Name: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
