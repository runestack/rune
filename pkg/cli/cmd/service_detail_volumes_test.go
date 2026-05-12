package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// renderServiceVolumes is a pure spec-summary block: no API calls, no
// instance lookups. The contract is that `rune get service <name>`
// surfaces every declared mount with its source (claim or
// claimTemplate) so a developer can see which Volumes back a service
// without typing a second command.

func TestRenderServiceVolumes_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderServiceVolumes(&buf, &types.Service{Name: "x"})
	if buf.Len() != 0 {
		t.Fatalf("no volumes => no output, got %q", buf.String())
	}
}

func TestRenderServiceVolumes_ClaimAndTemplate(t *testing.T) {
	svc := &types.Service{
		Name: "demo",
		Volumes: []types.VolumeMount{
			{
				Name:      "data",
				MountPath: "/var/data",
				Claim:     &types.VolumeClaim{Name: "shared"},
			},
			{
				Name:      "cache",
				MountPath: "/var/cache",
				ReadOnly:  true,
				SubPath:   "shard-0",
				ClaimTemplate: &types.VolumeClaimTemplate{
					StorageClassName: "fast",
					Size:             "5Gi",
					AccessMode:       types.AccessModeRWO,
				},
			},
		},
	}
	var buf bytes.Buffer
	renderServiceVolumes(&buf, svc)
	out := buf.String()
	for _, want := range []string{
		"Volumes (2):",
		"data",
		"/var/data",
		"claim:shared",
		"cache",
		"/var/cache",
		"(ro)",
		"subPath=shard-0",
		"claimTemplate",
		"class=fast",
		"size=5Gi",
		"mode=ReadWriteOnce",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}
