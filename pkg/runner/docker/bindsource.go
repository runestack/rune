package docker

import (
	"fmt"
	"path/filepath"
)

// bindSource returns the host path to bind for a volume mount with an
// optional subPath. The subPath's components live inside the volume, so
// the container can make any of them a symlink; Docker follows symlinks
// when it binds. The joined path is therefore resolved and must stay
// inside the resolved volume source, and the resolved path is what gets
// bound.
//
// A container that swaps a component for a symlink between this check
// and the bind can still win the race; closing that needs the bind to be
// made from an open file descriptor, which the Docker API cannot express.
func bindSource(source, subPath string) (string, error) {
	if subPath == "" {
		return source, nil
	}
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve volume source %q: %w", source, err)
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, subPath))
	if err != nil {
		return "", fmt.Errorf("resolve subPath %q: %w", subPath, err)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || (len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("subPath %q resolves to %q, outside the volume", subPath, p)
	}
	return p, nil
}
