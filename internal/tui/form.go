package tui

import (
	"cmp"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/store"
)

// The form's fields, in tab order.
const (
	fieldName = iota
	fieldAddress
	fieldPort
	fieldCompose
	fieldCount
)

// tldSuffix ends every name. A form shows its suffix after the name field,
// dimmed, so only the labels before it are typed.
const tldSuffix = "." + store.TLD

// fieldIndex maps store's field names to the form's fields.
var fieldIndex = map[string]int{
	store.FieldName:    fieldName,
	store.FieldAddress: fieldAddress,
	store.FieldPort:    fieldPort,
	store.FieldCompose: fieldCompose,
}

// form adds a domain, or edits one when editing names it; with parent set,
// the name is a subdomain of parent and stays one.
type form struct {
	origin         Start // where lodo started, for compose paths typed with ~/ or relative
	editing        string
	parent         string       // the name a new subdomain goes under
	suffix         string       // what the typed labels end in: .test, or .<parent>
	original       store.Domain // the domain being edited
	inputs         [fieldCount]textinput.Model
	focus          int
	addressTouched bool   // typing in the address field stops the prefill
	hint           string // shown after the address
	errs           [fieldCount]string
}

// newAddForm opens an empty form whose address follows the name. With a
// parent, the name ends in .<parent> and only the labels before it are typed.
func newAddForm(domains []store.Domain, parent string, start Start) form {
	f := form{origin: start, parent: parent, suffix: tldSuffix}
	if parent != "" {
		f.suffix = "." + parent
	}
	f.inputs = newInputs(f.suffix)
	f.prefill(domains)
	f.focusField(fieldName)
	return f
}

// newEditForm opens a form holding d. Its address stays as typed. A
// subdomain keeps its parent: only the labels before .<parent> are typed.
func newEditForm(domains []store.Domain, d store.Domain, start Start) form {
	f := form{origin: start, editing: d.Name, original: d, suffix: tldSuffix, addressTouched: true}
	if parent, ok := store.Parent(domains, d.Name); ok {
		f.parent, f.suffix = parent.Name, "."+parent.Name
	}
	f.inputs = newInputs(f.suffix)
	f.inputs[fieldName].SetValue(strings.TrimSuffix(d.Name, f.suffix))
	f.inputs[fieldAddress].SetValue(d.Address)
	if d.Port > 0 {
		f.inputs[fieldPort].SetValue(strconv.Itoa(d.Port))
	}
	f.inputs[fieldCompose].SetValue(shortPath(d.Compose, start.Home))
	if free, err := store.NextFree(domains); err == nil {
		f.hint = "next free: " + free
	}
	f.focusField(fieldName)
	return f
}

func newInputs(suffix string) [fieldCount]textinput.Model {
	styles := textinput.DefaultDarkStyles()
	styles.Cursor.Blink = false
	var inputs [fieldCount]textinput.Model
	for i, limit := range [fieldCount]int{store.MaxNameLen - len(suffix), len("127.255.255.255"), len("65535"), maxPathLen} {
		inputs[i] = textinput.New()
		inputs[i].Prompt = ""
		inputs[i].CharLimit = limit
		inputs[i].SetStyles(styles)
	}
	inputs[fieldName].SetWidth(store.MaxNameLen) // never scrolls; formView draws it at the text's width
	inputs[fieldName].Placeholder = "app.flowy"
	if suffix != tldSuffix {
		inputs[fieldName].Placeholder = "api"
	}
	inputs[fieldAddress].SetWidth(16)
	inputs[fieldPort].SetWidth(6)
	inputs[fieldPort].Placeholder = "none"
	inputs[fieldCompose].SetWidth(40)
	inputs[fieldCompose].Placeholder = "optional: ~/path/to/compose.dev.yml"
	return inputs
}

// maxPathLen is the longest path macOS takes.
const maxPathLen = 1024

// prefill fills the address while the name is typed: the parent's address
// for a subdomain of a listed name, otherwise the lowest free own address.
func (f *form) prefill(domains []store.Domain) {
	if f.addressTouched {
		return
	}
	name := cmp.Or(f.name(), f.suffix) // before typing, a subdomain form already sits under its parent
	free, freeErr := store.NextFree(domains)
	parent, isSub := store.Parent(domains, name)
	switch {
	case isSub:
		f.inputs[fieldAddress].SetValue(parent.Address)
		f.hint = parent.Name + "'s address"
		if freeErr == nil {
			f.hint += "; next free: " + free
		}
	case freeErr == nil:
		f.inputs[fieldAddress].SetValue(free)
		f.hint = "the lowest free own address"
	default:
		f.inputs[fieldAddress].SetValue("127.0.0.1")
		f.hint = "all own addresses are taken, so it shares 127.0.0.1"
	}
}

func (f *form) focusField(i int) {
	f.focus = (i + fieldCount) % fieldCount
	for j := range f.inputs {
		if j == f.focus {
			f.inputs[j].Focus() // no blink, so no command to run
		} else {
			f.inputs[j].Blur()
		}
	}
}

// domain reads the form into a domain, or returns the field rule it breaks.
func (f form) domain() (store.Domain, error) {
	port, err := store.ParsePort(f.inputs[fieldPort].Value())
	if err != nil {
		return store.Domain{}, err
	}
	enabled, https, root := true, false, ""
	if f.editing != "" {
		enabled, https, root = f.original.Enabled, f.original.HTTPS, f.original.Root
	}
	return store.Domain{
		Name:    f.name(),
		Address: strings.TrimSpace(f.inputs[fieldAddress].Value()),
		Port:    port,
		Enabled: enabled,
		Compose: f.compose(),
		HTTPS:   https,
		Root:    root,
	}, nil
}

// compose reads the compose field: empty, none, or a path, which ~/ starts at
// the home folder and a relative one at the folder lodo started in.
func (f form) compose() string {
	v := strings.TrimSpace(f.inputs[fieldCompose].Value())
	switch {
	case v == "" || v == store.NoCompose:
		return v
	case strings.HasPrefix(v, "~/") && f.origin.Home != "":
		v = filepath.Join(f.origin.Home, v[2:])
	case !filepath.IsAbs(v) && f.origin.Dir != "":
		v = filepath.Join(f.origin.Dir, v)
	}
	return filepath.Clean(v)
}

// name is the typed labels with the suffix, or empty when none are typed. A
// suffix typed out of habit is not doubled.
func (f form) name() string {
	labels := strings.TrimSuffix(strings.TrimSpace(f.inputs[fieldName].Value()), f.suffix)
	if labels == "" {
		return ""
	}
	return labels + f.suffix
}

// setError shows err under the field it concerns and moves the cursor there.
func (f *form) setError(err error) {
	f.errs = [fieldCount]string{}
	field := fieldName
	if fe, ok := errors.AsType[*store.FieldError](err); ok {
		field = fieldIndex[fe.Field]
	}
	f.errs[field] = err.Error()
	f.focusField(field)
}

// update handles a key the form owns: typing and moving between fields.
func (f form) update(msg tea.KeyPressMsg, domains []store.Domain) form {
	switch msg.String() {
	case "tab", "down":
		f.focusField(f.focus + 1)
		return f
	case "shift+tab", "up":
		f.focusField(f.focus - 1)
		return f
	}
	before := f.inputs[f.focus].Value()
	f.inputs[f.focus], _ = f.inputs[f.focus].Update(msg) // the cursor doesn't blink, so no command
	if f.inputs[f.focus].Value() == before {
		return f
	}
	f.errs[f.focus] = ""
	switch f.focus {
	case fieldName:
		f.prefill(domains)
	case fieldAddress:
		f.addressTouched = true
	}
	return f
}
