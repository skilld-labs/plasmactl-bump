package plasmactlbump

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cespare/xxhash/v2"
)

var excludeSubDirs = []string{
	".git",
	".compose",
	".plasmactl",
	".gitlab-ci.yml",
	"ansible_collections",
	".gitlab-ci.yml",
	"scripts/ci/.gitlab-ci.platform.yaml",
	"venv",
}

// GetDiffFiles takes two directory paths as inputs and returns a slice of updated file paths
// and an error. It compares the files in the two directories (excluding subdirectories
// specified in the excludeSubDirs variable) and checks if they are equal. If a file is
// modified, its path is added to the updated slice.
func GetDiffFiles(dirA, dirB string) ([]string, error) {
	updated, err := compareDirectories(dirA, dirB, excludeSubDirs)
	return updated, err
}

func compareDirectories(dirA, dirB string, excludeSubDirs []string) ([]string, error) {
	filesInDirA, err := getFiles(dirA, excludeSubDirs)
	if err != nil {
		return nil, err
	}
	filesInDirB, err := getFiles(dirB, excludeSubDirs)
	if err != nil {
		return nil, err
	}

	var updated []string

	for f := range filesInDirA {
		_, found := filesInDirB[f]
		if !found {
			continue
		}
		fileA := filepath.Join(dirA, f)
		fileB := filepath.Join(dirB, f)

		if !fileEqual(fileA, fileB) {
			updated = append(updated, f)
		}
	}

	return updated, nil
}

func getFiles(path string, excludeSubDirs []string) (map[string]bool, error) {
	files := make(map[string]bool)
	err := filepath.Walk(path, func(fpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath := strings.TrimPrefix(fpath, path)
		// skip symlinks
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return nil
		}

		// exclude subdirectories
		for _, d := range excludeSubDirs {
			if strings.Contains(relPath, d) {
				return nil
			}
		}

		files[relPath] = true
		return nil
	})

	return files, err
}

func fileEqual(fileA, fileB string) bool {
	hashA := hashFile(fileA)
	hashB := hashFile(fileB)
	return hashA == hashB
}

func hashFile(path string) uint64 {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		panic(err)
	}
	defer file.Close()

	hash := xxhash.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		panic(err)
	}

	return hash.Sum64()
}

func hashString(item string) uint64 {
	return xxhash.Sum64String(item)
}
