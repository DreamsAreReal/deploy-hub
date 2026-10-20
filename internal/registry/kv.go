package registry

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const filePerm = 0o600

type KV struct {
	Key   string
	Value string
}

func ReadKV(path string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return ParseKV(string(data)), nil
}

func ParseKV(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if _, seen := out[key]; !seen {
			out[key] = value
		}
	}
	return out
}

func EncodeKV(pairs []KV) string {
	var b strings.Builder
	for _, p := range pairs {
		if p.Value == "" {
			continue
		}
		b.WriteString(p.Key + "=" + p.Value + "\n")
	}
	return b.String()
}

func WriteFile(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), filePerm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
