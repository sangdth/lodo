// Package dnsmasq generates oo's dnsmasq config and resolver list, and tails
// dnsmasq's query log.
package dnsmasq

import (
	"fmt"
	"strings"

	"github.com/sangdth/oo/internal/store"
)

// ListenAddress is the address oo's dnsmasq listens on.
const ListenAddress = "127.0.0.1"

// Port is the port oo's dnsmasq listens on. It is above 1024, so dnsmasq
// runs as the user, without root.
const Port = 53535

// Config returns oo's dnsmasq config: log every query to logPath, and answer
// each enabled domain's name with its address, in store.Sort order. dnsmasq
// answers a name from the most specific address= line that matches it, so a
// subdomain with a line of its own gets its own address and an unlisted one
// gets its parent's. Only names under an /etc/resolver file reach dnsmasq.
func Config(domains []store.Domain, logPath string) string {
	var b strings.Builder
	b.WriteString(store.GeneratedHeader)
	b.WriteString("log-queries\n")
	b.WriteString("log-facility=" + logPath + "\n")
	for _, d := range store.Sort(domains) {
		if d.Enabled {
			fmt.Fprintf(&b, "address=/%s/%s\n", d.Name, d.Address)
		}
	}
	return b.String()
}

// ResolverList returns the enabled names, one per line, in store.Sort order.
// The root script reads it and counts any line that is not a name as skipped,
// so it has no header. With no enabled domain it is empty.
func ResolverList(domains []store.Domain) string {
	var b strings.Builder
	for _, d := range store.Sort(domains) {
		if d.Enabled {
			b.WriteString(d.Name + "\n")
		}
	}
	return b.String()
}
