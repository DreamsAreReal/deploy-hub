package policy

import _ "embed"

//go:embed gate.yaml
var gate []byte

//go:embed architecture.yaml
var architecture []byte

func Gate() []byte {
	return gate
}

func Architecture() []byte {
	return architecture
}
