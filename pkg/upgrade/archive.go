package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// untarBinaries extracts exactly the named top-level regular files from a
// gzipped tarball into the given destinations (0755). Anything else in the
// archive — directories, symlinks, path-traversal names — is ignored, and
// a missing wanted entry is an error.
func untarBinaries(tarball string, want map[string]string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.Clean(hdr.Name))
		dst, ok := want[name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxArtifactBytes)); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		found[name] = true
	}
	for name := range want {
		if !found[name] {
			return fmt.Errorf("tarball is missing %q", name)
		}
	}
	return nil
}

func untarBinary(tarball, name, dst string) error {
	return untarBinaries(tarball, map[string]string{name: dst})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyExecutable(src, dst string) error { return copyFile(src, dst, 0o755) }
