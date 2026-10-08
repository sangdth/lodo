package tui

import (
	"errors"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lcd/internal/store"
)

// The form's fields, in tab order.
const (
	fieldName = iota
	fieldAddress
	fieldPort
	fieldCount
)

// fieldIndex maps store's field names to the form's fields.
var fieldIndex = map[string]int{
	store.FieldName:    fieldName,
	store.FieldAddress: fieldAddress,
	store.FieldPort:    fieldPort,
}

// form adds a domain, or edits one when editing names it.
type form struct {
	editing        string
	original       store.Domain // the domain being edited
	inputs         [fieldCount]textinput.Model
	focus          int
	addressTouched bool   // typing in the address field stops the prefill
	hint           string // shown after the address
	errs           [fieldCount]string
}

// newAddForm opens an empty form whose address follows the name.
func newAddForm(domains []store.Domain) form {
	f := form{inputs: newInputs()}
	f.prefill(domains)
	f.focusField(fieldName)
	return f
}

// newEditForm opens a form holding d. Its address stays as typed.
func newEditForm(domains []store.Domain, d store.Domain) form {
	f := form{editing: d.Name, original: d, inputs: newInputs(), addressTouched: true}
	f.inputs[fieldName].SetValue(d.Name)
	f.inputs[fieldAddress].SetValue(d.Address)
	if d.Port > 0 {
		f.inputs[fieldPort].SetValue(strconv.Itoa(d.Port))
	}
	if free, err := store.NextFree(domains); err == nil {
		f.hint = "next free: " + free
	}
	f.focusField(fieldName)
	return f
}

func newInputs() [fieldCount]textinput.Model {
	styles := textinput.DefaultDarkStyles()
	styles.Cursor.Blink = false
	var inputs [fieldCount]textinput.Model
	for i, limit := range [fieldCount]int{store.MaxNameLen, len("127.255.255.255"), len("65535")} {
		inputs[i] = textinput.New()
		inputs[i].Prompt = ""
		inputs[i].CharLimit = limit
		inputs[i].SetStyles(styles)
	}
	inputs[fieldName].SetWidth(40)
	inputs[fieldName].Placeholder = "app.flowy.lcd"
	inputs[fieldAddress].SetWidth(16)
	inputs[fieldPort].SetWidth(6)
	inputs[fieldPort].Placeholder = "none"
	return inputs
}

// prefill fills the address while the name is typed: the parent's address
// for a subdomain of a listed name, otherwise the lowest free own address.
func (f *form) prefill(domains []store.Domain) {
	if f.addressTouched {
		return
	}
	name := strings.TrimSpace(f.inputs[fieldName].Value())
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
	enabled := true
	if f.editing != "" {
		enabled = f.original.Enabled
	}
	return store.Domain{
		Name:    strings.TrimSpace(f.inputs[fieldName].Value()),
		Address: strings.TrimSpace(f.inputs[fieldAddress].Value()),
		Port:    port,
		Enabled: enabled,
	}, nil
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
