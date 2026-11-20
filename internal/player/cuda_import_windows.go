//go:build windows && amd64

package player

import (
	"archive/zip"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/egoist/mygo"
)

const (
	cudaZipDir      = "demucs/native-v1/cuda/"
	demucsModelName = "htdemucs-f16.gguf"
)

type cudaFile struct {
	sum  string
	size uint64
}

var cudaModel = cudaFile{sum: "4d285d7a9e98067ecc15dcaaf1646e452fc9574ae4668298ac091cb6d8b5c9ee", size: 84059168}

//go:embed cuda-runtime-v1.txt
var cudaManifest string

var cudaImports sync.Mutex

var cudaFiles = sync.OnceValue(func() map[string]cudaFile {
	files := make(map[string]cudaFile)
	for _, line := range strings.Split(strings.TrimSpace(cudaManifest), "\n") {
		fields := strings.Fields(line)
		size, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || len(fields[0]) != sha256.Size*2 {
			panic("invalid embedded CUDA file manifest")
		}
		files[fields[2]] = cudaFile{sum: fields[0], size: size}
	}
	return files
})

func importedDemucsRuntime() (string, error) {
	data, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "demucs", "cuda-runtime-v1"), nil
}

// ImportDemucsCUDA verifies a release CUDA archive before making it available to the worker.
func ImportDemucsCUDA(path string) error {
	cudaImports.Lock()
	defer cudaImports.Unlock()
	worker := findDemucs()
	if worker == "" {
		return errDemucsMissing
	}
	dest, err := importedDemucsRuntime()
	if err != nil {
		return err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".cuda-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	cuda := filepath.Join(stage, "cuda")
	if err := os.Mkdir(cuda, 0o700); err != nil {
		return err
	}
	if err := extractDemucsCUDA(path, cuda, cudaFiles()); err != nil {
		return err
	}
	// The worker opens the shared separation model relative to its runtime directory.
	model, err := os.Open(filepath.Join(filepath.Dir(worker), "native-v1", demucsModelName))
	if err != nil {
		return err
	}
	defer model.Close()
	if err := writeCUDAFile(filepath.Join(stage, demucsModelName), model, cudaModel); err != nil {
		return err
	}
	return installCUDARuntime(stage, dest)
}

func installCUDARuntime(stage, dest string) error {
	err := os.Rename(stage, dest)
	if err == nil || cudaRuntimeMatches(dest) {
		return nil // Reimporting the same package leaves a running worker's files in place.
	}
	if info, statErr := os.Lstat(dest); statErr != nil || !info.IsDir() {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(dest), ".cuda-previous-")
	if err != nil {
		return err
	}
	previous := filepath.Join(backup, "runtime")
	if err := os.Rename(dest, previous); err != nil {
		os.Remove(backup)
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		if restoreErr := os.Rename(previous, dest); restoreErr != nil {
			return fmt.Errorf("could not install CUDA runtime: %v; previous runtime saved at %s: %w", err, previous, restoreErr)
		}
		os.Remove(backup)
		return err
	}
	os.RemoveAll(backup)
	return nil
}

func extractDemucsCUDA(path, dest string, allowed map[string]cudaFile) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) > len(allowed)+1 {
		return fmt.Errorf("CUDA package has unexpected entries")
	}
	entries := make(map[string]*zip.File, len(allowed))
	for _, file := range archive.File {
		if file.Name == cudaZipDir && file.FileInfo().IsDir() {
			continue
		}
		name, inside := strings.CutPrefix(file.Name, cudaZipDir)
		want, known := allowed[name]
		if !inside || !known || !file.Mode().IsRegular() || file.UncompressedSize64 != want.size || entries[name] != nil {
			return fmt.Errorf("unexpected file in CUDA package: %q", file.Name)
		}
		entries[name] = file
	}
	if len(entries) != len(allowed) {
		return fmt.Errorf("CUDA package is missing required files")
	}
	for name, entry := range entries {
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeCUDAFile(filepath.Join(dest, name), reader, allowed[name])
		reader.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeCUDAFile(path string, src io.Reader, want cudaFile) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(src, int64(want.size)+1))
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if uint64(n) != want.size || fmt.Sprintf("%x", hash.Sum(nil)) != want.sum {
		return fmt.Errorf("CUDA file failed verification: %s", filepath.Base(path))
	}
	return nil
}

func cudaRuntimeMatches(dest string) bool {
	cuda := filepath.Join(dest, "cuda")
	files, err := os.ReadDir(cuda)
	allowed := cudaFiles()
	if err != nil || len(files) != len(allowed) {
		return false
	}
	for _, file := range files {
		want, ok := allowed[file.Name()]
		if !ok || !file.Type().IsRegular() || !matchesCUDAFile(filepath.Join(cuda, file.Name()), want) {
			return false
		}
	}
	return matchesCUDAFile(filepath.Join(dest, demucsModelName), cudaModel)
}

func matchesCUDAFile(path string, want cudaFile) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, int64(want.size)+1))
	return err == nil && uint64(n) == want.size && fmt.Sprintf("%x", hash.Sum(nil)) == want.sum
}
