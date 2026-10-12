package compose

import (
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Service is a compose service: its image and the ports it publishes on this
// Mac, from the port in the container to the one on the Mac.
type Service struct {
	Image string
	Ports map[int]int
}

// Services returns the services of a compose file, in the file's order. A
// port with no fixed number on this Mac, such as a range or one a variable
// sets, is left out, and so is a port for anything but TCP. Like Rewrite, it
// skips a value an alias repeats.
func Services(src []byte) ([]Service, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}
	var services []Service
	for _, service := range pairs(servicesOf(&doc)) {
		s := Service{Ports: map[int]int{}}
		for key, value := range pairs(service) {
			switch key {
			case "image":
				s.Image = value.Value
			case "ports":
				for _, port := range value.Content {
					if container, host, ok := published(port); ok {
						s.Ports[container] = host
					}
				}
			}
		}
		services = append(services, s)
	}
	return services, nil
}

// published returns the container's port and the Mac's of one entry in a
// service's ports: a short one, such as "127.0.0.1:8025:8025/tcp", or a long
// one with target and published.
func published(port *yaml.Node) (container, host int, ok bool) {
	var containerText, hostText, protocol string
	switch port.Kind {
	case yaml.ScalarNode:
		parts := splitPort(port.Value)
		if len(parts) < 2 {
			return 0, 0, false // only the container's port: Docker picks the Mac's
		}
		hostText = parts[len(parts)-2]
		containerText, protocol, _ = strings.Cut(parts[len(parts)-1], "/")
	case yaml.MappingNode:
		for key, value := range pairs(port) {
			switch key {
			case "target":
				containerText = value.Value
			case "published":
				hostText = value.Value
			case "protocol":
				protocol = value.Value
			}
		}
	}
	if protocol != "" && protocol != "tcp" {
		return 0, 0, false
	}
	container, errContainer := strconv.Atoi(containerText)
	host, errHost := strconv.Atoi(hostText)
	return container, host, errContainer == nil && errHost == nil && container > 0 && host > 0
}
