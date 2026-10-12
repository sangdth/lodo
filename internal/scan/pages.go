package scan

import (
	"strings"

	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/fsutil"
)

// Page is a web page a project's compose file publishes on this Mac that lodo
// names, such as Mailpit's inbox.
type Page struct {
	Label string `json:"label"` // its name's first label: mail for mail.shop.test
	Port  int    `json:"port"`  // the port on this Mac
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

// findPages returns the pages of the project's best compose file, the first
// of files. A file it can't read or parse has none.
func findPages(files []string) []Page {
	if len(files) == 0 {
		return nil
	}
	data, err := fsutil.ReadRegular(files[0], fsutil.MaxProjectFile)
	if err != nil {
		return nil
	}
	services, _ := compose.Services(data)
	var pages []Page
	for _, s := range services {
		for _, w := range webImages {
			if port, ok := s.Ports[w.port]; ok && isImage(s.Image, w.image) {
				pages = append(pages, Page{Label: w.label, Port: port})
			}
		}
	}
	return pages
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
