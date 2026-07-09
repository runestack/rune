package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// staleContainerID is a syntactically valid but nonexistent container ID,
// simulating a record whose instance→container mapping drifted (the
// probed-services churn bug: reconciler saw "No such container" for a
// live, healthy container and recreated it every pass, forever).
const staleContainerID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// TestStatusHealsStaleContainerMapping verifies that Status re-resolves a
// stale ContainerID through the rune.instance.id label, repairs the
// in-memory mapping (ContainerID + ContainerIP), and reports the live
// container's status instead of a not-found error.
func TestStatusHealsStaleContainerMapping(t *testing.T) {
	skipIfDockerUnavailable(t)

	namespace := "test-heal-" + time.Now().Format("20060102150405")
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{DisableColors: true}),
	)
	r, err := NewDockerRunner(logger)
	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	instance := &runetypes.Instance{
		// Unique per run: the label lookup matches rune.instance.id, so a
		// reused ID could resolve a leftover container from a prior run.
		ID:        "heal-" + time.Now().Format("20060102150405.000"),
		Namespace: namespace,
		Name:      "heal-target",
		ServiceID: "test-service",
		NodeID:    "test-node",
		Metadata: &runetypes.InstanceMetadata{
			Image:     "alpine:latest",
			ImagePull: runetypes.ImagePullMissing,
		},
		Exec: &runetypes.Exec{
			Command: []string{"sleep", "30"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer r.Remove(ctx, instance, true)

	if err := r.Create(ctx, instance); err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}
	if err := r.Start(ctx, instance); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	realID := instance.ContainerID
	if realID == "" {
		t.Fatal("Container ID was not set after Create")
	}

	// Corrupt the mapping the way the prod bug manifested: a non-empty
	// ContainerID that no longer inspects, plus a stale IP the health
	// probes would dial into a timeout.
	instance.ContainerID = staleContainerID
	instance.Metadata.ContainerIP = "203.0.113.7" // TEST-NET; unroutable

	status, err := r.Status(ctx, instance)
	if err != nil {
		t.Fatalf("Status did not heal the stale mapping: %v", err)
	}
	if status != runetypes.InstanceStatusRunning {
		t.Fatalf("expected Running after heal, got %s", status)
	}
	if instance.ContainerID != realID {
		t.Fatalf("ContainerID not healed: got %s, want %s", instance.ContainerID, realID)
	}
	if instance.Metadata.ContainerIP == "" || instance.Metadata.ContainerIP == "203.0.113.7" {
		t.Fatalf("ContainerIP not refreshed from live container: got %q", instance.Metadata.ContainerIP)
	}
}

// TestStatusStaleMappingContainerGone verifies the heal does NOT invent a
// container: when the labeled container is genuinely gone, Status still
// returns not-found so the reconciler's recreate stays correct.
func TestStatusStaleMappingContainerGone(t *testing.T) {
	skipIfDockerUnavailable(t)

	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{DisableColors: true}),
	)
	r, err := NewDockerRunner(logger)
	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	instance := &runetypes.Instance{
		ID:          "gone-" + time.Now().Format("20060102150405.000"),
		Namespace:   "test-heal-gone",
		Name:        "gone-target",
		ContainerID: staleContainerID,
		Metadata:    &runetypes.InstanceMetadata{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err = r.Status(ctx, instance)
	if err == nil {
		t.Fatal("expected error for a stale ID with no labeled container, got nil")
	}
	if !strings.Contains(err.Error(), "no container found for instance ID") {
		t.Fatalf("expected label-lookup miss error, got: %v", err)
	}
	// The stale ID must be left in place — only a confirmed live
	// container may rewrite the mapping.
	if instance.ContainerID != staleContainerID {
		t.Fatalf("ContainerID mutated without a live container: %s", instance.ContainerID)
	}
}
