package compose

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	ImagePlaceholder  = "${DEPLOY_IMAGE:?set by deploy runner}"
	DefaultMemLimit   = "256m"
	DefaultRestart    = "unless-stopped"
	DefaultLogDriver  = "json-file"
	DefaultLogSize    = "10m"
	DefaultLogFiles   = "3"
	DefaultContext    = "."
	DefaultDockerfile = "Dockerfile"
	EnvFile           = ".env"

	LabelMain   = "deploy-hub.main"
	LabelPublic = "deploy-hub.public"
	LabelHost   = "deploy-hub.host"

	HealthCompose = "compose"
	HealthHTTP    = "http"
	HealthProcess = "process"

	keyServices      = "services"
	keyBuild         = "build"
	keyImage         = "image"
	keyContainerName = "container_name"
	keyEnvFile       = "env_file"
	keyRestart       = "restart"
	keyMemLimit      = "mem_limit"
	keyDeploy        = "deploy"
	keyLogging       = "logging"
	keyLabels        = "labels"
	keyPorts         = "ports"
	keyPublished     = "published"
	keyHealthcheck   = "healthcheck"
	keyContext       = "context"
	keyTarget        = "target"
	keyDockerfile    = "dockerfile"
	keyDriver        = "driver"
	keyOptions       = "options"
	keyMaxSize       = "max-size"
	keyMaxFile       = "max-file"
	yamlIndent       = 2
	hostIPPortParts  = 3
	boolTrue         = "true"
	boolFalse        = "false"
)

var (
	ErrNotMapping   = errors.New("compose is not a mapping (expected top-level services:)")
	ErrNoServices   = errors.New("no services in compose (services: must be a mapping of names)")
	ErrServiceShape = errors.New("service is not a mapping")
	ErrManyMain     = errors.New("more than one service labelled deploy-hub.main")
	ErrManyBuilt    = errors.New(`more than one service has build: — mark the main one with label deploy-hub.main: "true"`)
	ErrNoMain       = errors.New(`cannot determine the main service — add build: or the label deploy-hub.main: "true" to one service`)
	ErrCannotParse  = errors.New("cannot parse compose")
	ErrBadMain      = errors.New("main service name must match ^[a-zA-Z0-9._-]{1,64}$")
	ErrBadPort      = errors.New("published port of the exposed service must be a single port number")
	ErrBadBuildPath = errors.New("build context and dockerfile must be relative paths inside the repo")
)

type Result struct {
	Main       string
	Port       string
	Public     bool
	Health     string
	Context    string
	Dockerfile string
	Compose    []byte
}

func (r Result) Metadata() string {
	public := boolFalse
	if r.Public {
		public = boolTrue
	}
	return fmt.Sprintf("main=%s\nport=%s\npublic=%s\nhealth=%s\ncontext=%s\ndockerfile=%s\n",
		r.Main, r.Port, public, r.Health, r.Context, r.Dockerfile)
}

type service struct {
	name string
	node *yaml.Node
}

type document struct {
	root     *yaml.Node
	services []service
}

func parse(data []byte) (document, error) {
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return document{}, fmt.Errorf("%w: %s", ErrCannotParse, err.Error())
	}
	root := mapping()
	if len(raw.Content) > 0 {
		root = resolve(raw.Content[0])
	}
	if root.Kind != yaml.MappingNode {
		return document{}, ErrNotMapping
	}
	svcs := get(root, keyServices)
	if svcs == nil || svcs.Kind != yaml.MappingNode || len(svcs.Content) == 0 {
		return document{}, ErrNoServices
	}
	doc := document{root: root}
	for _, e := range entries(svcs) {
		name, node := e.key.Value, e.value
		if node.Kind != yaml.MappingNode {
			return document{}, fmt.Errorf("%w: '%s'", ErrServiceShape, name)
		}
		doc.services = append(doc.services, service{name: name, node: node})
	}
	return doc, nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case boolTrue, "1", "yes":
		return true
	default:
		return false
	}
}

func labels(svc *yaml.Node) map[string]string {
	out := map[string]string{}
	l := get(svc, keyLabels)
	switch {
	case l == nil:
	case l.Kind == yaml.SequenceNode:
		for _, item := range l.Content {
			k, v, _ := strings.Cut(item.Value, "=")
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	default:
		for _, e := range entries(l) {
			out[e.key.Value] = e.value.Value
		}
	}
	return out
}

func filter(svcs []service, keep func(service) bool) []service {
	var out []service
	for _, s := range svcs {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func isBuilt(s service) bool {
	return has(s.node, keyBuild)
}

func pickMain(svcs []service) (service, error) {
	explicit := filter(svcs, func(s service) bool { return truthy(labels(s.node)[LabelMain]) })
	if len(explicit) > 1 {
		return service{}, ErrManyMain
	}
	if len(explicit) == 1 {
		return explicit[0], nil
	}
	built := filter(svcs, isBuilt)
	if len(built) > 1 {
		return service{}, ErrManyBuilt
	}
	if len(built) == 1 {
		return built[0], nil
	}
	if len(svcs) == 1 {
		return svcs[0], nil
	}
	return service{}, ErrNoMain
}

func publishedPort(svc *yaml.Node) string {
	ports := get(svc, keyPorts)
	if ports == nil {
		return ""
	}
	for _, p := range ports.Content {
		if v := portOf(p); v != "" {
			return v
		}
	}
	return ""
}

func portOf(p *yaml.Node) string {
	if p.Kind == yaml.MappingNode {
		if pub := get(p, keyPublished); pub != nil {
			return pub.Value
		}
		return ""
	}
	parts := strings.Split(p.Value, ":")
	if len(parts) >= hostIPPortParts {
		return parts[len(parts)-2]
	}
	return parts[0]
}

func containerPort(svc *yaml.Node) string {
	ports := get(svc, keyPorts)
	if ports == nil || len(ports.Content) == 0 {
		return ""
	}
	first := ports.Content[0]
	if first.Kind == yaml.MappingNode {
		if t := get(first, keyTarget); t != nil {
			return t.Value
		}
		return ""
	}
	parts := strings.Split(first.Value, ":")
	spec, _, _ := strings.Cut(parts[len(parts)-1], "/")
	return spec
}

func buildPaths(svc *yaml.Node) (string, string) {
	b := get(svc, keyBuild)
	if b == nil {
		return DefaultContext, DefaultDockerfile
	}
	if b.Kind == yaml.ScalarNode {
		return b.Value, path.Join(b.Value, DefaultDockerfile)
	}
	ctx, df := DefaultContext, DefaultDockerfile
	if c := get(b, keyContext); c != nil {
		ctx = c.Value
	}
	if d := get(b, keyDockerfile); d != nil {
		df = d.Value
	}
	if path.IsAbs(df) {
		return ctx, df
	}
	return ctx, path.Join(ctx, df)
}

func attachEnv(svc *yaml.Node) {
	ef := get(svc, keyEnvFile)
	switch {
	case ef == nil || (ef.Kind == yaml.ScalarNode && ef.Value == ""):
		set(svc, keyEnvFile, sequence(str(EnvFile)))
	case ef.Kind == yaml.ScalarNode:
		files := sequence(str(ef.Value))
		if ef.Value != EnvFile {
			files.Content = append(files.Content, str(EnvFile))
		}
		set(svc, keyEnvFile, files)
	case ef.Kind == yaml.SequenceNode && !listHas(ef, EnvFile):
		ef.Content = append(ef.Content, str(EnvFile))
	}
}

func listHas(seq *yaml.Node, v string) bool {
	for _, c := range seq.Content {
		if c.Value == v {
			return true
		}
	}
	return false
}

func defaultLogging() *yaml.Node {
	opts := mapping(str(keyMaxSize), str(DefaultLogSize), str(keyMaxFile), str(DefaultLogFiles))
	return mapping(str(keyDriver), str(DefaultLogDriver), str(keyOptions), opts)
}

func applyDefaults(svc *yaml.Node) {
	setDefault(svc, keyRestart, str(DefaultRestart))
	if !has(svc, keyMemLimit) && !has(svc, keyDeploy) {
		setDefault(svc, keyMemLimit, str(DefaultMemLimit))
	}
	setDefault(svc, keyLogging, defaultLogging())
}

func hostService(svcs []service, main service) service {
	for _, s := range svcs {
		if truthy(labels(s.node)[LabelHost]) {
			return s
		}
	}
	return main
}

type Summary struct {
	Built         int
	ContainerPort string
}

func Summarize(data []byte) (Summary, error) {
	doc, err := parse(data)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Built: len(filter(doc.services, isBuilt))}
	if main, err := pickMain(doc.services); err == nil {
		sum.ContainerPort = containerPort(main.node)
	}
	return sum, nil
}

func Render(app string, data []byte) (Result, error) {
	doc, err := parse(data)
	if err != nil {
		return Result{}, err
	}
	main, err := pickMain(doc.services)
	if err != nil {
		return Result{}, err
	}
	res := Result{Main: main.name, Public: isPublic(labels(main.node))}
	res.Context, res.Dockerfile = buildPaths(main.node)
	if res.Public {
		res.Port = publishedPort(hostService(doc.services, main).node)
	}
	res.Health = healthMode(main.node, res.Port)
	remove(main.node, keyBuild)
	set(main.node, keyImage, str(ImagePlaceholder))
	set(main.node, keyContainerName, str(app))
	attachEnv(main.node)
	for _, s := range doc.services {
		applyDefaults(s.node)
		if berr := bindLoopback(s.name, s.node); berr != nil {
			return Result{}, berr
		}
	}
	if verr := res.validate(); verr != nil {
		return Result{}, verr
	}
	res.Compose, err = encode(doc.root)
	return res, err
}

func (r Result) validate() error {
	return errors.Join(
		fieldErr(validate.Service(r.Main), ErrBadMain),
		fieldErr(validate.OptionalPort(r.Port), ErrBadPort),
		fieldErr(validate.RelPath(r.Context), ErrBadBuildPath),
		fieldErr(validate.RelPath(r.Dockerfile), ErrBadBuildPath),
	)
}

func fieldErr(err, kind error) error {
	if err != nil {
		return kind
	}
	return nil
}

func isPublic(l map[string]string) bool {
	v, ok := l[LabelPublic]
	return !ok || truthy(v)
}

func healthMode(main *yaml.Node, port string) string {
	if has(main, keyHealthcheck) {
		return HealthCompose
	}
	if port != "" {
		return HealthHTTP
	}
	return HealthProcess
}

func encode(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(yamlIndent)
	err := enc.Encode(root)
	if err == nil {
		err = enc.Close()
	}
	return buf.Bytes(), err
}
