package cmd

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/runestack/rune/pkg/cli/utils"
	"github.com/spf13/cobra"
)

var (
	packOutput   string
	packWithHash bool
)

var packCmd = &cobra.Command{
	Use:   "pack <runeset-dir>",
	Short: "Package a runeset directory into a .runeset.tgz archive",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := args[0]
		// Validate runeset
		if !utils.IsDirectory(root) {
			return fmt.Errorf("not a directory: %s", root)
		}
		if !utils.FileExists(filepath.Join(root, "runeset.yaml")) {
			return fmt.Errorf("runeset.yaml not found in %s", root)
		}
		if !utils.IsDirectory(filepath.Join(root, "casts")) {
			return fmt.Errorf("casts/ directory required in %s", root)
		}
		out := packOutput
		if out == "" {
			base := filepath.Base(filepath.Clean(root))
			out = base + ".runeset.tgz"
		}
		if err := tarGzDir(root, out); err != nil {
			return err
		}
		fmt.Println("📦 Created:", out)
		if packWithHash {
			// Best-effort sha256
			if err := writeSHA256(out); err == nil {
				fmt.Println("🔐 Wrote:", out+".sha256")
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(packCmd)
	packCmd.Flags().StringVarP(&packOutput, "output", "o", "", "Output archive path (defaults to <dir>.runeset.tgz)")
	packCmd.Flags().BoolVar(&packWithHash, "sha256", false, "Also write <output>.sha256 with checksum")
}

func tarGzDir(root, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gzw := gzip.NewWriter(f)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		head := &tar.Header{Name: rel, ModTime: info.ModTime(), Mode: int64(info.Mode())}
		if info.IsDir() {
			head.Typeflag = tar.TypeDir
			head.Size = 0
			return tw.WriteHeader(head)
		}
		head.Typeflag = tar.TypeReg
		head.Size = info.Size()
		if err := tw.WriteHeader(head); err != nil {
			return err
		}
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(tw, r)
		return err
	})
}

func writeSHA256(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	return os.WriteFile(path+".sha256", []byte(sum+"  "+filepath.Base(path)+"\n"), 0o600)
}
