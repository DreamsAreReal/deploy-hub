package policy

import (
	"bytes"
	"os"
	"testing"
)

func TestGateIsTheRepositoryPolicy(t *testing.T) {
	want, err := os.ReadFile("gate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Gate(), want) {
		t.Fatal("embedded policy differs from policy/gate.yaml")
	}
}

func TestArchitectureIsTheRepositoryCatalog(t *testing.T) {
	want, err := os.ReadFile("architecture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Architecture(), want) {
		t.Fatal("embedded catalog differs from policy/architecture.yaml")
	}
}
