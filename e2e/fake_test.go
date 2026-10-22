package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	envState      = "DH_FAKE_STATE"
	envPort       = "DH_FAKE_PORT"
	envVersion    = "DH_FAKE_VERSION"
	brokenMark    = ":sha-dead"
	badPull       = "sha-bad"
	badDigest     = "sha256:bad0000000000000000000000000000000000000000000000000000000000000"
	bootWait      = 3 * time.Second
	bootPoll      = 20 * time.Millisecond
	filePerm      = 0o600
	exitFail      = 1
	verbPs        = "ps"
	binDocker     = "docker"
	binSudo       = "sudo"
	binCaddy      = "caddy"
	binSysctl     = "systemctl"
	binHub        = "deploy-hub"
	binFakeApp    = "dh-fake-app"
	verbCaddySync = "caddy-sync"
	sudoArgs      = 2
	shaHexLen     = 40
)

type container struct {
	PID   int    `json:"pid"`
	Image string `json:"image"`
	Port  string `json:"port"`
}

type fake struct {
	state string
}

func (f fake) path(name string) string {
	return filepath.Join(f.state, "containers", name+".json")
}

func (f fake) load(name string) (container, bool) {
	data, err := os.ReadFile(filepath.Clean(f.path(name)))
	if err != nil {
		return container{}, false
	}
	var c container
	return c, json.Unmarshal(data, &c) == nil
}

func (f fake) save(name string, c container) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path(name)), 0o750); err != nil {
		return err
	}
	return os.WriteFile(f.path(name), data, filePerm)
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (f fake) kill(name string) {
	if c, ok := f.load(name); ok && alive(c.PID) {
		_ = syscall.Kill(c.PID, syscall.SIGKILL)
		for alive(c.PID) {
			time.Sleep(bootPoll)
		}
	}
}

func (f fake) start(name, image, port string) error {
	f.kill(name)
	c := container{Image: image, Port: port}
	if !strings.Contains(image, brokenMark) {
		pid, err := spawnServer(port, image)
		if err != nil {
			return err
		}
		c.PID = pid
	}
	return f.save(name, c)
}

func spawnServer(port, version string) (int, error) {
	cmd := exec.CommandContext(context.Background(), "dh-fake-app")
	cmd.Env = append(os.Environ(), envPort+"="+port, envVersion+"="+version)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(bootWait)
	for time.Now().Before(deadline) {
		if listening(port) {
			pid := cmd.Process.Pid
			return pid, cmd.Process.Release()
		}
		time.Sleep(bootPoll)
	}
	return 0, errors.New("fake app did not start")
}

func listening(port string) bool {
	d := net.Dialer{Timeout: bootPoll}
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func serve() int {
	version := os.Getenv(envVersion)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "version.txt", time.Time{}, strings.NewReader(version))
	})
	srv := &http.Server{Addr: net.JoinHostPort("127.0.0.1", os.Getenv(envPort)), Handler: mux, ReadHeaderTimeout: time.Second}
	if err := srv.ListenAndServe(); err != nil {
		return exitFail
	}
	return 0
}

func (f fake) docker(args []string) int {
	f.logCall(args)
	if len(args) == 0 {
		return exitFail
	}
	handlers := map[string]func([]string) int{
		"compose": f.compose, verbPs: f.ps, "inspect": f.inspect, "run": f.run, "rm": f.rm, "pull": f.pull,
	}
	if h, ok := handlers[args[0]]; ok {
		return h(args)
	}
	return 0
}

func (f fake) logCall(args []string) {
	file, err := os.OpenFile(filepath.Join(f.state, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, strings.Join(args, " "))
	_ = file.Close()
}

func mainName(composeFile string) string {
	return filepath.Base(filepath.Dir(composeFile)) + "-app-1"
}

func (f fake) compose(args []string) int {
	file, verb := args[2], args[3]
	name := mainName(file)
	switch verb {
	case "up":
		conf, err := registry.LoadAppConf(filepath.Dir(file))
		if err != nil || f.start(name, os.Getenv("DEPLOY_IMAGE"), conf.Port) != nil {
			return exitFail
		}
	case "stop":
		f.kill(name)
	case verbPs:
		if _, ok := f.load(name); ok {
			fmt.Println(name)
		}
	}
	return 0
}

func (f fake) ps(args []string) int {
	name := strings.TrimSuffix(strings.TrimPrefix(args[len(args)-1], "name=^"), "$")
	if c, ok := f.load(name); ok && alive(c.PID) {
		fmt.Println("id-" + name)
	}
	return 0
}

func (f fake) inspect(args []string) int {
	format, name := args[2], args[3]
	c, ok := f.load(name)
	if !ok {
		return exitFail
	}
	switch {
	case strings.Contains(format, ".State.Health"):
		fmt.Println("-")
	case strings.Contains(format, ".State.Status"):
		fmt.Println(map[bool]string{true: "running", false: "exited"}[alive(c.PID)])
	case strings.Contains(format, ".Mounts"):
	case strings.Contains(format, ".Ports"):
		fmt.Printf("%s/tcp %s\n", c.Port, c.Port)
	default:
		fmt.Println("bridge")
	}
	return 0
}

func (f fake) run(args []string) int {
	name, publish := "", ""
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "--name":
			name = args[i+1]
		case "-p":
			publish = args[i+1]
		}
	}
	parts := strings.Split(publish, ":")
	if name == "" || len(parts) != 3 || f.start(name, args[len(args)-1], parts[1]) != nil {
		return exitFail
	}
	return 0
}

func (f fake) rm(args []string) int {
	name := args[len(args)-1]
	f.kill(name)
	_ = os.Remove(f.path(name))
	return 0
}

func (f fake) pull(args []string) int {
	if strings.HasSuffix(args[len(args)-1], badDigest) {
		fmt.Fprintln(os.Stderr, "manifest unknown")
		return exitFail
	}
	return 0
}

func sudo(args []string) int {
	if len(args) > 0 && args[0] == "-n" {
		args = args[1:]
	}
	if len(args) != sudoArgs || filepath.Base(args[0]) != binHub || args[1] != verbCaddySync {
		return exitFail
	}
	cmd := exec.CommandContext(context.Background(), "deploy-hub", "caddy-sync")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return exitFail
	}
	return 0
}

func (f fake) killAll() {
	entries, _ := os.ReadDir(filepath.Join(f.state, "containers"))
	for _, e := range entries {
		f.kill(strings.TrimSuffix(e.Name(), ".json"))
	}
}
