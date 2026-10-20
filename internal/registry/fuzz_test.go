package registry

import (
	"strings"
	"testing"
)

func FuzzParseKV(f *testing.F) {
	f.Add("profile=service\nport=9001\nport=9002\nnoise\n=x\n")
	f.Fuzz(func(t *testing.T, text string) {
		kv := ParseKV(text)
		checkKeys(t, kv)
		checkRoundTrip(t, kv, ParseKV(EncodeKV(pairs(kv))))
	})
}

func checkKeys(t *testing.T, kv map[string]string) {
	t.Helper()
	for k := range kv {
		if strings.ContainsAny(k, "=\n") {
			t.Fatalf("key %q leaked a separator", k)
		}
	}
}

func checkRoundTrip(t *testing.T, kv, again map[string]string) {
	t.Helper()
	for k, v := range kv {
		if representable(v) && again[k] != v {
			t.Fatalf("round trip lost %q=%q", k, v)
		}
	}
}

func representable(v string) bool {
	return v != "" && !strings.ContainsAny(v, "\r\n")
}

func pairs(kv map[string]string) []KV {
	out := make([]KV, 0, len(kv))
	for k, v := range kv {
		out = append(out, KV{Key: k, Value: v})
	}
	return out
}
