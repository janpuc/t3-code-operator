package apply

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const (
	DefaultRepositoryScanDepth    = 4
	DefaultRepositoryScanInterval = 5 * time.Minute
	maxRepositoryScanEntries      = 10_000
	maxSafeDirectories            = 2_048
)

func (applier *Applier) prepareFileTargets(
	ctx context.Context,
	targets []render.FileTarget,
) ([]render.FileTarget, error) {
	result := append([]render.FileTarget(nil), targets...)
	for index := range result {
		if !result[index].DiscoverGitSafeDirectories {
			continue
		}
		directories, err := applier.repositorySafeDirectories(ctx)
		if err != nil {
			return nil, err
		}
		result[index].Content = render.AppendGitSafeDirectories(result[index].Content, directories)
		result[index].DiscoverGitSafeDirectories = false
	}
	return result, nil
}

func (applier *Applier) repositorySafeDirectories(ctx context.Context) ([]string, error) {
	if !applier.lastRepositoryScan.IsZero() && applier.now().Sub(applier.lastRepositoryScan) < applier.repositoryScanInterval {
		return append([]string(nil), applier.safeDirectories...), nil
	}
	directories, err := scanGitRepositories(ctx, applier.workspaceRoot, applier.repositoryScanDepth)
	if err != nil {
		return nil, err
	}
	applier.safeDirectories = append(applier.safeDirectories[:0], directories...)
	applier.lastRepositoryScan = applier.now()
	return append([]string(nil), directories...), nil
}

func scanGitRepositories(ctx context.Context, root string, maxDepth int) ([]string, error) {
	root = filepath.Clean(root)
	repositories := make(map[string]struct{})
	entries := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxRepositoryScanEntries {
			return errors.New("repository scan exceeds its entry limit")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(filepath.ToSlash(relative), "/") + 1
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == ".git" && depth <= maxDepth+1 {
			repository := filepath.Dir(path)
			if isPathWithin(root, repository) {
				repositories[repository] = struct{}{}
				if len(repositories) > maxSafeDirectories {
					return errors.New("repository scan exceeds its repository limit")
				}
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() && depth > maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan Git repositories under %s: %w", root, err)
	}
	result := make([]string, 0, len(repositories))
	for repository := range repositories {
		result = append(result, repository)
	}
	sort.Strings(result)
	return result, nil
}
