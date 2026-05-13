package types

import (
	"strings"
	"testing"
)

// TestParseCastFile_StorageClassesAndVolumes verifies that the cast-file
// parser recognises the new top-level storage keys (RUNE-072) in both
// singular and plural forms and produces the expected concrete objects via
// GetStorageClasses / GetVolumes.
func TestParseCastFile_StorageClassesAndVolumes(t *testing.T) {
	t.Parallel()

	yamlContent := `
storageclass:
  name: fast-ssd
  driver: do-volume
  reclaimPolicy: delete
  default: true
  parameters:
    fsType: ext4
volumes:
  - name: data
    namespace: app
    storageClassName: fast-ssd
    size: 10Gi
    accessMode: ReadWriteOnce
  - name: cache
    storageClassName: fast-ssd
    size: 5Gi
`

	cf, err := ParseCastFileFromBytes([]byte(yamlContent), "")
	if err != nil {
		t.Fatalf("ParseCastFileFromBytes: %v", err)
	}
	if cf.HasParseErrors() {
		t.Fatalf("parse errors: %v", cf.GetParseErrors())
	}

	classes, err := cf.GetStorageClasses()
	if err != nil {
		t.Fatalf("GetStorageClasses: %v", err)
	}
	if len(classes) != 1 {
		t.Fatalf("want 1 storageclass, got %d", len(classes))
	}
	sc := classes[0]
	if sc.Name != "fast-ssd" || sc.Driver != "do-volume" || !sc.Default {
		t.Fatalf("unexpected storageclass: %+v", sc)
	}
	if sc.ReclaimPolicy != ReclaimPolicyDelete {
		t.Fatalf("reclaim policy: got %q", sc.ReclaimPolicy)
	}
	if sc.Parameters["fsType"] != "ext4" {
		t.Fatalf("parameters: %+v", sc.Parameters)
	}

	vols, err := cf.GetVolumes()
	if err != nil {
		t.Fatalf("GetVolumes: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("want 2 volumes, got %d", len(vols))
	}
	if vols[0].Name != "data" || vols[0].Namespace != "app" || vols[0].Size != "10Gi" {
		t.Fatalf("unexpected volume[0]: %+v", vols[0])
	}
	if vols[0].AccessMode != AccessModeRWO {
		t.Fatalf("access mode: %q", vols[0].AccessMode)
	}
	// vols[1] omits namespace + accessMode → should default
	if vols[1].Namespace != "default" {
		t.Fatalf("default namespace: got %q", vols[1].Namespace)
	}
	if vols[1].AccessMode != AccessModeRWO {
		t.Fatalf("default access mode: got %q", vols[1].AccessMode)
	}
}

// TestParseCastFile_StorageClassPlural verifies the plural `storageclasses:`
// sequence form parses identically to the singular form.
func TestParseCastFile_StorageClassPlural(t *testing.T) {
	t.Parallel()
	yamlContent := `
storageclasses:
  - name: gold
    driver: local
  - name: silver
    driver: local-host
`
	cf, err := ParseCastFileFromBytes([]byte(yamlContent), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	classes, err := cf.GetStorageClasses()
	if err != nil {
		t.Fatalf("GetStorageClasses: %v", err)
	}
	if len(classes) != 2 || classes[0].Name != "gold" || classes[1].Name != "silver" {
		t.Fatalf("unexpected classes: %+v", classes)
	}
}

// TestCastFile_VolumeRequiredFields checks that GetVolumes surfaces the
// required-field validations (name / storageClassName / size).
func TestCastFile_VolumeRequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "missing-storage-class",
			yaml:    "volume:\n  name: v1\n  size: 1Gi\n",
			wantSub: "storageClassName is required",
		},
		{
			name:    "missing-size",
			yaml:    "volume:\n  name: v1\n  storageClassName: fast\n",
			wantSub: "size is required",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cf, err := ParseCastFileFromBytes([]byte(tc.yaml), "")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = cf.GetVolumes()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

// TestCastFile_OverrideNamespaceAppliesToVolumes verifies the cast-file
// override namespace replaces a volume's spec namespace.
func TestCastFile_OverrideNamespaceAppliesToVolumes(t *testing.T) {
	t.Parallel()
	yamlContent := `
volume:
  name: data
  namespace: dev
  storageClassName: fast
  size: 1Gi
`
	cf, err := ParseCastFileFromBytes([]byte(yamlContent), "prod")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	vols, err := cf.GetVolumes()
	if err != nil {
		t.Fatalf("GetVolumes: %v", err)
	}
	if len(vols) != 1 || vols[0].Namespace != "prod" {
		t.Fatalf("override namespace not applied: %+v", vols)
	}
}
