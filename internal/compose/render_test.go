package compose

import (
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func render(t *testing.T, src string) (Result, map[string]any) {
	t.Helper()
	r, err := Render("myapp", []byte(src))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(r.Compose, &doc); err != nil {
		t.Fatalf("output is not yaml: %v\n%s", err, r.Compose)
	}
	return r, doc
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a mapping: %#v", v)
	}
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	return l
}

func svc(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	return asMap(t, asMap(t, doc["services"])[name])
}

const (
	restartAlways = "always"
	driverLocal   = "local"
)

func TestRenderBuiltServiceWithSidecar(t *testing.T) {
	r, doc := render(t, `
services:
  api:
    build: ./api
    ports: ["127.0.0.1:9005:8000"]
    env_file: [.env.local]
  redis:
    image: redis:7
    mem_limit: 64m
    restart: always
    stop_grace_period: 5s
`)
	testutil.Equal(t, r.Metadata(), "main=api\nport=9005\npublic=true\nhealth=http\ncontext=./api\ndockerfile=api/Dockerfile\n")
	api := svc(t, doc, "api")
	_, hasBuild := api["build"]
	testutil.True(t, !hasBuild, "build must be removed")
	testutil.Equal(t, []any{api["image"], api["container_name"], api["restart"], api["mem_limit"]}, []any{ImagePlaceholder, "myapp", DefaultRestart, DefaultMemLimit})
	testutil.Equal(t, asList(t, api["env_file"]), []any{".env.local", EnvFile})
	logging := asMap(t, api["logging"])
	testutil.Equal(t, logging["driver"], DefaultLogDriver)
	testutil.Equal(t, asMap(t, logging["options"])["max-file"], "3")
	redis := svc(t, doc, "redis")
	testutil.Equal(t, []any{redis["mem_limit"], redis["restart"], redis["image"]}, []any{"64m", restartAlways, "redis:7"})
	testutil.Equal(t, []any{api["stop_grace_period"], redis["stop_grace_period"]}, []any{DefaultStopGrace, "5s"})
}

func TestOverlapLabel(t *testing.T) {
	src := []byte("services:\n  bot:\n    labels:\n      deploy-hub.overlap: \"false\"\n  web:\n    labels: [deploy-hub.overlap=0]\n  api:\n    labels: {deploy-hub.overlap: \"true\"}\n  plain: {}\n")
	type label struct {
		value string
		set   bool
	}
	for service, want := range map[string]label{"bot": {boolFalse, true}, "web": {"0", true}, "api": {boolTrue, true}, "plain": {}, "missing": {}} {
		v, ok := OverlapLabel(src, service)
		testutil.Equal(t, label{v, ok}, want)
	}
	_, ok := OverlapLabel([]byte("not: [valid"), "bot")
	testutil.True(t, !ok, "unreadable compose has no label")
	for label, want := range map[string]bool{"": true, boolTrue: true, "False": false, "No": false, "0": false, "maybe": true} {
		testutil.Equal(t, AllowsOverlap(label), want)
	}
}

func TestRenderLabelsAndHealth(t *testing.T) {
	r, doc := render(t, `
services:
  worker:
    image: x
    labels: ["deploy-hub.main=true", "deploy-hub.public=false"]
    healthcheck: {test: ["CMD", "true"]}
    deploy: {resources: {limits: {memory: 32m}}}
    env_file: .env
  web:
    build: .
    labels: {deploy-hub.host: "true"}
    ports: ["8080"]
`)
	testutil.Equal(t, r.Metadata(), "main=worker\nport=\npublic=false\nhealth=compose\ncontext=.\ndockerfile=Dockerfile\n")
	w := svc(t, doc, "worker")
	_, hasMem := w["mem_limit"]
	testutil.True(t, !hasMem, "deploy: block means no mem_limit default")
	testutil.Equal(t, asList(t, w["env_file"]), []any{EnvFile})
}

func TestRenderHostLabelAndLongPorts(t *testing.T) {
	r, _ := render(t, `
services:
  app:
    build: {context: ./svc, dockerfile: docker/Dockerfile.prod}
    ports: [{target: 80, published: 0}]
  proxy:
    image: nginx
    labels: {deploy-hub.host: "yes"}
    ports: [{target: 80}, {target: 80, published: 9100}]
`)
	if r.Port != "9100" || r.Context != "./svc" || r.Dockerfile != "svc/docker/Dockerfile.prod" || r.Health != HealthHTTP {
		t.Fatalf("meta: %+v", r)
	}
}

func TestRenderProcessModeAndScalarEnv(t *testing.T) {
	r, doc := render(t, `
services:
  bot:
    image: x
    env_file: secrets.env
    build: {dockerfile: docker/Dockerfile}
`)
	if r.Health != HealthProcess || r.Port != "" || r.Dockerfile != "docker/Dockerfile" || r.Context != "." {
		t.Fatalf("meta: %+v", r)
	}
	if ef := asList(t, svc(t, doc, "bot")["env_file"]); len(ef) != 2 || ef[0] != "secrets.env" {
		t.Fatalf("env_file: %v", ef)
	}
}

func TestRenderEmptyEnvFileAndShortPorts(t *testing.T) {
	r, doc := render(t, `
services:
  a:
    build: .
    env_file: ""
    ports: ["", "9300:80/tcp"]
`)
	if r.Port != "9300" {
		t.Fatalf("port: %q", r.Port)
	}
	if ef := asList(t, svc(t, doc, "a")["env_file"]); len(ef) != 1 {
		t.Fatalf("env_file: %v", ef)
	}
}

func TestRenderResolvesAnchorsAndMergeKeys(t *testing.T) {
	r, doc := render(t, `
x-base: &base
  restart: always
  logging: &log
    driver: local
x-mem: &mem
  mem_limit: 99m
services:
  web:
    <<: [*base, *mem]
    build: .
    healthcheck: {test: ["CMD", "true"]}
  db:
    image: postgres
    logging: *log
    labels:
      "<<": literal
`)
	web := svc(t, doc, "web")
	testutil.Equal(t, []any{web["restart"], web["mem_limit"], asMap(t, web["logging"])["driver"]}, []any{restartAlways, "99m", driverLocal})
	_, hasMerge := web["<<"]
	testutil.True(t, !hasMerge, "merge key must be expanded")
	testutil.Equal(t, asMap(t, svc(t, doc, "db")["logging"])["driver"], driverLocal)
	testutil.Excludes(t, string(r.Compose), "&", "*log")
}

func TestRenderErrors(t *testing.T) {
	cases := map[string]error{
		"services: [":         ErrCannotParse,
		"- a\n- b\n":          ErrNotMapping,
		"":                    ErrNoServices,
		"services: {}\n":      ErrNoServices,
		"services: []\n":      ErrNoServices,
		"version: '3'\n":      ErrNoServices,
		"services:\n  a: 1\n": ErrServiceShape,
		"services:\n  a: {build: .}\n  b: {build: .}":                                             ErrManyBuilt,
		"services:\n  a: {image: x}\n  b: {image: y}":                                             ErrNoMain,
		"services:\n  a: {labels: {deploy-hub.main: 'true'}}\n  b: {labels: [deploy-hub.main=1]}": ErrManyMain,
	}
	for src, want := range cases {
		if _, err := Render("x", []byte(src)); !errors.Is(err, want) {
			t.Errorf("%q: got %v want %v", src, err, want)
		}
	}
}

func TestRenderRejectsUnsafeOutputs(t *testing.T) {
	cases := map[string]error{
		"services:\n  \"web\\nbyoc=0\": {build: .}\n":                        ErrBadMain,
		"services:\n  web: {build: ., ports: ['9000-9001:80']}\n":            ErrBadPort,
		"services:\n  web: {build: ../outside}\n":                            ErrBadBuildPath,
		"services:\n  web: {build: {context: ., dockerfile: /etc/passwd}}\n": ErrBadBuildPath,
		"services:\n  web: {build: \"a\\nb\"}\n":                             ErrBadBuildPath,
	}
	for src, want := range cases {
		_, err := Render("x", []byte(src))
		testutil.Is(t, err, want)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		src  string
		want Summary
	}{
		{"services:\n  a: {build: ., ports: ['127.0.0.1:9001:3000/tcp']}\n  b: {image: x}\n", Summary{Built: 1, ContainerPort: "3000"}},
		{"services:\n  a: {build: .}\n  b: {build: .}\n", Summary{Built: 2}},
		{"services:\n  a: {build: ., ports: [{target: 5000}]}\n", Summary{Built: 1, ContainerPort: "5000"}},
		{"services:\n  a: {build: ., ports: [{published: 1}]}\n", Summary{Built: 1}},
		{"services:\n  a: {build: .}\n", Summary{Built: 1}},
	}
	for _, c := range cases {
		got, err := Summarize([]byte(c.src))
		testutil.NoErr(t, err)
		testutil.Equal(t, got, c.want)
	}
	_, err := Summarize([]byte("["))
	testutil.Fails(t, err)
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"true", " TRUE ", "1", "yes", "Yes"} {
		if !truthy(v) {
			t.Errorf("%q should be truthy", v)
		}
	}
	for _, v := range []string{"", "false", "0", "no", "on"} {
		if truthy(v) {
			t.Errorf("%q should be falsy", v)
		}
	}
}

func TestNodeHelpersOnNonMapping(t *testing.T) {
	if get(nil, "a") != nil || get(str("x"), "a") != nil {
		t.Fatal("get on non-mapping")
	}
	m := mapping(str("a"), str("1"))
	remove(m, "missing")
	if len(m.Content) != 2 {
		t.Fatal("remove of missing key")
	}
	if resolve(nil) != nil {
		t.Fatal("resolve nil")
	}
}

func TestRenderBindsEveryPortToLoopback(t *testing.T) {
	src := "services:\n  web:\n    build: .\n    ports: ['9100:8000', 8080, '127.0.0.1:9200:90', {target: 80, published: 9300}, {target: 81, host_ip: 127.0.0.1}]\n" +
		"  db:\n    image: postgres:17\n    ports: ['5432:5432']\n"
	res, err := Render("web", []byte(src))
	testutil.NoErr(t, err)
	out := string(res.Compose)
	for _, want := range []string{`"127.0.0.1:9100:8000"`, `"127.0.0.1::8080"`, "127.0.0.1:9200:90", "host_ip: 127.0.0.1", `"127.0.0.1:5432:5432"`} {
		testutil.Contains(t, out, want)
	}
	testutil.Equal(t, strings.Count(out, "host_ip: 127.0.0.1"), 2)
	testutil.Equal(t, res.Port, "9100")
}

func TestRenderRejectsPublicInterfaces(t *testing.T) {
	cases := map[string]error{
		"services:\n  web: {build: ., ports: ['0.0.0.0:80:80']}\n":                        ErrPublicBind,
		"services:\n  web: {build: .}\n  db: {image: x, ports: ['10.0.0.5:5432:5432']}\n": ErrPublicBind,
		"services:\n  web: {build: ., ports: ['[::]:80:80']}\n":                           ErrPublicBind,
		"services:\n  web: {build: ., ports: [{target: 80, host_ip: 0.0.0.0}]}\n":         ErrPublicBind,
		"services:\n  web: {build: ., network_mode: host}\n":                              ErrHostNetwork,
	}
	for src, want := range cases {
		_, err := Render("x", []byte(src))
		testutil.Is(t, err, want)
	}
	res, err := Render("x", []byte("services:\n  web: {build: ., network_mode: bridge}\n"))
	testutil.NoErr(t, err)
	testutil.Contains(t, string(res.Compose), "network_mode: bridge")
}

func TestRenderListLabelsAndExactLayout(t *testing.T) {
	src := "services:\n  web:\n    build: .\n    ports: ['9100:80']\n    labels: [\"deploy-hub.public = false\", \"other\"]\n"
	res, err := Render("web", []byte(src))
	testutil.NoErr(t, err)
	testutil.Equal(t, res.Port, "")
	testutil.True(t, !res.Public, "list-form label opts out of the route")
	testutil.True(t, strings.HasPrefix(string(res.Compose), "services:\n  web:\n    ports: [\"127.0.0.1:9100:80\"]\n"), string(res.Compose))
	plain, err := Render("web", []byte("services:\n  web:\n    build: .\n    ports: [{target: 80}]\n"))
	testutil.NoErr(t, err)
	testutil.Equal(t, plain.Port, "")
	testutil.True(t, plain.Public, "no labels means public")
	_, err = Summarize([]byte("[not, a, mapping]"))
	testutil.Is(t, err, ErrNotMapping)
}
