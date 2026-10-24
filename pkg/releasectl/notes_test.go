package releasectl

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const labelNote = "labels: running instances pick this up when they are next replaced (e.g. rune restart)"

func notesService(mut func(*types.Service)) *types.Service {
	s := &types.Service{
		Name: "web", Namespace: "default", Image: "app:v1", Scale: 2,
		Labels: map[string]string{"team": "a"},
		Expose: &types.ServiceExpose{Port: "http", Host: "web.example.com"},
		Ports:  []types.ServicePort{{Name: "http", Port: 80}, {Name: "admin", Port: 81}},
		Volumes: []types.VolumeMount{{Name: "data", MountPath: "/data",
			ClaimTemplate: &types.VolumeClaimTemplate{StorageClassName: "standard", Size: "10Gi", AccessMode: "ReadWriteOnce"}}},
	}
	if mut != nil {
		mut(s)
	}
	return s
}

func TestNextRollNotes(t *testing.T) {
	cases := []struct {
		name   string
		stored func(*types.Service)
		mut    func(*types.Service)
		want   []string
	}{
		{name: "unchanged"},
		{name: "scale only", mut: func(s *types.Service) { s.Scale = 5 }},
		// Ingress reads expose from the live service, and routes to ports[0].
		{name: "expose is read live", mut: func(s *types.Service) { s.Expose.Port = "admin"; s.Expose.Host = "x.example.com" }},
		{name: "spelling out the default pull policy", mut: func(s *types.Service) { s.ImagePull = types.ImagePullAlways }},
		{name: "label", mut: func(s *types.Service) { s.Labels["team"] = "b" }, want: []string{labelNote}},
		{
			name: "label and pull together",
			mut:  func(s *types.Service) { s.Labels = nil; s.ImagePull = types.ImagePullMissing },
			want: []string{"labels, imagePull: running instances pick this up when they are next replaced (e.g. rune restart)"},
		},
		{
			name: "a template change replaces instances, so the label lands now",
			mut:  func(s *types.Service) { s.Labels["team"] = "b"; s.Image = "app:v2" },
		},
		{name: "nothing running before", stored: func(s *types.Service) { s.Scale = 0 }, mut: func(s *types.Service) { s.Labels["team"] = "b" }},
		{name: "nothing running after", mut: func(s *types.Service) { s.Scale = 0; s.Labels["team"] = "b" }},
		{
			name:   "the process runner pulls no image",
			stored: func(s *types.Service) { s.Runtime = types.RuntimeTypeProcess },
			mut:    func(s *types.Service) { s.Runtime = types.RuntimeTypeProcess; s.ImagePull = types.ImagePullNever },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextRollNotes(notesService(tc.stored), notesService(tc.mut)))
		})
	}
}

func claimVolume(name, size string) *types.Volume {
	return &types.Volume{Name: name, Namespace: "default", OwnerService: "default/web",
		StorageClassName: "standard", Size: size, AccessMode: "ReadWriteOnce"}
}

func TestClaimTemplateNotes(t *testing.T) {
	existing := []*types.Volume{claimVolume("data-web-0", "10Gi"), claimVolume("data-web-1", "10Gi")}
	cases := []struct {
		name string
		mut  func(*types.Service)
		vols []*types.Volume
		want []string
	}{
		{name: "matches what exists", vols: existing},
		{name: "no volumes yet", mut: func(s *types.Service) { s.Volumes[0].ClaimTemplate.Size = "20Gi" }},
		{
			name: "resize",
			mut:  func(s *types.Service) { s.Volumes[0].ClaimTemplate.Size = "20Gi" },
			vols: existing,
			want: []string{"volume data: existing volumes stay 10Gi; only replicas without a volume yet get 20Gi"},
		},
		{
			name: "existing volumes of mixed sizes",
			mut:  func(s *types.Service) { s.Volumes[0].ClaimTemplate.Size = "20Gi" },
			vols: []*types.Volume{claimVolume("data-web-0", "10Gi"), claimVolume("data-web-1", "5Gi"), claimVolume("data-web-2", "20Gi")},
			want: []string{"volume data: existing volumes stay 10Gi, 5Gi; only replicas without a volume yet get 20Gi"},
		},
		{
			name: "other template change",
			mut:  func(s *types.Service) { s.Volumes[0].ClaimTemplate.StorageClassName = "fast" },
			vols: existing,
			want: []string{"volume data: the claim template change applies only to replicas without a volume yet"},
		},
		{
			name: "another service's or another mount's volumes",
			mut:  func(s *types.Service) { s.Volumes[0].ClaimTemplate.Size = "20Gi" },
			vols: []*types.Volume{
				{Name: "data-web-0", Namespace: "default", OwnerService: "default/other", Size: "10Gi"},
				claimVolume("logs-web-0", "10Gi"),
				claimVolume("data-web-x", "10Gi"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, claimTemplateNotes(notesService(tc.mut), tc.vols))
		})
	}
}

func TestCast_NotesUpdatesRunningStateWillNotPickUp(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	ref := svcRef("default", "web")
	spec := release.ReleaseSpec{Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: ref}}}

	cast := func(svc *types.Service) *release.Plan {
		t.Helper()
		_, plan, err := c.Cast(ctx, spec, Payloads{Services: map[string]*types.Service{ref.Key(): svc}})
		require.NoError(t, err)
		require.NotNil(t, plan)
		require.Len(t, plan.Changes, 1)
		return plan
	}

	assert.Empty(t, cast(notesService(nil)).Changes[0].Notes, "a created service has nothing running yet")

	second := cast(notesService(func(s *types.Service) { s.Labels["team"] = "b" }))
	assert.Equal(t, release.ActionUpdate, second.Changes[0].Action)
	assert.Equal(t, []string{labelNote}, second.Changes[0].Notes,
		"notes compare against the service as stored before this cast")

	// Notes come from the volumes that exist, not from the stored spec.
	require.NoError(t, c.volumes.Create(ctx, claimVolume("data-web-0", "10Gi")))
	third := cast(notesService(func(s *types.Service) { s.Labels["team"] = "b"; s.Volumes[0].ClaimTemplate.Size = "20Gi" }))
	assert.Equal(t, []string{"volume data: existing volumes stay 10Gi; only replicas without a volume yet get 20Gi"},
		third.Changes[0].Notes)
}

func TestCast_DetachedCastCarriesNotes(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	ref := svcRef("default", "web")
	spec := release.ReleaseSpec{Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: ref}}}
	payload := func(svc *types.Service) Payloads {
		return Payloads{Services: map[string]*types.Service{ref.Key(): svc}}
	}
	_, _, err := c.Cast(ctx, spec, payload(notesService(nil)))
	require.NoError(t, err)

	spec.Detach = true
	_, plan, err := c.Cast(ctx, spec, payload(notesService(func(s *types.Service) { s.Labels["team"] = "b" })))
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	assert.Equal(t, []string{labelNote}, plan.Changes[0].Notes)
}
