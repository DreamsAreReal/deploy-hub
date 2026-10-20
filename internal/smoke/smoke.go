package smoke

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

const commentPrefix = "#"

var portToken = regexp.MustCompile(`^[0-9]{1,5}$`)

func Ports(path string) []int {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, commentPrefix) {
			continue
		}
		out = append(out, linePorts(trimmed)...)
	}
	return out
}

func linePorts(line string) []int {
	var out []int
	for _, tok := range strings.Fields(line) {
		if !portToken.MatchString(tok) {
			continue
		}
		if p, err := strconv.Atoi(tok); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func Check(path string, listening func(int) bool) string {
	ports := Ports(path)
	if len(ports) == 0 {
		return journal.ResultSkip
	}
	for _, p := range ports {
		if !listening(p) {
			return journal.ResultFail
		}
	}
	return journal.ResultOK
}
