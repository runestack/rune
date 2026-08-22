// Secret/configmap mount materialization. Split from runner.go
// (RUNE-312).

package docker

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/runestack/rune/pkg/types"
)

// mountMaterializer turns resolved secret/config mounts into host-side
// files and docker mount.Mount entries (RUNE-312 Phase 2). Stateless:
// constructed from the four DockerConfig FileMode fields alone, so the
// runner's mounter() can derive one lazily and bare-struct tests keep
// working. Zero-valued modes fall back to the historical defaults at
// the use sites.
type mountMaterializer struct {
	secretDirMode  os.FileMode
	secretFileMode os.FileMode
	configDirMode  os.FileMode
	configFileMode os.FileMode
}

func newMountMaterializer(cfg *DockerConfig) *mountMaterializer {
	if cfg == nil {
		return &mountMaterializer{}
	}
	return &mountMaterializer{
		secretDirMode:  cfg.SecretDirMode,
		secretFileMode: cfg.SecretFileMode,
		configDirMode:  cfg.ConfigDirMode,
		configFileMode: cfg.ConfigFileMode,
	}
}

// mounter returns the wired materializer, deriving one from config for
// bare-struct callers (it is stateless, so this is equivalent).
func (r *DockerRunner) mounter() *mountMaterializer {
	if r.mounts != nil {
		return r.mounts
	}
	return newMountMaterializer(r.config)
}

// Thin delegators: init.go and config translation call these on the
// runner; the logic lives on mountMaterializer.
func (r *DockerRunner) prepareSecretMounts(secretMounts []types.ResolvedSecretMount) ([]mount.Mount, error) {
	return r.mounter().prepareSecretMounts(secretMounts)
}

func (r *DockerRunner) prepareConfigmapsMounts(configMounts []types.ResolvedConfigmapMount) ([]mount.Mount, error) {
	return r.mounter().prepareConfigmapsMounts(configMounts)
}

// prepareSecretMounts creates temporary files and Docker mounts for secret mounts
func (m *mountMaterializer) prepareSecretMounts(secretMounts []types.ResolvedSecretMount) ([]mount.Mount, error) {
	var mounts []mount.Mount

	for _, secretMount := range secretMounts {
		// Create a temporary directory for this mount
		tempDir, err := os.MkdirTemp("", fmt.Sprintf("rune-secret-%s-", secretMount.Name))
		if err != nil {
			return nil, fmt.Errorf("failed to create temp directory for secret mount %s: %w", secretMount.Name, err)
		}
		// Adjust directory permissions to allow Docker Desktop to stat/bind mount
		// Keep files themselves locked down (0600) while directory is world-executable for traversal
		dirMode := m.secretDirMode
		if dirMode == 0 {
			dirMode = 0o755
		}
		_ = os.Chmod(tempDir, dirMode)

		// Create files for each secret key
		for key, value := range secretMount.Data {
			// Determine the file path
			var filePath string
			if len(secretMount.Items) > 0 {
				// Check if there's a specific path mapping for this key
				for _, item := range secretMount.Items {
					if item.Key == key {
						filePath = filepath.Join(tempDir, item.Path)
						break
					}
				}
				// If no specific mapping, use the key name
				if filePath == "" {
					filePath = filepath.Join(tempDir, key)
				}
			} else {
				// No specific mapping, use the key name
				filePath = filepath.Join(tempDir, key)
			}

			// Ensure subdirectories exist if path contains directories
			// Use 0755 so the container user (often not the host owner due to Docker Desktop FUSE) can traverse
			parentMode := m.secretDirMode
			if parentMode == 0 {
				parentMode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(filePath), parentMode); err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("failed to create directory for secret file %s: %w", filePath, err)
			}
			// Create the file with the secret value (decode base64 if applicable)
			fileMode := m.secretFileMode
			if fileMode == 0 {
				fileMode = 0o444
			}
			data := []byte(value)
			if decoded, ok := decodeIfBase64(value); ok {
				data = decoded
			}
			if err := os.WriteFile(filePath, data, fileMode); err != nil {
				os.RemoveAll(tempDir) // Clean up on error
				return nil, fmt.Errorf("failed to write secret file %s: %w", filePath, err)
			}
		}

		// Create Docker mount
		dockerMount := mount.Mount{
			Type:        mount.TypeBind,
			Source:      tempDir,
			Target:      secretMount.MountPath,
			ReadOnly:    true,
			BindOptions: &mount.BindOptions{},
		}

		mounts = append(mounts, dockerMount)
	}

	return mounts, nil
}

// decodeIfBase64 attempts to decode s as standard base64 if it looks like base64 content.
// Returns (decoded, true) when decoding is performed, otherwise (nil, false).
func decodeIfBase64(s string) ([]byte, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || len(trimmed)%4 != 0 {
		return nil, false
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			continue
		}
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// prepareConfigmapsMounts creates temporary files and Docker mounts for config mounts
func (m *mountMaterializer) prepareConfigmapsMounts(configMounts []types.ResolvedConfigmapMount) ([]mount.Mount, error) {
	var mounts []mount.Mount

	for _, configMount := range configMounts {
		// Create a temporary directory for this mount
		tempDir, err := os.MkdirTemp("", fmt.Sprintf("rune-config-%s-", configMount.Name))
		if err != nil {
			return nil, fmt.Errorf("failed to create temp directory for config mount %s: %w", configMount.Name, err)
		}
		// Adjust directory permissions to allow Docker Desktop to stat/bind mount
		dirMode := m.configDirMode
		if dirMode == 0 {
			dirMode = 0o755
		}
		_ = os.Chmod(tempDir, dirMode)

		// Create files for each config key
		for key, value := range configMount.Data {
			// Determine the file path
			var filePath string
			if len(configMount.Items) > 0 {
				// Check if there's a specific path mapping for this key
				for _, item := range configMount.Items {
					if item.Key == key {
						filePath = filepath.Join(tempDir, item.Path)
						break
					}
				}
				// If no specific mapping, use the key name
				if filePath == "" {
					filePath = filepath.Join(tempDir, key)
				}
			} else {
				// No specific mapping, use the key name
				filePath = filepath.Join(tempDir, key)
			}

			// Ensure subdirectories exist if path contains directories
			// Use 0755 so the container user can traverse
			parentMode := m.configDirMode
			if parentMode == 0 {
				parentMode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(filePath), parentMode); err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("failed to create directory for config file %s: %w", filePath, err)
			}
			// Create the file with the config value
			fileMode := m.configFileMode
			if fileMode == 0 {
				fileMode = 0o644
			}
			if err := os.WriteFile(filePath, []byte(value), fileMode); err != nil {
				os.RemoveAll(tempDir) // Clean up on error
				return nil, fmt.Errorf("failed to write config file %s: %w", filePath, err)
			}
		}

		// Create Docker mount
		dockerMount := mount.Mount{
			Type:        mount.TypeBind,
			Source:      tempDir,
			Target:      configMount.MountPath,
			ReadOnly:    true,
			BindOptions: &mount.BindOptions{},
		}

		mounts = append(mounts, dockerMount)
	}

	return mounts, nil
}
