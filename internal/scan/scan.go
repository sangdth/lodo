// Package scan reads a project and proposes what lodo should list for it: a
// name for the project and one for each app, their address and ports, the
// compose file to link, and the lines that start a dev server on every
// address. It reads files and never writes them.
package scan

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/store"
)

// Project is what Find read from a project.
type Project struct {
	Root       string   `json:"root"`
	Name       string   `json:"name"`    // what the root folder suggests, such as flowy.test; empty when it makes no label
	Compose    []string `json:"compose"` // its compose files, best first, absolute
	Apps       []App    `json:"apps"`
	EnvAddress string   `json:"-"` // DOCKER_HOST_IP from the root's .env or .env.example, when it is an own address
}

// Find reads the project dir is in. ok is false outside one
// (compose.ProjectRoot). A file it can't read counts as missing: the scan is
// only an offer.
func Find(dir string) (Project, bool) {
	root, ok := compose.ProjectRoot(dir)
	if !ok {
		return Project{}, false
	}
	files, _ := compose.Find(root)
	return Project{Root: root, Name: Name(root), Compose: files, Apps: findApps(root), EnvAddress: envAddress(root)}, true
}

// nonLabel matches what a folder's name holds that a DNS label can't.
var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// label returns the DNS label a folder's name suggests: flowy for
// ~/Projects/Flowy, my-app for my_app. It is empty when nothing is left.
func label(folder string) string {
	return strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(folder), "-"), "-")
}

// Name is the name a project's folder suggests: flowy.test for
// ~/Projects/flowy. It is empty when the folder's name makes no valid label.
func Name(root string) string {
	name := label(filepath.Base(root)) + "." + store.TLD
	if store.ValidateName(name) != nil {
		return ""
	}
	return name
}

// hostIPRE matches the line of a .env that sets DOCKER_HOST_IP. The group is
// the value.
var hostIPRE = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + compose.EnvVar + `[ \t]*=[ \t]*["']?([0-9.]+)`)

// envAddress returns the own address the root's .env, else its .env.example,
// gives DOCKER_HOST_IP, so lodo matches what the project expects. Only that
// line is read.
func envAddress(root string) string {
	for _, name := range []string{".env", ".env.example"} {
		data, err := fsutil.ReadRegular(filepath.Join(root, name), fsutil.MaxProjectFile)
		if err != nil {
			continue
		}
		if m := hostIPRE.FindSubmatch(data); m != nil && store.IsOwn(string(m[1])) {
			return string(m[1])
		}
	}
	return ""
}

// Proposal is what lodo should change for a project: what Propose found
// missing from the list.
type Proposal struct {
	Root    string         `json:"root"`
	Name    string         `json:"name"`
	Address string         `json:"address"`           // the project's address
	Listed  bool           `json:"listed"`            // the list already has a name of the project
	Claim   bool           `json:"claim,omitempty"`   // the listed name Name gets Root as its project folder
	Add     []store.Domain `json:"add"`               // names to add, the project's first
	Compose string         `json:"compose,omitempty"` // the compose file to link to Name; empty when there is none or one is linked
	Fixes   []Fix          `json:"fixes"`             // lines to change by hand
	Notes   []string       `json:"notes"`             // what lodo couldn't decide
}

// Empty reports whether the proposal changes nothing.
func (p Proposal) Empty() bool {
	return len(p.Add) == 0 && p.Compose == "" && len(p.Fixes) == 0 && !p.Claim
}

// ValidName checks a name the user chose for a project: a valid name with one
// label before .test, since a project is its last two labels.
func ValidName(name string) error {
	if err := store.ValidateName(name); err != nil {
		return err
	}
	if store.Project(name) != name {
		return &store.FieldError{Field: store.FieldName, Msg: "a project's name has one label before ." + store.TLD}
	}
	return nil
}

// projectName returns the project's name: the listed name whose Root is the
// project's folder, else the project of the listed name that links a compose
// file in it, whatever its folder is called (hugger.test for
// huggerhustle/platform-v2), else what the folder suggests.
func projectName(p Project, domains []store.Domain) string {
	if i := slices.IndexFunc(domains, func(d store.Domain) bool { return d.Root == p.Root }); i >= 0 {
		return domains[i].Name
	}
	if i := slices.IndexFunc(domains, func(d store.Domain) bool { return linksInside(d, p.Root) }); i >= 0 {
		return store.Project(domains[i].Name)
	}
	return p.Name
}

// Propose compares the project with the listed domains. name is the name the
// user chose for the project; empty lets Propose pick it (projectName). The
// project gets that name, with its folder as Root and the root app's port,
// and each other app a subdomain named after its folder with its port, all
// on the project's address: the one a listed name of the project has, else
// the .env's own address when no other project has it, else the lowest free
// one (assignPorts shares the ports out). Names already listed stay as they
// are; only their address and port are used for the fixes, and a listed name
// the user chose gets the folder as Root (Claim). A project with no app and no
// compose file gets nothing.
func Propose(p Project, domains []store.Domain, name string) Proposal {
	prop := Proposal{Root: p.Root, Add: []store.Domain{}, Fixes: []Fix{}, Notes: []string{}}
	if name != "" {
		if err := ValidName(name); err != nil {
			prop.Notes = append(prop.Notes, name+": "+err.Error())
			return prop
		}
		p.Name = name
	} else {
		p.Name = projectName(p, domains)
	}
	prop.Name = p.Name
	if p.Name == "" || (len(p.Apps) == 0 && len(p.Compose) == 0) {
		return prop
	}
	if d, ok := find(domains, p.Name); ok && name != "" && d.Root != p.Root {
		prop.Claim = true // the user named a listed name: it becomes the project's
	}
	prop.Address, prop.Listed = projectAddress(p, domains)
	if prop.Address == "" {
		prop.Notes = append(prop.Notes, store.ErrBlockFull.Error()+": free one, then scan again")
		return prop
	}
	apps := []entry{{dir: ".", name: p.Name}}
	for _, a := range p.Apps {
		if a.Dir == "." {
			apps[0].app = a
			if a.Port == 0 {
				prop.Notes = append(prop.Notes, p.Name+": lodo can't tell its dev server's port; e sets it")
			}
		}
	}
	for _, a := range p.Apps {
		if a.Dir == "." {
			continue
		}
		name := label(path.Base(a.Dir)) + "." + p.Name
		if store.ValidateName(name) != nil {
			prop.Notes = append(prop.Notes, a.Dir+": its folder's name makes no valid name")
			continue
		}
		if _, listed := find(domains, name); !listed {
			switch {
			case a.Port == 0:
				prop.Notes = append(prop.Notes, name+": lodo can't tell its dev server's port; e sets it")
			case a.Tool == "":
				prop.Notes = append(prop.Notes, name+": lodo doesn't know its dev command; start it on the name's address")
			case !HasHost(a.Tool):
				prop.Notes = append(prop.Notes, name+": "+a.Tool+" has no host option; make the app listen on the name's address")
			}
		}
		apps = append(apps, entry{dir: a.Dir, name: name, app: a})
	}
	ports := assignPorts(apps, domains, prop.Address)

	list := slices.Clone(domains)
	addresses := map[string]string{} // app folder → address, for the fixes
	moved := map[string]int{}        // app folder → the port its dev script must take, for the fixes
	for _, e := range apps {
		if d, ok := find(list, e.name); ok {
			addresses[e.dir] = d.Address
			if d.Port > 0 && e.app.Port > 0 && d.Port != e.app.Port {
				moved[e.dir] = d.Port
			}
			continue
		}
		d := store.Domain{Name: e.name, Address: prop.Address, Port: ports[e.dir], Enabled: true}
		if e.dir == "." {
			d.Root = p.Root
		}
		if d.Port > 0 && portTaken(list, d.Address, d.Port) {
			free, err := store.NextFree(list)
			if err != nil {
				prop.Notes = append(prop.Notes, fmt.Sprintf("%s: port %d is taken on %s and %v", e.name, d.Port, d.Address, err))
				continue
			}
			d.Address = free
		}
		if err := store.ValidatePort(d.Port); err != nil {
			prop.Notes = append(prop.Notes, e.name+": "+err.Error())
			d.Port = 0
		}
		next, err := store.Add(list, d)
		if err != nil {
			prop.Notes = append(prop.Notes, e.name+": "+err.Error())
			continue
		}
		list, addresses[e.dir] = next, d.Address
		if d.Port > 0 && d.Port != e.app.Port {
			moved[e.dir] = d.Port
		}
		prop.Add = append(prop.Add, d)
	}

	if owner, ok := find(list, p.Name); ok && owner.Compose == "" && len(p.Compose) > 0 &&
		!slices.ContainsFunc(list, func(d store.Domain) bool { return linksInside(d, p.Root) }) {
		prop.Compose = p.Compose[0]
	}
	prop.Fixes = fixesFor(p, addresses, moved)
	return prop
}

// entry is a name Propose gives: the project's, with the root app when there
// is one, or an app's subdomain.
type entry struct {
	dir, name string
	app       App
}

// assignPorts returns the port of each app that isn't listed, by folder, so
// a project's apps share its address. An app whose port only its code sets
// (nest, an unknown command) keeps its port; when another name has it on
// the address, it gets the next free address. Then each app whose dev server
// takes --port gets its port, or the next one free on the address: next on
// 3000 next to nest on 3000 gets 3001, which the fixes add to its dev script.
func assignPorts(apps []entry, domains []store.Domain, address string) map[string]int {
	taken := map[int]bool{}
	for _, d := range domains {
		if d.Address == address && d.Port > 0 {
			taken[d.Port] = true
		}
	}
	ports := map[string]int{}
	for _, movable := range []bool{false, true} {
		for _, e := range apps {
			if _, listed := find(domains, e.name); listed || e.app.Port == 0 || HasHost(e.app.Tool) != movable {
				continue
			}
			port := e.app.Port
			for movable && taken[port] {
				port++
			}
			taken[port], ports[e.dir] = true, port
		}
	}
	return ports
}

// projectAddress returns the project's address and whether a name of the
// project is listed. It is empty when every own address is taken.
func projectAddress(p Project, domains []store.Domain) (string, bool) {
	if d, ok := find(domains, p.Name); ok {
		return d.Address, true
	}
	for _, d := range domains {
		if store.Project(d.Name) == p.Name {
			return d.Address, true
		}
	}
	if p.EnvAddress != "" && !slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Address == p.EnvAddress }) {
		return p.EnvAddress, false
	}
	free, err := store.NextFree(domains)
	if err != nil {
		return "", false
	}
	return free, false
}

// fixesFor returns the fixes in the root's package.json and in each app's,
// each with its name's address and, for an app that must move, its port. The root gets the project's address when it
// is no app.
func fixesFor(p Project, addresses map[string]string, ports map[string]int) []Fix {
	dirs := []string{"."}
	for _, a := range p.Apps {
		if a.Dir != "." {
			dirs = append(dirs, a.Dir)
		}
	}
	var fixes []Fix
	for _, dir := range dirs {
		addr, ok := addresses[dir]
		if !ok {
			addr, ok = addresses["."]
		}
		if ok {
			fixes = append(fixes, fixesIn(p.Root, dir, addr, ports[dir])...)
		}
	}
	return fixes
}

// HostFixes returns the lines in the project at root that start a dev server
// on every address, with address added: in the root's package.json and in
// each app's, and in the shell scripts their scripts run.
func HostFixes(root, address string) []Fix {
	dirs := []string{"."}
	for _, a := range findApps(root) {
		if a.Dir != "." {
			dirs = append(dirs, a.Dir)
		}
	}
	var fixes []Fix
	for _, dir := range dirs {
		fixes = append(fixes, fixesIn(root, dir, address, 0)...)
	}
	return fixes
}

// find returns the domain called name.
func find(domains []store.Domain, name string) (store.Domain, bool) {
	i := slices.IndexFunc(domains, func(d store.Domain) bool { return d.Name == name })
	if i < 0 {
		return store.Domain{}, false
	}
	return domains[i], true
}

// portTaken reports whether a listed name already routes port on address.
func portTaken(domains []store.Domain, address string, port int) bool {
	return slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Address == address && d.Port == port })
}

// linksInside reports whether d links a compose file inside root.
func linksInside(d store.Domain, root string) bool {
	return d.Compose != "" && d.Compose != store.NoCompose && strings.HasPrefix(d.Compose, root+string(filepath.Separator))
}
