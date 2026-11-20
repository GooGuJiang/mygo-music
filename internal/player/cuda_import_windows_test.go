//go:build windows && amd64

package player

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Only pinned CUDA files are extracted; extra paths and changed bytes are rejected.
func TestExtractDemucsCUDA(t *testing.T) {
	const name, content = "worker.dll", "fixed runtime bytes"
	allowed := map[string]cudaFile{name: {sum: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), size: uint64(len(content))}}
	for _, tt := range []struct {
		name, entry, data string
		valid             bool
	}{
		{"valid", cudaZipDir + name, content, true},
		{"traversal", cudaZipDir + "../" + name, content, false},
		{"wrong hash", cudaZipDir + name, "X" + content[1:], false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "cuda.zip")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(file)
			writer, err := archive.Create(tt.entry)
			if err == nil {
				_, err = io.WriteString(writer, tt.data)
			}
			closeErr, fileErr := archive.Close(), file.Close()
			if err != nil || closeErr != nil || fileErr != nil {
				t.Fatalf("writing archive: %v, %v, %v", err, closeErr, fileErr)
			}
			dest := filepath.Join(root, "dest")
			if err := os.Mkdir(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := extractDemucsCUDA(path, dest, allowed); (err == nil) != tt.valid {
				t.Fatalf("extract result: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Fatalf("wrote outside destination: %v", err)
			}
			if tt.valid && !matchesCUDAFile(filepath.Join(dest, name), allowed[name]) {
				t.Fatal("extracted file changed")
			}
		})
	}
}
