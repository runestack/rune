package service

import (
	"bytes"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// secretSentinels are the values seeded into every field of the instance
// record that can hold resolved secret material. None of them may appear
// anywhere in the serialized proto.
var secretSentinels = []string{
	"env-interp-plaintext",   // Environment, from {{secret:...}} interpolation
	"env-from-plaintext",     // Environment, folded in from envFrom secret data
	"mounted-secret-payload", // Metadata.SecretMounts[].Data
	"mounted-config-payload", // Metadata.ConfigmapMounts[].Data
}

func secretBearingInstance() *types.Instance {
	return &types.Instance{
		ID:          "i-1",
		Namespace:   "default",
		Name:        "api-abc12",
		ServiceID:   "s-1",
		ServiceName: "api",
		NodeID:      "node-1",
		Runner:      types.RunnerType("docker"),
		Status:      types.InstanceStatusRunning,
		CreatedAt:   time.Unix(1, 0).UTC(),
		UpdatedAt:   time.Unix(2, 0).UTC(),
		Environment: map[string]string{
			"DB_PASSWORD": "env-interp-plaintext",
			"BULK_TOKEN":  "env-from-plaintext",
			"HARMLESS":    "plain",
		},
		Metadata: &types.InstanceMetadata{
			ServiceGeneration: 1,
			SecretMounts: []types.ResolvedSecretMount{{
				Name:      "creds",
				MountPath: "/etc/creds",
				Data:      map[string]string{"API_KEY": "mounted-secret-payload"},
			}},
			ConfigmapMounts: []types.ResolvedConfigmapMount{{
				Name:      "appcfg",
				MountPath: "/etc/appcfg",
				Data:      map[string]string{"CONNECTION": "mounted-config-payload"},
			}},
		},
	}
}

// TestInstanceModelToProto_DropsSecretMaterial is the regression guard for the
// readonly-reads-every-secret bug: an instance record carries fully-resolved
// secret values (Environment folds in envFrom secret data and {{secret:...}}
// interpolation; Metadata.SecretMounts carries mounted secret payloads), and
// GetInstance/ListInstances are authorized by instances:get/list — a strictly
// weaker grant than the secrets:reveal verb that gates the same plaintext on
// SecretService. Serving any of it inverts that boundary.
//
// The assertion is on the serialized message rather than on named fields, so
// wiring any of these payloads onto a new proto field fails here too.
//
// Only the two Environment sentinels regressed; the mount payloads have never
// been on the wire and are pinned here so they stay that way. Green on those
// two is a pin holding, not a bug fixed.
func TestInstanceModelToProto_DropsSecretMaterial(t *testing.T) {
	s := NewInstanceService(nil, nil, log.NewLogger())

	protoInstance, err := s.instanceModelToProto(secretBearingInstance())
	require.NoError(t, err)

	jsonWire, err := protojson.Marshal(protoInstance)
	require.NoError(t, err)
	binWire, err := proto.Marshal(protoInstance)
	require.NoError(t, err)

	for _, sentinel := range secretSentinels {
		require.NotContains(t, string(jsonWire), sentinel,
			"resolved secret material must not reach the instance wire form")
		require.False(t, bytes.Contains(binWire, []byte(sentinel)),
			"resolved secret material must not reach the instance wire form")
	}

	// Non-secret fields still travel, so the redaction is not just an empty
	// message.
	require.Equal(t, "api-abc12", protoInstance.Name)
	require.Equal(t, int32(1), protoInstance.Metadata.Generation)
}
