package compose

import (
	"bytes"
	"cmp"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Values are what Rewrite puts in place of localhost.
type Values struct {
	Domain string         // the project's name, such as flowy.test
	Names  map[int]string // names Caddy serves on the project's address, by port: 3000 → dashboard.flowy.test
}

// Kind says what a Change rewrote.
type Kind string

const (
	KindPort Kind = "port" // a port now binds HostIP
	KindURL  Kind = "url"  // a localhost URL now names the project
)

// Change is one value Rewrite changed.
type Change struct {
	Line     int    // 1-based, in the original file; Rewrite keeps every line where it was
	Service  string // the compose service the value belongs to
	Kind     Kind
	Old, New string // the value before and after, without quotes
}

// urlRE matches an http or ws URL, secure or not, on this Mac's own host. The
// groups are the scheme and the port, when it has one.
var urlRE = regexp.MustCompile(`(?i)\b(https?|wss?)://(?:localhost|127\.0\.0\.1)(?::(\d{1,5}))?`)

// hostChars are the bytes that, right after a match of urlRE, show its host
// goes on, as in localhost.example.com or localhost@example.com. Such a URL
// is not on this Mac.
const hostChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz.-_@"

// Rewrite returns src with the project's ports bound to HostIP and its
// localhost URLs pointing at its .test names, and the changes it made.
//
// It visits only each service's ports and environment: inside a container
// localhost is the container itself, so a healthcheck or command that calls
// localhost stays. It edits the text in place instead of writing the YAML
// out again, so comments, quotes and line numbers stay too. A value it can't
// find exactly in the text stays as it is: a block scalar, a value over more
// than one line, or one with an escape, an anchor or a tag. So does a whole
// file that breaks lines with anything but \n and \r\n.
func Rewrite(src []byte, v Values) ([]byte, []Change, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse compose file: %w", err)
	}
	// The parser also breaks lines at a lone \r, NEL, LS and PS, so in such a
	// file its line numbers miss the file's lines.
	if hasOddBreaks(src) {
		return bytes.Clone(src), nil, nil
	}
	r := rewriter{src: src, lines: lineStarts(src), values: v}
	for name, service := range pairs(servicesOf(&doc)) {
		for key, value := range pairs(service) {
			switch key {
			case "ports":
				r.ports(name, value)
			case "environment":
				r.environment(name, value)
			}
		}
	}
	out, changes := r.apply()
	return out, changes, nil
}

// servicesOf returns the document's top-level services mapping, or nil.
func servicesOf(doc *yaml.Node) *yaml.Node {
	if len(doc.Content) == 0 {
		return nil
	}
	for key, value := range pairs(doc.Content[0]) {
		if key == "services" {
			return value
		}
	}
	return nil
}

// pairs yields each key of mapping n with its value. It skips a value that is
// an alias: an alias repeats text from elsewhere and has none of its own.
func pairs(n *yaml.Node) iter.Seq2[string, *yaml.Node] {
	return func(yield func(string, *yaml.Node) bool) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if value.Kind != yaml.AliasNode && !yield(key.Value, value) {
				return
			}
		}
	}
}

// rewriter collects the edits Rewrite makes to src.
type rewriter struct {
	src    []byte
	lines  []int // where each line of src starts
	values Values
	edits  []edit
}

// edit puts text in place of src[start:end].
type edit struct {
	start, end int
	text       string
	change     Change
}

// ports binds each port in a service's ports list to HostIP.
func (r *rewriter) ports(service string, list *yaml.Node) {
	if list.Kind != yaml.SequenceNode {
		return
	}
	for _, port := range list.Content {
		switch port.Kind {
		case yaml.ScalarNode:
			if spec, ok := bindPort(port.Value); ok {
				r.replaceToken(service, port, spec)
			}
		case yaml.MappingNode:
			for key, ip := range pairs(port) {
				if key == "host_ip" && ip.Kind == yaml.ScalarNode && isLoopback(ip.Value) {
					r.replaceToken(service, ip, HostIP)
				}
			}
		}
	}
}

// environment points the localhost URLs in a service's environment, a
// mapping or a list of KEY=value, at the project.
func (r *rewriter) environment(service string, env *yaml.Node) {
	var values []*yaml.Node
	switch env.Kind {
	case yaml.MappingNode:
		for _, value := range pairs(env) {
			values = append(values, value)
		}
	case yaml.SequenceNode:
		values = env.Content
	}
	for _, value := range values {
		if value.Kind != yaml.ScalarNode {
			continue
		}
		if s, ok := pointURLs(value.Value, r.values); ok {
			r.replaceText(service, value, s)
		}
	}
}

// replaceToken puts a port's new spec in place of its whole token, in double
// quotes whatever the token's style: HostIP holds { and }, which a plain
// token in a flow list can't.
func (r *rewriter) replaceToken(service string, n *yaml.Node, spec string) {
	tok, ok := r.locate(n)
	if !ok || strings.ContainsAny(spec, `"\`) {
		return
	}
	r.edits = append(r.edits, edit{
		start:  tok.start,
		end:    tok.end,
		text:   `"` + spec + `"`,
		change: Change{Line: n.Line, Service: service, Kind: KindPort, Old: n.Value, New: spec},
	})
}

// replaceText puts a value with its URLs rewritten in place of the text
// inside its token's quotes, so the token keeps its style.
func (r *rewriter) replaceText(service string, n *yaml.Node, s string) {
	tok, ok := r.locate(n)
	if !ok {
		return
	}
	r.edits = append(r.edits, edit{
		start:  tok.start + tok.quote,
		end:    tok.end - tok.quote,
		text:   s,
		change: Change{Line: n.Line, Service: service, Kind: KindURL, Old: n.Value, New: s},
	})
}

// token is where a scalar sits in src: src[start:end], with quote bytes of
// quote mark on each side.
type token struct {
	start, end, quote int
}

// locate finds n's token in src. It finds only a scalar on one line whose
// text is exactly n.Value: plain, or quoted with no escape inside. Anything
// else, it leaves alone.
func (r *rewriter) locate(n *yaml.Node) (token, bool) {
	var q string
	switch {
	case n.Anchor != "" || strings.ContainsAny(n.Value, "\r\n"):
		return token{}, false // an anchor's text is also every alias's
	case n.Style == 0 && n.Value != "":
	case n.Style == yaml.DoubleQuotedStyle && !strings.ContainsAny(n.Value, `"\`):
		q = `"`
	case n.Style == yaml.SingleQuotedStyle && !strings.Contains(n.Value, "'"):
		q = "'"
	default:
		return token{}, false
	}
	start, ok := r.offset(n.Line, n.Column)
	text := q + n.Value + q
	if !ok || !bytes.HasPrefix(r.src[start:], []byte(text)) {
		return token{}, false
	}
	return token{start: start, end: start + len(text), quote: len(q)}, true
}

// offset returns where a 1-based line and column sit in src. The column
// counts characters, not bytes.
func (r *rewriter) offset(line, col int) (int, bool) {
	if line < 1 || line > len(r.lines) || col < 1 {
		return 0, false
	}
	i := r.lines[line-1]
	for range col - 1 {
		if i >= len(r.src) || r.src[i] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRune(r.src[i:])
		i += size
	}
	return i, true
}

// apply returns src with every edit made, and the changes in file order.
// Each edit replaces one scalar's token, so no two overlap.
func (r *rewriter) apply() ([]byte, []Change) {
	slices.SortFunc(r.edits, func(a, b edit) int { return cmp.Compare(a.start, b.start) })
	out := make([]byte, 0, len(r.src))
	var changes []Change
	last := 0
	for _, e := range r.edits {
		out = append(out, r.src[last:e.start]...)
		out = append(out, e.text...)
		last = e.end
		changes = append(changes, e.change)
	}
	return append(out, r.src[last:]...), changes
}

// hasOddBreaks reports whether src breaks a line with anything but \n or
// \r\n.
func hasOddBreaks(src []byte) bool {
	if bytes.ContainsAny(src, "\u0085\u2028\u2029") {
		return true
	}
	for i, b := range src {
		if b == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
			return true
		}
	}
	return false
}

// lineStarts returns where each line of src starts. Line 1 starts after a
// byte order mark: the parser counts no column for it.
func lineStarts(src []byte) []int {
	starts := []int{0}
	if bytes.HasPrefix(src, []byte(bom)) {
		starts[0] = len(bom)
	}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// bom is the UTF-8 byte order mark.
const bom = "\uFEFF"

// bindPort returns a short-syntax port bound to HostIP, and false for a port
// that stays: one without a host port, or bound to an address other than
// loopback, such as 0.0.0.0 or HostIP itself.
func bindPort(spec string) (string, bool) {
	parts := splitPort(spec)
	switch {
	case len(parts) == 2: // host port and container port
		return HostIP + ":" + spec, true
	case len(parts) == 3 && isLoopback(parts[0]): // address, host port and container port
		return HostIP + spec[len(parts[0]):], true
	}
	return "", false
}

// splitPort splits a short-syntax port on the colons outside ${...} and
// [...]. A /protocol suffix stays on the last part.
func splitPort(spec string) []string {
	var parts []string
	vars, brackets, start := 0, 0, 0
	for i := 0; i < len(spec); i++ {
		switch c := spec[i]; {
		case c == '$' && i+1 < len(spec) && spec[i+1] == '{':
			vars++
			i++
		case c == '}' && vars > 0:
			vars--
		case c == '[':
			brackets++
		case c == ']' && brackets > 0:
			brackets--
		case c == ':' && vars == 0 && brackets == 0:
			parts = append(parts, spec[start:i])
			start = i + 1
		}
	}
	return append(parts, spec[start:])
}

// isLoopback reports whether a port's address is this Mac's own.
func isLoopback(addr string) bool {
	return addr == "127.0.0.1" || addr == "localhost"
}

// pointURLs returns s with each localhost URL in it pointing at the project,
// and false when s has none. An http or ws URL on a port Caddy serves a name
// for gets that name without the port, since Caddy serves it on port 80. Any
// other URL gets the project's name and keeps its port.
func pointURLs(s string, v Values) (string, bool) {
	var b strings.Builder
	last := 0
	for _, m := range urlRE.FindAllStringSubmatchIndex(s, -1) {
		if m[1] < len(s) && strings.IndexByte(hostChars, s[m[1]]) >= 0 {
			continue
		}
		scheme, host, port := s[m[2]:m[3]], v.Domain, ""
		if m[4] >= 0 {
			port = s[m[4]:m[5]]
		}
		plain := strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "ws")
		if n, err := strconv.Atoi(port); plain && err == nil && v.Names[n] != "" {
			host, port = v.Names[n], ""
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(scheme + "://" + host)
		if port != "" {
			b.WriteString(":" + port)
		}
		last = m[1]
	}
	if last == 0 {
		return "", false
	}
	b.WriteString(s[last:])
	return b.String(), true
}
