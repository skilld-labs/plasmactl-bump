package sync

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// CompareDirs takes two directory paths as inputs and returns a slice of different files between them.
func CompareDirs(dirA, dirB string, excludeSubDirs []string) ([]string, error) {
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
			updated = append(updated, f)
			continue
		}
		fileA := filepath.Join(dirA, f)
		fileB := filepath.Join(dirB, f)

		if !fileEqual(fileA, fileB) {
			updated = append(updated, f)
		}
	}

	updated = append(updated, mapKeyDiff(filesInDirB, filesInDirA)...)

	return updated, nil
}

func mapKeyDiff(m1, m2 map[string]bool) []string {
	var diff []string
	for key := range m1 {
		if !m2[key] {
			diff = append(diff, key)
		}
	}
	return diff
}

func getFiles(path string, excludeSubDirs []string) (map[string]bool, error) {
	path = ensureTrailingSlash(path)
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

func ensureTrailingSlash(s string) string {
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
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

// HashString is wrapper for hashing string.
func HashString(item string) uint64 {
	return xxhash.Sum64String(item)
}
