// Package sync contains tools to provide bump propagation.
package sync

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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/launchrctl/launchr"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
)

const (
	// ArtifactTruncateLength contains bump commit hash truncate length.
	ArtifactTruncateLength = 7
	dirPermissions         = 0755
)

var (
	// ErrArtifactNotFound not artifact error.
	ErrArtifactNotFound = errors.New("artifact was not found")
)

// Artifact represents a storage for artifacts.
type Artifact struct {
	bumper                 *repository.Bumper
	artifactsDir           string
	artifactsRepositoryURL string
	override               string
	comparisonDir          string
	retryLimit             int
}

// NewArtifact returns new instance of artifact to get.
func NewArtifact(artifactsDir, artifactsRepoURL, override, comparisonDir string) (*Artifact, error) {
	b, err := repository.NewBumper()
	if err != nil {
		return nil, err
	}

	return &Artifact{
		bumper:                 b,
		override:               override,
		artifactsDir:           artifactsDir,
		artifactsRepositoryURL: artifactsRepoURL,
		comparisonDir:          comparisonDir,
		retryLimit:             50,
	}, nil
}

// Get prepares the artifact for comparison by downloading and extracting it into the specified directory.
func (a *Artifact) Get(username, password string) error {
	repoName, err := a.bumper.GetRepoName()
	if err != nil {
		return err
	}

	launchr.Term().Info().Printfln("Repository name: %s", repoName)
	var archivePath string
	if a.override != "" {
		comparisonRef := a.override
		launchr.Term().Info().Printfln("OVERRIDDEN_COMPARISON_REF has been set: %s", a.override)
		artifactFile, artifactPath := a.buildArtifactPaths(repoName, comparisonRef)
		err = a.downloadArtifact(username, password, artifactFile, artifactPath, repoName)
		if err != nil {
			return err
		}

		archivePath = artifactPath
	} else {
		hash, errHash := a.bumper.GetGit().ResolveRevision(plumbing.Revision(plumbing.HEAD))
		if errHash != nil {
			return errHash
		}

		from := hash
		retryCount := 0
		for retryCount < a.retryLimit {
			comparisonHash, errHash := a.bumper.GetComparisonCommit(*from, repository.BumpMessage)
			if errHash != nil {
				return errHash
			}

			commit := []rune(comparisonHash.String())
			comparisonRef := string(commit[:ArtifactTruncateLength])

			launchr.Term().Info().Printfln("Bump commit identified: %s", comparisonRef)
			artifactFile, artifactPath := a.buildArtifactPaths(repoName, comparisonRef)
			errDownload := a.downloadArtifact(username, password, artifactFile, artifactPath, repoName)
			if errDownload != nil {
				if errors.Is(errDownload, ErrArtifactNotFound) {
					retryCount++
					from = comparisonHash
					continue
				}

				return errDownload
			}

			archivePath = artifactPath
			break
		}

		if archivePath == "" {
			return ErrArtifactNotFound
		}
	}

	launchr.Term().Printf("Processing...\n")
	err = a.prepareComparisonDir(a.comparisonDir)
	if err != nil {
		return err
	}
	_, err = a.unarchiveTar(archivePath, a.comparisonDir)
	if err != nil {
		return err
	}

	return nil
}

func (a *Artifact) buildArtifactPaths(repoName, comparisonRef string) (string, string) {
	artifactFile := fmt.Sprintf("%s-%s-plasma-src.tar.gz", repoName, comparisonRef)
	artifactPath := filepath.Join(a.artifactsDir, artifactFile)

	return artifactFile, artifactPath
}

func (a *Artifact) prepareComparisonDir(path string) error {
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

func (a *Artifact) downloadArtifact(username, password, artifactFile, artifactPath, repo string) error {
	launchr.Term().Printfln("Attempting to get %s from local storage", artifactFile)
	_, errExists := os.Stat(artifactPath)
	if errExists == nil {
		launchr.Term().Printf("Local artifact found\n")
		return nil
	}

	launchr.Term().Printfln("Local artifact %s not found", artifactFile)
	url := fmt.Sprintf("%s/repository/%s-artifacts/%s", a.artifactsRepositoryURL, repo, artifactFile)
	launchr.Term().Printfln("Attempting to download artifact: %s", url)

	err := os.MkdirAll(a.artifactsDir, dirPermissions)
	if err != nil {
		return err
	}

	out, err := os.Create(filepath.Clean(artifactPath))
	if err != nil {
		return err
	}

	defer func() {
		if err = out.Close(); err != nil {
			launchr.Term().Error().Printf("error closing stream\n")
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
			launchr.Term().Error().Printf("error closing stream\n")
		}
	}()

	statusCode := resp.StatusCode
	if statusCode != http.StatusOK {
		errRemove := os.Remove(artifactPath)
		if errRemove != nil {
			launchr.Log().Debug("Error during removing invalid artifact: msg", "msg", errRemove.Error())
		}

		if statusCode == http.StatusUnauthorized {
			return errors.New("invalid credentials")
		}
		if statusCode == http.StatusNotFound {
			return ErrArtifactNotFound
		}

		return fmt.Errorf("unexpected status code: %d while trying to get %s", statusCode, url)
	}

	_, err = io.Copy(out, resp.Body)

	return err
}

func (a *Artifact) unarchiveTar(fpath, tpath string) (string, error) {
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
		target, readErr := a.sanitizeArchivePath(tpath, header.Name)
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
			f, errOpen := os.OpenFile(filepath.Clean(target), os.O_CREATE|os.O_RDWR, header.FileInfo().Mode())
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

func (a *Artifact) sanitizeArchivePath(d, t string) (v string, err error) {
	v = filepath.Join(d, t)
	if strings.HasPrefix(v, filepath.Clean(d)) {
		return v, nil
	}

	return "", fmt.Errorf("%s: %s", "content filepath is tainted", t)
}
