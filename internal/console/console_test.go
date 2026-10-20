package console

import (
	"bytes"
	"errors"
	"testing"
)

type failingWriter struct{ calls int }

var errWrite = errors.New("write failed")

func (f *failingWriter) Write([]byte) (int, error) {
	f.calls++
	return 0, errWrite
}

func TestPrinterWrites(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf)
	p.Printf("%s=%d\n", "a", 1)
	p.Println("b", 2)
	p.Print("c")
	if _, err := p.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a=1\nb 2\ncd" || p.Err() != nil {
		t.Fatalf("got %q err=%v", buf.String(), p.Err())
	}
}

func TestPrinterKeepsFirstError(t *testing.T) {
	w := &failingWriter{}
	p := New(w)
	p.Print("x")
	p.Println("y")
	p.Printf("z")
	if _, err := p.Write([]byte("w")); !errors.Is(err, errWrite) {
		t.Fatalf("write err: %v", err)
	}
	if !errors.Is(p.Err(), errWrite) || w.calls != 4 {
		t.Fatalf("err=%v calls=%d", p.Err(), w.calls)
	}
}
