package scan

import (
	"slices"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/store"
)

// Worktree reports whether p is a git worktree: its repository's main
// checkout is another folder.
func (p Project) Worktree() bool {
	return p.Repo != "" && !SameFolder(p.Repo, p.Root)
}

// mainProject returns the listed project name whose Root is the main checkout
// of the repository p is a worktree of; ok is false when p is no worktree or
// its main checkout has no name.
func mainProject(p Project, domains []store.Domain) (store.Domain, bool) {
	if !p.Worktree() {
		return store.Domain{}, false
	}
	i := slices.IndexFunc(domains, func(d store.Domain) bool { return store.Project(d.Name) == d.Name && SameFolder(d.Root, p.Repo) })
	if i < 0 {
		return store.Domain{}, false
	}
	return domains[i], true
}

// worktreeSlot returns the name and address a worktree of main takes:
// <main's label><n>.test on 127.0.1.<n>, so the number names the address, for
// the lowest n whose address no name uses, or whose address holds only the
// names of <label><n>.test, listed with no folder or a gone one: a deleted
// worktree's, which the worktree takes over (Proposal.Claim). ok is false when
// every address is taken.
func worktreeSlot(p Project, domains []store.Domain, main store.Domain, gone func(path string) bool) (name, address string, ok bool) {
	label := strings.TrimSuffix(main.Name, store.Suffix)
	for n := store.OwnFirst; n <= store.OwnLast; n++ {
		name, address := label+strconv.Itoa(n)+store.Suffix, store.OwnAddress(n)
		if store.ValidateProject(name) != nil {
			return "", "", false // too long for a label: every n is
		}
		owner, listed := store.Find(domains, name)
		others := slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Address == address && store.Project(d.Name) != name })
		switch {
		case others:
		case !listed && !slices.ContainsFunc(domains, func(d store.Domain) bool { return store.Project(d.Name) == name }):
			return name, address, true
		case listed && owner.Address == address && !Taken(owner, p.Root, gone):
			return name, address, true
		}
	}
	return "", "", false
}

// fromMain returns d with the HTTPS of the name of main's project with d's
// label, so a worktree's names match its main checkout's, and the .env URLs
// it copies keep their scheme.
func fromMain(d store.Domain, project string, main store.Domain, domains []store.Domain) store.Domain {
	if main.Name == "" || d.Port == 0 {
		return d
	}
	if m, ok := store.Find(domains, strings.TrimSuffix(d.Name, project)+main.Name); ok {
		d.HTTPS = m.HTTPS
	}
	return d
}
