package client

import (
	"reflect"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// Volume mounts must round-trip cleanly through gRPC; if the proto
// converter drops the Volumes slice (it used to, before RUNE-072) the
// API server can never reconcile a service that uses storage. Lock the
// invariant down with a deep-equal check on a representative spec.
func TestServiceProto_VolumesRoundTrip(t *testing.T) {
	svc := &types.Service{
		Name:      "demo",
		Namespace: "app",
		Image:     "nginx",
		Scale:     1,
		Volumes: []types.VolumeMount{
			{
				Name:      "data",
				MountPath: "/var/lib/data",
				ReadOnly:  false,
				SubPath:   "shard-0",
				Claim:     &types.VolumeClaim{Name: "shared"},
			},
			{
				Name:      "cache",
				MountPath: "/var/cache",
				ReadOnly:  true,
				ClaimTemplate: &types.VolumeClaimTemplate{
					StorageClassName: "fast-ssd",
					Size:             "5Gi",
					AccessMode:       types.AccessModeRWO,
					Parameters:       map[string]string{"fsType": "ext4"},
					ReclaimPolicy:    types.ReclaimPolicyDelete,
				},
			},
		},
	}

	out, err := ProtoToService(ServiceToProto(svc))
	if err != nil {
		t.Fatalf("round-trip error: %v", err)
	}
	if !reflect.DeepEqual(svc.Volumes, out.Volumes) {
		t.Fatalf("volumes round-trip mismatch:\nin:  %#v\nout: %#v", svc.Volumes, out.Volumes)
	}
}

func TestServiceProto_NoVolumes(t *testing.T) {
	svc := &types.Service{Name: "x", Namespace: "ns", Image: "x", Scale: 1}
	out, err := ProtoToService(ServiceToProto(svc))
	if err != nil {
		t.Fatalf("round-trip error: %v", err)
	}
	if len(out.Volumes) != 0 {
		t.Fatalf("expected no volumes, got %#v", out.Volumes)
	}
}
