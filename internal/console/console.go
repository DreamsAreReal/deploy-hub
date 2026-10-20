package console

import (
	"fmt"
	"io"
)

type Printer struct {
	w   io.Writer
	err error
}

func New(w io.Writer) *Printer {
	return &Printer{w: w}
}

func (p *Printer) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.keep(err)
	return n, err
}

func (p *Printer) keep(err error) {
	if p.err == nil && err != nil {
		p.err = err
	}
}

func (p *Printer) Printf(format string, args ...any) {
	_, err := fmt.Fprintf(p.w, format, args...)
	p.keep(err)
}

func (p *Printer) Println(args ...any) {
	_, err := fmt.Fprintln(p.w, args...)
	p.keep(err)
}

func (p *Printer) Print(s string) {
	_, err := io.WriteString(p.w, s)
	p.keep(err)
}

func (p *Printer) Err() error {
	return p.err
}
