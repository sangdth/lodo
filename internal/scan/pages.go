package scan

import (
	"maps"
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
)

// Page is a web page a project's compose file publishes on this Mac that lodo
// names, such as Mailpit's inbox.
type Page struct {
	Label string // its name's first label: mail for mail.shop.test
	Port  int    // the port on this Mac
}

// webImages are the images whose web page lodo names: Mailpit's inbox, on
// port 8025 in the container, gets mail.<project>.
var webImages = []struct {
	image string // without registry or tag
	label string
	port  int
}{
	{image: "axllent/mailpit", label: "mail", port: 8025},
}

// readCompose returns the pages of the project's best compose file and the
// ports on this Mac it binds to DOCKER_HOST_IP. A file it can't read or parse
// has none.
func readCompose(file string) (pages []Page, bound map[int]bool) {
	data, err := fsutil.ReadRegular(file)
	if err != nil {
		return nil, nil
	}
	services, _ := compose.Services(data)
	bound = map[int]bool{}
	for _, s := range services {
		maps.Copy(bound, s.Bound)
		for _, w := range webImages {
			if port, ok := s.Ports[w.port]; ok && isImage(s.Image, w.image) {
				pages = append(pages, Page{Label: w.label, Port: port})
			}
		}
	}
	return pages, bound
}

// isImage reports whether image is repo, from any registry and at any tag or
// digest: ghcr.io/axllent/mailpit:v1.21 is axllent/mailpit.
func isImage(image, repo string) bool {
	slash := strings.LastIndexByte(image, '/')
	if i := strings.IndexAny(image[slash+1:], ":@"); i >= 0 {
		image = image[:slash+1+i]
	}
	return image == repo || strings.HasSuffix(image, "/"+repo)
}
