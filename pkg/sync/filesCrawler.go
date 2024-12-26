package sync

import (
	"os"
	"path/filepath"
	"strings"
)

const roles = "roles"

// FilesCrawler is a type that represents a crawler for resources in a given directory.
type FilesCrawler struct {
	rootDir string
}

// NewFilesCrawler creates a new instance of FilesCrawler with initialized taskSources and templateSources maps.
func NewFilesCrawler(directory string) *FilesCrawler {
	return &FilesCrawler{
		rootDir: directory,
	}
}

// FindVarsFiles return list of variables files in platform.
// If platform is empty, search across all.
func (cr *FilesCrawler) FindVarsFiles(platform string) (map[string][]string, error) {
	partsCount := 3
	platformPart := 0
	rolePart := 0
	kindPart := 1

	files := make(map[string][]string)
	dir := filepath.Join(cr.rootDir, platform)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath := strings.TrimPrefix(path, cr.rootDir+"/")
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if strings.Contains(path, "scripts") {
			return filepath.SkipDir
		}

		if info.IsDir() {
			return nil
		}

		parts := strings.Split(relPath, "/")
		if len(parts) > partsCount && (platform == "" || parts[platformPart] == platform) &&
			(rolePart == 0 || parts[rolePart] == roles) {

			if parts[kindPart] == "group_vars" {
				filename := filepath.Base(path)
				if filename == "vars.yaml" || filename == vaultFile {
					files[parts[platformPart]] = append(files[parts[platformPart]], relPath)
				}
			}
		}

		return nil
	})

	return files, err
}

// FindResourcesFiles return list of resources files in platform.
// If platform is empty, search across all.
func (cr *FilesCrawler) FindResourcesFiles(platform string) (map[string][]string, error) {
	partsCount := 4
	platformPart := 0
	rolePart := 2
	kindPart := 4

	files := make(map[string][]string)
	dir := filepath.Join(cr.rootDir, platform)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath := strings.TrimPrefix(path, cr.rootDir+"/")
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if strings.Contains(path, "scripts") {
			return filepath.SkipDir
		}

		if info.IsDir() {
			return nil
		}

		parts := strings.Split(relPath, "/")
		if len(parts) > partsCount && (platform == "" || parts[platformPart] == platform) &&
			(rolePart == 0 || parts[rolePart] == roles) {

			if parts[kindPart] == "templates" {
				ext := filepath.Ext(path)
				if ext != ".j2" {
					return nil
				}
				files[parts[platformPart]] = append(files[parts[platformPart]], relPath)

			} else if parts[kindPart] == "tasks" && filepath.Base(path) == "configuration.yaml" {
				files[parts[platformPart]] = append(files[parts[platformPart]], relPath)
			}
		}

		return nil
	})

	return files, err
}
