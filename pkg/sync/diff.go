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
	filesInDirA, err := GetFiles(dirA, excludeSubDirs)
	if err != nil {
		return nil, err
	}
	filesInDirB, err := GetFiles(dirB, excludeSubDirs)
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

		areEqual, err := fileEqual(fileA, fileB)
		if err != nil {
			return nil, err
		}
		if !areEqual {
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

// GetFiles returns list of files as map.
func GetFiles(path string, excludeSubDirs []string) (map[string]bool, error) {
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

func fileEqual(fileA, fileB string) (bool, error) {
	hashA, err := HashFileByPath(fileA)
	if err != nil {
		return false, err
	}
	hashB, err := HashFileByPath(fileB)
	if err != nil {
		return false, err
	}

	return hashA == hashB, nil
}

// HashFileByPath opens file and hash content.
func HashFileByPath(path string) (uint64, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, err
	}
	defer file.Close()

	hash := xxhash.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return 0, err
	}

	return hash.Sum64(), nil
}

// HashFileFromReader uses reader to copy and hash file.
func HashFileFromReader(reader io.ReadCloser) (uint64, error) {
	hash := xxhash.New()
	_, err := io.Copy(hash, reader)
	if err != nil {
		return 0, err
	}

	return hash.Sum64(), nil
}

// HashString is wrapper for hashing string.
func HashString(item string) uint64 {
	return xxhash.Sum64String(item)
}
