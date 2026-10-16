package compose_test

import (
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/sangdth/lodo/internal/compose"
)

func TestServices(t *testing.T) {
	t.Parallel()

	shapes, err := os.ReadFile("testdata/compose.dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	postgres := "postgres:17-alpine"
	tests := []struct {
		name string
		src  string
		want []compose.Service
	}{
		{
			name: "the shapes in ~/Projects",
			src:  string(shapes),
			want: []compose.Service{
				{Image: postgres, Ports: map[int]int{5432: 5432}},
				{Image: postgres, Ports: map[int]int{5432: 5432}},
				{Image: postgres}, // a variable sets the Mac's port
				{Image: postgres, Ports: map[int]int{5432: 5433}, Bound: map[int]bool{5433: true}},
				{Image: "redis:8-alpine", Ports: map[int]int{6379: 6379}},
				{Image: "minio/minio:latest", Ports: map[int]int{9000: 9000, 9001: 9001}},
				{Ports: map[int]int{3000: 3001}}, // built, not pulled; "3000" alone has no port on the Mac
				{Image: "ghcr.io/flowy/worker:latest"},
				{Image: "nginx:alpine", Ports: map[int]int{80: 8080, 443: 8443}},
			},
		},
		{
			name: "an IPv6 address",
			src:  yml("services:", "  mail:", "    ports:", `      - "[::1]:8025:8025"`),
			want: []compose.Service{{Ports: map[int]int{8025: 8025}}},
		},
		{
			name: "udp and ranges",
			src: yml("services:", "  dns:", "    ports:", `      - "53:53/udp"`, `      - "8000-8010:8000-8010"`,
				"      - target: 5353", "        published: 5353", "        protocol: udp"),
			want: []compose.Service{{}},
		},
		{
			name: "bound to DOCKER_HOST_IP",
			src: yml("services:", "  db:", "    ports:", `      - "${DOCKER_HOST_IP:-127.0.0.1}:5434:5432"`,
				"      - target: 6379", "        published: 6380", "        host_ip: ${DOCKER_HOST_IP}"),
			want: []compose.Service{{Ports: map[int]int{5432: 5434, 6379: 6380}, Bound: map[int]bool{5434: true, 6380: true}}},
		},
		{
			name: "bound to another address",
			src: yml("services:", "  db:", "    ports:", `      - "127.0.0.1:5434:5432"`,
				`      - "${DOCKER_HOST_IPX}:6380:6379"`),
			want: []compose.Service{{Ports: map[int]int{5432: 5434, 6379: 6380}}},
		},
		{name: "no services", src: yml("name: flowy")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := compose.Services([]byte(tt.src))
			if err != nil {
				t.Fatalf("Services: %v", err)
			}
			if !slices.EqualFunc(got, tt.want, func(a, b compose.Service) bool {
				return a.Image == b.Image && maps.Equal(a.Ports, b.Ports) && maps.Equal(a.Bound, b.Bound)
			}) {
				t.Errorf("Services =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestServices_InvalidYAML(t *testing.T) {
	t.Parallel()

	if _, err := compose.Services([]byte("services: [")); err == nil {
		t.Error("Services on invalid YAML: no error")
	}
}
