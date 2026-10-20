package compose

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type renderedDoc struct {
	Services map[string]struct {
		NetworkMode string `yaml:"network_mode"`
		Ports       []any  `yaml:"ports"`
	} `yaml:"services"`
}

func publicBindings(doc renderedDoc) []string {
	var out []string
	for name, svc := range doc.Services {
		if svc.NetworkMode == networkHost {
			out = append(out, name+": network_mode host")
		}
		for _, p := range svc.Ports {
			if !loopbackBinding(p) {
				out = append(out, name)
			}
		}
	}
	return out
}

func loopbackBinding(p any) bool {
	switch v := p.(type) {
	case string:
		return strings.HasPrefix(v, loopback+portSeparator)
	case map[string]any:
		return v[keyHostIP] == loopback
	case map[any]any:
		return v[keyHostIP] == loopback
	default:
		return false
	}
}

func FuzzRender(f *testing.F) {
	f.Add([]byte("services:\n  api:\n    build: .\n    ports: ['127.0.0.1:9100:8000', 8080]\n  db:\n    image: postgres:17\n    ports: ['5432:5432']\n"))
	f.Add([]byte("services:\n  web:\n    image: x\n    ports: ['0.0.0.0:80:80']\n"))
	f.Add([]byte("services:\n  web:\n    build: .\n    ports: [{target: 80, published: 9000}]\n    network_mode: host\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Render("web", data)
		if err != nil {
			return
		}
		var doc renderedDoc
		if err := yaml.Unmarshal(res.Compose, &doc); err != nil {
			return
		}
		if bad := publicBindings(doc); len(bad) > 0 {
			t.Fatalf("rendered compose publishes on public interfaces %v:\n%s", bad, res.Compose)
		}
	})
}
