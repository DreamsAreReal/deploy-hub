package policy

import _ "embed"

//go:embed gate.yaml
var gate []byte

func Gate() []byte {
	return gate
}
