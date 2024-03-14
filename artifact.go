package plasmactlbump

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/launchrctl/launchr/pkg/cli"
	"github.com/launchrctl/launchr/pkg/log"
)

const (
	artifactsRepositoryDomain = "https://repositories.skilld.cloud"
	artifactsPath             = ".compose/artifacts"
	bumpSearchText            = "versions bump"
	dirPermissions            = 0755
)

// ArtifactStorage represents a storage for artifacts.
type ArtifactStorage struct {
	repo     *BumperRepo
	username string
	password string
	override string
}

// PrepareComparisonArtifact prepares the artifact for comparison by downloading and extracting it into the specified directory.
func (s *ArtifactStorage) PrepareComparisonArtifact(comparisonDir string) error {
	repoName, err := s.repo.GetRepoName()
	if err != nil {
		return err
	}

	log.Info("Repository name: %s", repoName)
	var comparisonRef string
	if s.override != "" {
		comparisonRef = s.override
		log.Info("OVERRIDDEN_COMPARISON_REF has been set: %s", s.override)
	} else {
		comparisonRef, err = s.repo.GetComparisonRef(bumpSearchText)
		if err != nil {
			return err
		}
		log.Info("Last bump commit identified: %s", comparisonRef)
	}

	artifactFile := fmt.Sprintf("%s-%s-plasma-src.tar.gz", repoName, comparisonRef)
	artifactPath := filepath.Join(artifactsPath, artifactFile)

	err = s.downloadArtifact(s.username, s.password, artifactFile, artifactPath, repoName)
	if err != nil {
		return err
	}

	cli.Println("Processing...")
	err = s.prepareComparisonDir(comparisonDir)
	if err != nil {
		return err
	}
	_, err = s.unarchiveTar(artifactPath, comparisonDir)
	if err != nil {
		return err
	}

	return nil
}

func (s *ArtifactStorage) prepareComparisonDir(path string) error {
	err := os.MkdirAll(path, dirPermissions)
	if err != nil {
		return err
	}
	err = os.RemoveAll(path)
	if err != nil {
		return err
	}
	return nil
}

func (s *ArtifactStorage) downloadArtifact(username, password, artifactFile, artifactPath, repo string) error {
	cli.Println("Attempting to get %s from local storage", artifactFile)
	_, errExists := os.Stat(artifactPath)
	if errExists == nil {
		cli.Println("Local artifact found")
		return nil
	}

	cli.Println("Local artifact not found")
	url := fmt.Sprintf("%s/repository/%s-artifacts/%s", artifactsRepositoryDomain, repo, artifactFile)
	cli.Println("Attempting to download artifact: %s", url)

	err := os.MkdirAll(artifactsPath, dirPermissions)
	if err != nil {
		return err
	}

	out, err := os.Create(filepath.Clean(artifactPath))
	if err != nil {
		return err
	}

	defer func() {
		if err = out.Close(); err != nil {
			cli.Println("error closing stream")
		}
	}()

	client := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, url, nil)

	req.SetBasicAuth(username, password)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer func() {
		if err = resp.Body.Close(); err != nil {
			cli.Println("error closing stream")
		}
	}()

	statusCode := resp.StatusCode
	if statusCode != http.StatusOK {
		errRemove := os.Remove(artifactPath)
		if errRemove != nil {
			log.Debug("Error during removing invalid artifact: %s", errRemove.Error())
		}

		if statusCode == http.StatusUnauthorized {
			return errors.New("invalid credentials")
		}
		if statusCode == http.StatusNotFound {
			return errors.New("artifact was not found")
		}

		return fmt.Errorf("unexpected status code: %d while trying to get %s", statusCode, url)
	}

	_, err = io.Copy(out, resp.Body)

	return err
}

func (s *ArtifactStorage) unarchiveTar(fpath, tpath string) (string, error) {
	var rootDir string
	rgxPathRoot := regexp.MustCompile(`^[^/]*`)

	r, err := os.Open(filepath.Clean(fpath))
	if err != nil {
		return rootDir, err
	}

	gzr, err := gzip.NewReader(r)
	if err != nil {
		return rootDir, err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, readErr := tr.Next()

		switch {

		// if no more files are found return
		case readErr == io.EOF:
			if rootDir != "" {
				rootDir = rgxPathRoot.FindString(rootDir)
			}

			return rootDir, nil

		// return any other error
		case readErr != nil:
			return rootDir, readErr

		// if the header is nil, just skip it (not sure how this happens)
		case header == nil:
			continue
		}

		// the target location where the dir/file should be created
		target, readErr := s.sanitizeArchivePath(tpath, header.Name)
		if readErr != nil {
			return rootDir, errors.New("invalid filepath")
		}

		if !strings.HasPrefix(target, filepath.Clean(tpath)) {
			return rootDir, errors.New("invalid filepath")
		}

		// check the file type
		switch header.Typeflag {

		// if it's a dir, and it doesn't exist create it
		case tar.TypeDir:
			rootDir = header.Name
			if _, errStat := os.Stat(target); errStat != nil {
				if errMk := os.MkdirAll(target, 0750); errMk != nil {
					return rootDir, err
				}
			}

		// if it's a file create it
		case tar.TypeReg:
			f, errOpen := os.OpenFile(filepath.Clean(target), os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if errOpen != nil {
				return rootDir, errOpen
			}

			for {
				_, errCopy := io.CopyN(f, tr, 1024)
				if errCopy != nil {
					if errCopy != io.EOF {
						return rootDir, errCopy
					}
					break
				}
			}

			// manually close here after each file operation; deferring would cause each file close
			// to wait until all operations have completed.
			err = f.Close()
			if err != nil {
				return rootDir, err
			}
		}
	}
}

func (s *ArtifactStorage) sanitizeArchivePath(d, t string) (v string, err error) {
	v = filepath.Join(d, t)
	if strings.HasPrefix(v, filepath.Clean(d)) {
		return v, nil
	}

	return "", fmt.Errorf("%s: %s", "content filepath is tainted", t)
}
