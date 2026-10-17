// Package scan reads a project and proposes what lodo should list for it: a
// name for the project, one for each app and one for each web page its compose
// file publishes that lodo knows, their address and ports, the compose file to
// link, and the lines that start a dev server on every address. It reads files
// and never writes them.
package scan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/store"
)

// Project is what Find read from a project.
type Project struct {
	Root       string
	Name       string   // what the root folder suggests, such as flowy.test; empty when it makes no label
	Compose    []string // its compose files, best first, absolute
	Apps       []App
	Pages      []Page
	EnvAddress string // DOCKER_HOST_IP from the root's .env or .env.example, when it is an own address

	Env            []EnvLine    // the lines of each app's .env that mention localhost or 127.0.0.1, none from a file git tracks
	Shared         []string     // the app .env files git tracks that hold one, from the root
	Bound          map[int]bool // the ports on this Mac the best compose file binds to DOCKER_HOST_IP
	ComposeAddress string       // DOCKER_HOST_IP in the .env that file runs with (compose.EnvFile), when it is an own address
	NoDevOrigins   []string     // the folders of next apps whose next.config has no allowedDevOrigins
}

// ErrNoProject is what lodo reports when Find finds no project.
var ErrNoProject = errors.New("not in a project: lodo scans the git root, or a folder with a lock file, it starts in")

// Find reads the project dir is in. ok is false outside one
// (compose.ProjectRoot). A file it can't read counts as missing: the scan is
// only an offer. tracked reports whether git tracks a file; nil means nothing
// is tracked.
func Find(dir string, tracked func(path string) bool) (Project, bool) {
	root, ok := compose.ProjectRoot(dir)
	if !ok {
		return Project{}, false
	}
	files, _ := compose.Find(root)
	apps := findApps(root)
	p := Project{
		Root: root, Name: Name(root), Compose: files, Apps: apps, EnvAddress: envAddress(root),
		NoDevOrigins: noDevOrigins(root, apps),
	}
	p.Env, p.Shared = findEnv(root, apps, tracked)
	if len(files) > 0 {
		p.Pages, p.Bound = readCompose(files[0])
		p.ComposeAddress = envMatch(hostIPRE, store.IsOwn, compose.EnvFile(files[0]))
	}
	return p, true
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
	name := label(filepath.Base(root)) + store.Suffix
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
	return envMatch(hostIPRE, store.IsOwn, filepath.Join(root, ".env"), filepath.Join(root, ".env.example"))
}

// portLineRE matches the PORT= line of a .env, not a comment. The group is the
// port.
var portLineRE = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?PORT[ \t]*=[ \t]*["']?(\d{1,5})\b`)

// envPort returns the port PORT sets in the .env of the app in dir, else in
// its .env.example, and 0 without one.
func envPort(dir string) int {
	valid := func(s string) bool {
		port, err := strconv.Atoi(s)
		return err == nil && port >= 1 && port <= 65535
	}
	port, _ := strconv.Atoi(envMatch(portLineRE, valid, filepath.Join(dir, ".env"), filepath.Join(dir, ".env.example")))
	return port
}

// envMatch returns group 1 of re in the first of files that has a match valid
// accepts. A file it can't read counts as missing.
func envMatch(re *regexp.Regexp, valid func(string) bool, files ...string) string {
	for _, file := range files {
		data, err := fsutil.ReadRegular(file)
		if err != nil {
			continue
		}
		if m := re.FindSubmatch(data); m != nil && valid(string(m[1])) {
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
	Env     []Fix          `json:"env"`               // .env lines lodo sets on y, masked (ApplyEnv)
	Notes   []string       `json:"notes"`             // what lodo couldn't decide

	Dirs map[string]string `json:"-"` // each name in Add to its app's folder
}

// CanAccept reports whether y has something to apply: names to add, a compose
// file to link, a folder to save as the project's or .env lines to set.
func (p Proposal) CanAccept() bool {
	return len(p.Add) > 0 || p.Compose != "" || p.Claim || len(p.Env) > 0
}

// Empty reports whether the proposal changes nothing.
func (p Proposal) Empty() bool { return !p.CanAccept() && len(p.Fixes) == 0 }

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
// one (assignPorts shares the ports out). Each page gets its label's
// subdomain, such as mail.shop.test, with its port on the address of the
// project's name, unless a listed name has that port there, or the project
// runs none or another compose file than the first, which the pages come
// from. Names already listed stay as they are; only their address and port are
// used for the fixes, and a listed name the user chose gets the folder as Root
// (Claim). The project's best compose file is offered when no name of the
// project links one that is still there (linked). A project with no app and no
// compose file gets nothing. labels maps an app's folder to the label the user
// gave it, "" for the project's name alone (CheckLabel); an app without one
// keeps a listed name of the project on its port (aliasOf). The apps' .env
// lines that point at this Mac's own host get the names (envFixes).
func Propose(p Project, domains []store.Domain, name string, labels map[string]string) Proposal {
	prop := Proposal{Root: p.Root, Add: []store.Domain{}, Fixes: []Fix{}, Env: []Fix{}, Notes: []string{}, Dirs: map[string]string{}}
	if name != "" {
		if err := store.ValidateProject(name); err != nil {
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
	if d, ok := store.Find(domains, p.Name); ok && name != "" && d.Root != p.Root {
		prop.Claim = true // the user named a listed name: it becomes the project's
	}
	prop.Address, prop.Listed = projectAddress(p, domains)
	if prop.Address == "" {
		prop.Notes = append(prop.Notes, store.ErrBlockFull.Error()+": free one, then scan again")
		return prop
	}
	apps, appNotes := entries(p, domains, labels, prop.Address)
	prop.Notes = append(prop.Notes, appNotes...)
	ports := assignPorts(apps, domains, prop.Address)

	list := slices.Clone(domains)
	addresses := map[string]string{} // app folder → address, for the fixes
	moved := map[string]int{}        // app folder → the port its dev script must take, for the fixes
	for _, e := range apps {
		if d, ok := store.Find(list, e.name); ok {
			addresses[e.dir] = d.Address
			if e.name == p.Name {
				addresses["."] = d.Address // the root's scripts follow the project's name
			}
			if d.Port > 0 && e.app.Port > 0 && d.Port != e.app.Port {
				moved[e.dir] = d.Port
			}
			switch {
			case e.app.Dir == "" || d.Port > 0:
			case e.app.Port > 0:
				prop.Notes = append(prop.Notes, fmt.Sprintf("%s: listed without a port; its dev server takes %d: set it with e in the list", e.name, e.app.Port))
			default:
				prop.Notes = append(prop.Notes, e.name+": listed without a port, and lodo can't tell its dev server's; set it with e in the list")
			}
			continue
		}
		d := store.Domain{Name: e.name, Address: prop.Address, Port: ports[e.dir], Enabled: true}
		if e.name == p.Name {
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
		if e.name == p.Name {
			addresses["."] = d.Address
		}
		if d.Port > 0 && d.Port != e.app.Port {
			moved[e.dir] = d.Port
		}
		prop.Add = append(prop.Add, d)
		if e.app.Dir != "" {
			prop.Dirs[d.Name] = e.app.Dir
		}
	}

	owner, ok := store.Find(list, p.Name)
	used := "" // the compose file the project runs once the proposal is applied
	if ok && len(p.Compose) > 0 {
		used = owner.Compose
		if !linked(owner, p.Compose) {
			used = p.Compose[0]
			if i := slices.IndexFunc(list, func(d store.Domain) bool { return linksInside(d, p.Root) && linked(d, p.Compose) }); i >= 0 {
				used = list[i].Compose
			} else {
				prop.Compose = used
			}
		}
		if used == p.Compose[0] { // the file Find read the pages from
			for _, pg := range p.Pages {
				d := store.Domain{Name: pg.Label + "." + p.Name, Address: owner.Address, Port: pg.Port, Enabled: true}
				if _, listed := store.Find(list, d.Name); listed || portTaken(list, d.Address, d.Port) {
					continue // a listed name stays as it is, and the name on the port serves the page
				}
				next, err := store.Add(list, d)
				if err != nil {
					prop.Notes = append(prop.Notes, d.Name+": "+err.Error())
					continue
				}
				list = next
				prop.Add = append(prop.Add, d)
			}
		}
	}
	if ok {
		var bound map[int]bool // only while compose runs that file with the project's address
		if len(p.Compose) > 0 && used == p.Compose[0] && p.ComposeAddress == owner.Address {
			bound = p.Bound
		}
		env, notes := envFixes(p, ValuesFor(list, owner), bound)
		prop.Env, prop.Notes = append(prop.Env, env...), append(prop.Notes, notes...)
	}
	if note := devOriginsNote(p); note != "" {
		prop.Notes = append(prop.Notes, note)
	}
	rootEnv := filepath.Join(p.Root, ".env")
	hostIP := isTurbo(p.Root) && (envMatch(hostIPRE, store.IsOwn, rootEnv) == prop.Address ||
		prop.Compose != "" && compose.EnvFile(prop.Compose) == rootEnv && owner.Address == prop.Address) // y's LinkEnv writes it
	prop.Fixes = fixesFor(p, prop.Address, hostIP, addresses, moved)
	return prop
}

// entries names the project's apps, the root's entry first: the root app gets
// the project's name, and each other app a subdomain named after its folder,
// the label the user gave it in labels, or a listed name of the project on its
// port on address (aliasOf). It also returns notes on what it couldn't decide.
func entries(p Project, domains []store.Domain, labels map[string]string, address string) ([]entry, []string) {
	var notes []string
	apps := []entry{{dir: ".", name: p.Name}}
	own := map[string]bool{} // the names apps get by default
	_, listed := store.Find(domains, p.Name)
	for _, a := range p.Apps {
		if a.Dir != "." {
			own[label(path.Base(a.Dir))+"."+p.Name] = true
			continue
		}
		apps[0].app, own[p.Name] = a, true
		if a.Port == 0 && !listed {
			notes = append(notes, p.Name+": "+noPort)
		}
	}
	for _, a := range p.Apps {
		if a.Dir == "." {
			continue
		}
		name, bare := label(path.Base(a.Dir))+"."+p.Name, false
		if lbl, ok := labels[a.Dir]; ok {
			switch err := CheckLabel(p, domains, p.Name, a.Dir, lbl); {
			case err != nil:
				notes = append(notes, a.Dir+": "+err.Error())
			case lbl != "":
				name = lbl + "." + p.Name
			case apps[0].dir == ".":
				name, bare = p.Name, true
			default:
				notes = append(notes, a.Dir+": "+apps[0].dir+" has "+p.Name)
			}
		} else if _, listed := store.Find(domains, name); !listed && a.Port > 0 {
			if alias, ok := aliasOf(domains, p.Name, address, a.Port, own); ok {
				name, own[alias] = alias, true // one app per listed name; the next keeps its folder's
			}
		}
		if store.ValidateName(name) != nil {
			notes = append(notes, a.Dir+": its folder's name makes no valid name")
			continue
		}
		if _, listed := store.Find(domains, name); !listed {
			if a.Port == 0 {
				notes = append(notes, name+": "+noPort)
			}
			switch {
			case a.Tool == "":
				notes = append(notes, name+": lodo doesn't know its dev command; start it on the name's address")
			case !HasHost(a.Tool):
				notes = append(notes, name+": "+a.Tool+" has no host option; make the app listen on the name's address")
			}
		}
		if bare {
			apps[0] = entry{dir: a.Dir, name: name, app: a} // the project's name serves the app
			continue
		}
		if i := slices.IndexFunc(apps[1:], func(e entry) bool { return e.name == name }); i >= 0 {
			notes = append(notes, a.Dir+": "+apps[i+1].dir+" has "+name)
			continue
		}
		apps = append(apps, entry{dir: a.Dir, name: name, app: a})
	}
	return apps, notes
}

// devOriginsNote returns the note for the project's next apps whose
// next.config has no allowedDevOrigins, or "" when there are none.
func devOriginsNote(p Project) string {
	if len(p.NoDevOrigins) == 0 {
		return ""
	}
	var names []string
	for _, dir := range p.NoDevOrigins {
		if dir == "." {
			dir = p.Name
		}
		names = append(names, path.Base(dir))
	}
	return strings.Join(names, ", ") + ": Next 16.2+ refuses hot reload from other hosts; add allowedDevOrigins: ['" +
		p.Name + "', '*." + p.Name + "'] to next.config"
}

// noPort is the note for an app whose port lodo can't read.
const noPort = "lodo can't tell its dev server's port; set it with e in the list"

// CheckLabel says why app dir can't take lbl in project: an invalid name,
// another app's name by default, a name listed on another port, the bare name
// when the root is an app or the bare name is listed, or an app whose port
// lodo can't tell, whose name a later scan couldn't find. lbl "" means the
// project's name alone.
func CheckLabel(p Project, domains []store.Domain, project, dir, lbl string) error {
	if lbl != "" {
		if err := store.ValidateName(lbl + "." + project); err != nil {
			return err
		}
		if i := slices.IndexFunc(p.Apps, func(a App) bool { return a.Dir != dir && a.Dir != "." && label(path.Base(a.Dir)) == lbl }); i >= 0 {
			return errors.New(p.Apps[i].Dir + " has " + lbl + "." + project)
		}
	} else if slices.ContainsFunc(p.Apps, func(a App) bool { return a.Dir == "." }) {
		return errors.New("the root's app has " + project)
	} else if _, listed := store.Find(domains, project); listed {
		return errors.New(project + " is listed: give it the app's port with e in the list")
	}
	i := slices.IndexFunc(p.Apps, func(a App) bool { return a.Dir == dir })
	if i < 0 || p.Apps[i].Port == 0 {
		return errors.New("lodo can't tell its port, so the name wouldn't stick: set PORT in its .env or a port in its dev script")
	}
	if d, listed := store.Find(domains, lbl+"."+project); lbl != "" && listed && d.Port > 0 && d.Port != p.Apps[i].Port {
		return fmt.Errorf("%s is listed on port %d, not this app's %d", d.Name, d.Port, p.Apps[i].Port)
	}
	return nil
}

// aliasOf returns the listed name of project that routes port on address and
// that no app is named after by default (own): the bare name or label the
// user gave that app.
func aliasOf(domains []store.Domain, project, address string, port int, own map[string]bool) (string, bool) {
	i := slices.IndexFunc(domains, func(d store.Domain) bool {
		return d.Address == address && d.Port == port && store.Project(d.Name) == project && !own[d.Name]
	})
	if i < 0 {
		return "", false
	}
	return domains[i].Name, true
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
			if _, listed := store.Find(domains, e.name); listed || e.app.Port == 0 || HasHost(e.app.Tool) != movable {
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
	if d, ok := store.Find(domains, p.Name); ok {
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
// each with its name's address and, for an app that must move, its port. The
// root gets the project's address, projectAddr, when it is no app. With
// hostIP, set in a turbo project whose root .env holds projectAddr once the
// proposal is applied, a script on projectAddr gets compose.HostIP instead,
// which the root's turbo line sets (withTurbo); an app moved to another
// address keeps its own.
func fixesFor(p Project, projectAddr string, hostIP bool, addresses map[string]string, ports map[string]int) []Fix {
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
		if hostIP && addr == projectAddr {
			addr = compose.HostIP
		}
		if ok {
			fixes = append(fixes, fixesIn(p.Root, dir, addr, ports[dir])...)
		}
	}
	if hostIP {
		return withTurbo(p.Root, dirs, fixes)
	}
	return fixes
}

// HostFixes returns the lines in the project at root that start a dev server
// on every address, with address added: in the root's package.json and in
// each app's, and in the shell scripts their scripts run. A turbo project
// whose root .env sets DOCKER_HOST_IP to address gets compose.HostIP and its
// turbo line (withTurbo).
func HostFixes(root, address string) []Fix {
	hostIP := isTurbo(root) && envMatch(hostIPRE, store.IsOwn, filepath.Join(root, ".env")) == address
	// Every app falls back to the root's address, and no app moves to another port.
	return fixesFor(Project{Root: root, Apps: findApps(root)}, address, hostIP, map[string]string{".": address}, nil)
}

// withTurbo puts the root's turbo line (turboFix) before fixes when an app's
// dev script reads DOCKER_HOST_IP: a fix sets it, or the app's package.json
// names it already. Without one the line would change nothing.
//
// shortcut: it doesn't look in the shell scripts an app's dev script runs;
// check shellScripts too if an app sets its host there.
func withTurbo(root string, dirs []string, fixes []Fix) []Fix {
	reads := slices.ContainsFunc(fixes, func(f Fix) bool { return strings.Contains(f.New, compose.EnvVar) })
	for _, dir := range dirs {
		if _, text, ok := readPackage(filepath.Join(root, filepath.FromSlash(dir))); ok && dir != "." && strings.Contains(text, compose.EnvVar) {
			reads = true
		}
	}
	if f, ok := turboFix(root); ok && reads {
		return append([]Fix{f}, fixes...)
	}
	return fixes
}

// portTaken reports whether a listed name already routes port on address.
func portTaken(domains []store.Domain, address string, port int) bool {
	return slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Address == address && d.Port == port })
}

// linksInside reports whether d links a compose file inside root.
func linksInside(d store.Domain, root string) bool {
	return strings.HasPrefix(d.Compose, root+string(filepath.Separator))
}

// linked reports whether d links no compose file on purpose, or one that is
// still there: one of found, the files the scan found, or one on disk. A link
// to a file that is gone, since the project's folder or the file moved,
// counts as none, so the scan offers the project's file again.
func linked(d store.Domain, found []string) bool {
	switch {
	case d.Compose == "":
		return false
	case d.Compose == store.NoCompose || slices.Contains(found, d.Compose):
		return true
	}
	_, err := os.Stat(d.Compose)
	return !errors.Is(err, fs.ErrNotExist)
}
