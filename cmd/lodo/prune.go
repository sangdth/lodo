package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
	"github.com/sangdth/lodo/internal/tui"
)

// prune removes every listed name whose project folder is gone on disk, with
// the names under it and its listen hook file, and applies the list. With
// worktree, it also removes the git worktree dir is in.
func (a app) prune(ctx context.Context, dir string, worktree bool) int {
	if _, err := os.Stat(a.paths.DomainsJSON); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(a.stdout, "nothing to prune")
		return 0
	}
	wt := ""
	if worktree {
		wt = scan.WorktreeRoot(dir)
	}
	var removed []store.Domain
	if err := store.WithLock(a.paths.DomainsJSON, func() error {
		domains, err := store.Load(a.paths.DomainsJSON)
		if err != nil {
			return err
		}
		next := slices.Clone(domains)
		for _, d := range domains {
			if d.Root == "" || (!gone(d.Root) && (wt == "" || !scan.SameFolder(d.Root, wt))) {
				continue
			}
			if after, err := store.Remove(next, d.Name); err == nil {
				next = after
			}
		}
		if len(next) == len(domains) {
			return nil
		}
		if !system.SetupDone(a.paths) {
			return system.ErrNotSetUp
		}
		if err := store.Save(a.paths.DomainsJSON, next); err != nil {
			return err
		}
		for _, d := range domains {
			if _, kept := store.Find(next, d.Name); !kept {
				removed = append(removed, d)
				proj := store.Project(d.Name)
				if !slices.ContainsFunc(next, func(x store.Domain) bool { return store.Project(x.Name) == proj }) {
					_ = os.Remove(filepath.Join(a.paths.DevDir, proj+".cjs"))
				}
			}
		}
		return system.Apply(ctx, a.paths, a.runner, next)
	}); err != nil {
		a.fail(err)
		return 1
	}
	if len(removed) == 0 {
		fmt.Fprintln(a.stdout, "nothing to prune")
		return 0
	}
	for _, d := range removed {
		if d.Root != "" {
			fmt.Fprintf(a.stdout, "removed %s (%s)\n", d.Name, tui.ShortPath(d.Root, a.paths.Home))
			continue
		}
		fmt.Fprintf(a.stdout, "removed %s\n", d.Name)
	}
	return 0
}

// gone reports whether nothing is at path any more.
func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}
