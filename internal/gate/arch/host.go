package arch

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	ProcNetDir    = "/proc/net"
	AccountsDir   = "/etc"
	passwdFile    = "passwd"
	groupFile     = "group"
	rootUser      = "root"
	rootUID       = "0"
	viaUID        = "uid 0"
	viaGroup      = "group "
	tcpPrefix     = "tcp"
	tcpListen     = "0A"
	udpUnbound    = "07"
	unsetAddress  = "0:"
	localField    = 1
	remoteField   = 2
	stateField    = 3
	socketFields  = 4
	wordBytes     = 4
	hexBase       = 16
	portBits      = 16
	nameField     = 0
	idField       = 2
	gidField      = 3
	membersField  = 3
	accountFields = 4
	listSeparator = ","
	netnsKey      = "NetworkNamespacePath="
	dropInDir     = ".d"
	dropInGlob    = "*.conf"
	templateMark  = "@"
	instanceMark  = "%i"
	hostNetns     = "the host network namespace"
	listenOnHost  = "port %d listens beyond loopback on the host, and no service declares it public in exposes"
	rootOnHost    = "user %s can act as root through %s, and admins in the catalog does not list it"
	netnsOnHost   = "unit %s runs in %s, and the catalog declares %s"
)

var (
	socketFiles = []string{"tcp", "tcp6", "udp", "udp6"}
	rootGroups  = []string{"root", "sudo", "wheel", "admin", "docker", "lxd", "disk"}
)

type Admin struct {
	User string `yaml:"user"`
	Why  string `yaml:"why"`
}

type Isolation struct {
	Unit  string `yaml:"unit"`
	Netns string `yaml:"netns"`
	Why   string `yaml:"why"`
}

type Grant struct {
	User string
	Via  string
}

type HostDirs struct {
	Units    string
	Sites    string
	Net      string
	Accounts string
}

func (d HostDirs) Facts(isolated []string) (Facts, error) {
	facts, err := Installed(d.Units, d.Sites)
	if err == nil {
		facts.Listening, err = Listening(d.Net)
	}
	if err == nil {
		facts.Grants, err = RootUsers(d.Accounts)
	}
	if err == nil {
		facts.Namespaces, err = Namespaces(d.Units, isolated)
	}
	return facts, err
}

func readOptional(dir, name string) (string, error) {
	if dir == "" {
		return "", nil
	}
	body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(body), err
}

func Listening(dir string) ([]int, error) {
	var ports []int
	for _, name := range socketFiles {
		table, err := readOptional(dir, name)
		if err != nil {
			return nil, err
		}
		for _, port := range exposed(table, strings.HasPrefix(name, tcpPrefix)) {
			if !slices.Contains(ports, port) {
				ports = append(ports, port)
			}
		}
	}
	slices.Sort(ports)
	return ports, nil
}

func exposed(table string, tcp bool) []int {
	var ports []int
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < socketFields || !open(fields, tcp) {
			continue
		}
		if port, ok := beyondLoopback(fields[localField]); ok {
			ports = append(ports, port)
		}
	}
	return ports
}

func open(fields []string, tcp bool) bool {
	if tcp {
		return fields[stateField] == tcpListen
	}
	return fields[stateField] == udpUnbound && strings.Trim(fields[remoteField], unsetAddress) == ""
}

func beyondLoopback(local string) (int, bool) {
	address, hexPort, _ := strings.Cut(local, ":")
	raw, err := hex.DecodeString(address)
	port, portErr := strconv.ParseUint(hexPort, hexBase, portBits)
	if err != nil || portErr != nil || (len(raw) != net.IPv4len && len(raw) != net.IPv6len) {
		return 0, false
	}
	return int(port), !kernelOrder(raw).IsLoopback()
}

func kernelOrder(raw []byte) net.IP {
	ip := make(net.IP, len(raw))
	for i := range raw {
		ip[i] = raw[i/wordBytes*wordBytes+wordBytes-1-i%wordBytes]
	}
	return ip
}

func RootUsers(dir string) ([]Grant, error) {
	passwd, err := readOptional(dir, passwdFile)
	if err != nil {
		return nil, err
	}
	groups, err := readOptional(dir, groupFile)
	if err != nil {
		return nil, err
	}
	names, grants := groupGrants(groups)
	return userGrants(passwd, names, grants), nil
}

func groupGrants(groups string) (map[string]string, []Grant) {
	names := map[string]string{}
	var grants []Grant
	for _, group := range records(groups) {
		names[group[idField]] = group[nameField]
		if !slices.Contains(rootGroups, group[nameField]) {
			continue
		}
		for _, member := range strings.Split(group[membersField], listSeparator) {
			grants = grant(grants, member, viaGroup+group[nameField])
		}
	}
	return names, grants
}

func userGrants(passwd string, names map[string]string, grants []Grant) []Grant {
	for _, user := range records(passwd) {
		if user[idField] == rootUID {
			grants = grant(grants, user[nameField], viaUID)
		}
		if group := names[user[gidField]]; slices.Contains(rootGroups, group) {
			grants = grant(grants, user[nameField], viaGroup+group)
		}
	}
	return grants
}

func records(body string) [][]string {
	var out [][]string
	for _, line := range strings.Split(body, "\n") {
		if fields := strings.Split(strings.TrimSpace(line), ":"); len(fields) >= accountFields {
			out = append(out, fields)
		}
	}
	return out
}

func grant(grants []Grant, user, via string) []Grant {
	g := Grant{User: user, Via: via}
	if user == "" || user == rootUser || slices.Contains(grants, g) {
		return grants
	}
	return append(grants, g)
}

func Namespaces(dir string, units []string) (map[string]string, error) {
	found := map[string]string{}
	for _, unit := range units {
		if err := namespacesOf(dir, unit, found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func namespacesOf(dir, unit string, found map[string]string) error {
	body, err := readOptional(dir, unit)
	if err != nil || body == "" {
		return err
	}
	base, err := dropIns(dir, unit, netnsIn(body, ""))
	if err != nil {
		return err
	}
	found[unit] = base
	return instances(dir, unit, base, found)
}

func instances(dir, template, base string, found map[string]string) error {
	head, tail, ok := strings.Cut(template, templateMark)
	if !ok {
		return nil
	}
	dirs, _ := filepath.Glob(filepath.Join(dir, head+templateMark+"*"+tail+dropInDir))
	for _, path := range dirs {
		name := strings.TrimSuffix(filepath.Base(path), dropInDir)
		instance, ok := instanceOf(template, name)
		if !ok {
			continue
		}
		value, err := dropIns(dir, name, base)
		if err != nil {
			return err
		}
		found[name] = strings.ReplaceAll(value, instanceMark, instance)
	}
	return nil
}

func dropIns(dir, unit, value string) (string, error) {
	files, _ := filepath.Glob(filepath.Join(dir, unit+dropInDir, dropInGlob))
	for _, file := range files {
		body, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return "", err
		}
		value = netnsIn(string(body), value)
	}
	return value, nil
}

func netnsIn(body, value string) string {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), netnsKey); ok {
			value = strings.TrimSpace(rest)
		}
	}
	return value
}

func instanceOf(template, unit string) (string, bool) {
	head, tail, ok := strings.Cut(template, templateMark)
	rest, prefixed := strings.CutPrefix(unit, head+templateMark)
	instance, suffixed := strings.CutSuffix(rest, tail)
	return instance, ok && prefixed && suffixed && instance != ""
}

func (i Isolation) expects(unit string) (string, bool) {
	if unit == i.Unit {
		return i.Netns, true
	}
	instance, ok := instanceOf(i.Unit, unit)
	return strings.ReplaceAll(i.Netns, instanceMark, instance), ok
}

func (c Catalog) IsolatedUnits() []string {
	var units []string
	for _, name := range c.names() {
		for _, isolation := range c.Services[name].Isolated {
			units = appendNew(units, isolation.Unit)
		}
	}
	return units
}

func (c Catalog) publicPorts() []int {
	var ports []int
	for _, service := range c.Services {
		for _, expose := range service.Exposes {
			if expose.Public {
				ports = append(ports, expose.Port)
			}
		}
	}
	return ports
}

func (c Catalog) listenDrift(listening []int) []string {
	declared := c.publicPorts()
	var drift []string
	for _, port := range listening {
		if !slices.Contains(declared, port) {
			drift = append(drift, fmt.Sprintf(listenOnHost, port))
		}
	}
	return drift
}

func (c Catalog) adminDrift(grants []Grant) []string {
	var drift []string
	for _, g := range grants {
		if !slices.ContainsFunc(c.Admins, func(a Admin) bool { return a.User == g.User }) {
			drift = append(drift, fmt.Sprintf(rootOnHost, g.User, g.Via))
		}
	}
	return drift
}

func (c Catalog) netnsDrift(found map[string]string) []string {
	var drift []string
	for _, name := range c.names() {
		for _, isolation := range c.Services[name].Isolated {
			drift = append(drift, isolation.drift(found)...)
		}
	}
	return drift
}

func (i Isolation) drift(found map[string]string) []string {
	var drift []string
	for _, unit := range slices.Sorted(maps.Keys(found)) {
		if want, ok := i.expects(unit); ok && found[unit] != want {
			drift = append(drift, fmt.Sprintf(netnsOnHost, unit, namespace(found[unit]), namespace(want)))
		}
	}
	return drift
}

func namespace(path string) string {
	if path == "" {
		return hostNetns
	}
	return path
}

func (c Catalog) adminProblems() []string {
	var problems []string
	for _, admin := range c.Admins {
		if strings.TrimSpace(admin.User) == "" || strings.TrimSpace(admin.Why) == "" {
			problems = append(problems, fmt.Sprintf("admin %q without a user or a why: root on the server needs a reason", admin.User))
		}
	}
	return problems
}

func (s Service) hostProblems(name string) []string {
	var problems []string
	for _, expose := range s.Exposes {
		if expose.Public && strings.TrimSpace(expose.Why) == "" {
			problems = append(problems, fmt.Sprintf("%s: public port %d without a why: a port open to the internet needs a reason", name, expose.Port))
		}
	}
	for _, isolation := range s.Isolated {
		if isolation.Unit == "" || isolation.Netns == "" || strings.TrimSpace(isolation.Why) == "" {
			problems = append(problems, fmt.Sprintf("%s: isolated unit %q needs a unit, a netns and a why", name, isolation.Unit))
		}
	}
	return problems
}
