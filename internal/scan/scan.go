// Package scan reads a project and proposes what lodo should list for it: a
// name for the project, one for each app and one for each web page its compose
// file publishes that lodo knows, their address and ports, the compose file to
// link, and the apps' .env lines to point at the names. It reads files and
// never writes them.
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

	Env   []EnvLine    // the KEY=value lines of each app's .env
	Bound map[int]bool // the ports on this Mac the best compose file binds to DOCKER_HOST_IP
	Repo  string       // the main checkout of the git repository Root is the root of (repoRoot); empty outside one
}

// ErrNoProject is what lodo reports when Find finds no project.
var ErrNoProject = errors.New("not in a project: lodo scans the git root, or a folder with a lock file, it starts in")

// ErrUnknownPort refuses a name for an app whose port lodo can't tell: a later
// scan ties a listed name to an app by its port, so it couldn't find this one.
var ErrUnknownPort = errors.New("lodo can't tell its port, so the name wouldn't stick: set PORT in its .env or a port in its dev script")

// Find reads the project dir is in. ok is false outside one
// (compose.ProjectRoot). A file it can't read counts as missing: the scan is
// only an offer.
func Find(dir string) (Project, bool) {
	root, ok := compose.ProjectRoot(dir)
	if !ok {
		return Project{}, false
	}
	files, _ := compose.Find(root)
	apps := findApps(root)
	p := Project{Root: root, Name: Name(root), Compose: files, Apps: apps, EnvAddress: envAddress(root), Env: findEnv(root, apps), Repo: repoRoot(root)}
	if len(files) > 0 {
		p.Pages, p.Bound = readCompose(files[0])
	}
	return p, true
}

// nonLabel matches what a folder's name holds that a DNS label can't.
var nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// Label returns the DNS label a folder's name suggests: flowy for
// ~/Projects/Flowy, my-app for my_app. It is empty when nothing is left.
func Label(folder string) string {
	return strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(folder), "-"), "-")
}

// Name is the name a project's folder suggests: flowy.test for
// ~/Projects/flowy. It is empty when the folder's name makes no valid label.
func Name(root string) string {
	name := Label(filepath.Base(root)) + store.Suffix
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
	Env     []Var          `json:"env"`               // the variables lodo dev and lodo exec set in place of .env values, masked
	Notes   []string       `json:"notes"`             // what lodo couldn't decide

	Worktree bool   `json:"worktree,omitempty"` // Root is a git worktree, which gets its own compose project
	Main     string `json:"main,omitempty"`     // the main checkout's project name, for a worktree of a listed one

	Dirs map[string]string `json:"-"` // each app's name, in Add or listed, to the app's folder
}

// CanAccept reports whether y has something to apply: names to add, a compose
// file to link or a folder to save as the project's. Env needs nothing saved.
func (p Proposal) CanAccept() bool {
	return len(p.Add) > 0 || p.Compose != "" || p.Claim
}

// projectName returns the project's name: the listed name whose Root is the
// project's folder, else the project of the listed name that links a compose
// file in it, whatever its folder is called (hugger.test for
// huggerhustle/platform-v2), unless that project is another's (Taken), else
// a worktree's of a listed main checkout's (worktreeSlot), else what the folder
// suggests. When that name is another project's, such as a second clone's, it
// is the first of <label>-2.test, <label>-3.test … that no listed name has, so
// the project gets its own names and address.
func projectName(p Project, domains []store.Domain, gone func(path string) bool) string {
	if i := slices.IndexFunc(domains, func(d store.Domain) bool { return SameFolder(d.Root, p.Root) }); i >= 0 {
		return domains[i].Name
	}
	if i := slices.IndexFunc(domains, func(d store.Domain) bool {
		owner, ok := store.Find(domains, store.Project(d.Name))
		return linksInside(d, p.Root) && (!ok || !Taken(owner, p.Root, gone))
	}); i >= 0 {
		return store.Project(domains[i].Name)
	}
	if main, ok := mainProject(p, domains); ok {
		if name, _, ok := worktreeSlot(p, domains, main, gone); ok {
			return name
		}
	}
	if d, ok := store.Find(domains, p.Name); !ok || !Taken(d, p.Root, gone) {
		return p.Name
	}
	base := strings.TrimSuffix(p.Name, store.Suffix)
	for n := 2; ; n++ {
		name := base + "-" + strconv.Itoa(n) + store.Suffix
		if !slices.ContainsFunc(domains, func(d store.Domain) bool { return store.Project(d.Name) == name }) {
			return name
		}
	}
}

// Taken reports whether the listed name d belongs to another project than the
// one at root: its Root is another folder, and gone, which reports whether
// nothing is at a path any more, says it is still there. A name whose Root is
// unset, root or gone is the project's to take over.
func Taken(d store.Domain, root string, gone func(path string) bool) bool {
	return d.Root != "" && !SameFolder(d.Root, root) && !gone(d.Root)
}

// SameFolder reports whether a and b name one folder: macOS ignores case, and
// /tmp is /private/tmp, so os.Getwd's path can differ from a saved Root.
func SameFolder(a, b string) bool {
	if a == b {
		return true
	}
	x, errA := os.Stat(a)
	y, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(x, y)
}

// BelongsTo is why a project can't take name: it belongs to the project in
// folder.
func BelongsTo(name, folder string) error {
	return errors.New(name + " belongs to " + folder + ": give this project another name")
}

// Propose compares the project with the listed domains. name is the name the
// user chose for the project; empty lets Propose pick it (projectName). The
// project gets that name, with its folder as Root and the root app's port,
// and each other app a subdomain named after its folder with its port, all
// on the project's address: the one a listed name of the project has, else
// the .env's own address when no other project has it, else the lowest free
// one. An app keeps its own port; when another name has it on the address, it
// gets the next free address and a note, since lodo dev binds one app per
// port. Each page gets its label's subdomain, such as mail.shop.test, with
// its port on the address of the project's name, unless a listed name has
// that port there, or the project runs none or another compose file than the
// first, which the pages come from. Names already listed stay as they are,
// and a listed name whose folder is unset or gone gets the folder as Root
// (Claim); a name the user chose that is another project's (Taken, by gone)
// gets only a note. The project's best compose file is offered when
// no name of the project links one that is still there (linked). A project
// with no app and no compose file gets nothing. An app whose folder's name
// isn't listed keeps a listed name of the project on its port (aliasOf). The
// apps' .env values that point at this Mac's own host, or at another checkout
// of the project's repository, give the variables lodo dev sets (envVars).
func Propose(p Project, domains []store.Domain, name string, gone func(path string) bool) Proposal {
	prop := Proposal{Root: p.Root, Add: []store.Domain{}, Env: []Var{}, Notes: []string{}, Dirs: map[string]string{}}
	if name != "" {
		if err := store.ValidateProject(name); err != nil {
			prop.Notes = append(prop.Notes, name+": "+err.Error())
			return prop
		}
		p.Name = name
	} else {
		p.Name = projectName(p, domains, gone)
	}
	prop.Name, prop.Worktree = p.Name, p.Worktree()
	main, _ := mainProject(p, domains)
	prop.Main = main.Name
	if p.Name == "" || (len(p.Apps) == 0 && len(p.Compose) == 0) {
		return prop
	}
	if d, ok := store.Find(domains, p.Name); ok && !SameFolder(d.Root, p.Root) {
		switch {
		case !Taken(d, p.Root, gone):
			prop.Claim = true // a listed name whose folder is unset or gone becomes the project's
			if p.Worktree() && len(p.Compose) > 0 {
				prop.Notes = append(prop.Notes, fmt.Sprintf("%s takes over a gone folder's name, with its compose containers and data: lodo exec docker compose down -v gives it fresh ones",
					p.Name))
			}
		case name != "":
			prop.Notes = append(prop.Notes, BelongsTo(p.Name, d.Root).Error())
			return prop
		}
	}
	prop.Address, prop.Listed = projectAddress(p, domains)
	if name, address, ok := worktreeSlot(p, domains, main, gone); main.Name != "" && !prop.Listed && ok && name == p.Name {
		prop.Address = address // the number in the name names the address
	}
	if prop.Address == "" {
		prop.Notes = append(prop.Notes, store.ErrBlockFull.Error()+": free one, then scan again")
		return prop
	}
	apps, appNotes := entries(p, domains, prop.Address)
	prop.Notes = append(prop.Notes, appNotes...)

	list := slices.Clone(domains)
	for _, e := range apps {
		if d, ok := store.Find(list, e.name); ok {
			if e.app.Dir != "" {
				prop.Dirs[e.name] = e.app.Dir
			}
			switch {
			case e.app.Dir == "" || d.Port > 0:
			case e.app.Port > 0:
				prop.Notes = append(prop.Notes, fmt.Sprintf("%s: listed without a port; its dev server takes %d: set its port in lodo dev --edit", e.name, e.app.Port))
			default:
				prop.Notes = append(prop.Notes, e.name+": listed without a port, and lodo can't tell its dev server's; set its port in lodo dev --edit")
			}
			continue
		}
		d := fromMain(store.Domain{Name: e.name, Address: prop.Address, Port: e.app.Port, Enabled: true}, p.Name, main, domains)
		if e.name == p.Name {
			d.Root = p.Root
		}
		if other := onPort(list, d.Address, d.Port); d.Port > 0 && other != "" {
			free, err := store.NextFree(list)
			if err != nil {
				prop.Notes = append(prop.Notes, fmt.Sprintf("%s: port %d is taken on %s and %v", e.name, d.Port, d.Address, err))
				continue
			}
			d.Address = free
			prop.Notes = append(prop.Notes, fmt.Sprintf(
				"%s and %s both take port %d; lodo dev binds one app per port: give one of them another port", other, e.name, d.Port))
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
		list = next
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
				d := fromMain(store.Domain{Name: pg.Label + "." + p.Name, Address: owner.Address, Port: pg.Port, Enabled: true}, p.Name, main, domains)
				if _, listed := store.Find(list, d.Name); listed || onPort(list, d.Address, d.Port) != "" {
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
		var bound map[int]bool // only while compose runs the file Find read the ports from
		if len(p.Compose) > 0 && used == p.Compose[0] {
			bound = p.Bound
		}
		env, notes := envVars(p, ValuesFor(list, owner), bound, siblingNames(p, list, p.Name))
		prop.Env, prop.Notes = append(prop.Env, env...), append(prop.Notes, notes...)
	}
	return prop
}

// entries names the project's apps, the root's entry first: the root app gets
// the project's name, and each other app a subdomain named after its folder,
// or a listed name of the project on its port on address (aliasOf). It also
// returns notes on what it couldn't decide.
func entries(p Project, domains []store.Domain, address string) ([]entry, []string) {
	var notes []string
	apps := []entry{{dir: ".", name: p.Name}}
	own := map[string]bool{} // the names apps get by default
	_, listed := store.Find(domains, p.Name)
	for _, a := range p.Apps {
		if a.Dir != "." {
			own[Label(path.Base(a.Dir))+"."+p.Name] = true
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
		name := Label(path.Base(a.Dir)) + "." + p.Name
		if _, listed := store.Find(domains, name); !listed && a.Port > 0 {
			if alias, ok := aliasOf(domains, p.Name, address, a.Port, own); ok {
				name, own[alias] = alias, true // one app per listed name; the next keeps its folder's
			}
		}
		if store.ValidateName(name) != nil {
			notes = append(notes, a.Dir+": its folder's name makes no valid name")
			continue
		}
		if _, listed := store.Find(domains, name); !listed && a.Port == 0 {
			notes = append(notes, name+": "+noPort)
		}
		if i := slices.IndexFunc(apps[1:], func(e entry) bool { return e.name == name }); i >= 0 {
			notes = append(notes, a.Dir+": "+apps[i+1].dir+" has "+name)
			continue
		}
		apps = append(apps, entry{dir: a.Dir, name: name, app: a})
	}
	return apps, notes
}

// noPort is the note for an app whose port lodo can't read.
const noPort = "lodo can't tell its port; set it in lodo dev --edit"

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

// onPort returns the listed name that routes port on address, or "" when
// none does.
func onPort(domains []store.Domain, address string, port int) string {
	if i := slices.IndexFunc(domains, func(d store.Domain) bool { return d.Address == address && d.Port == port }); i >= 0 {
		return domains[i].Name
	}
	return ""
}

// linksInside reports whether d links a compose file inside root. It compares
// the paths as written, unlike SameFolder.
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
