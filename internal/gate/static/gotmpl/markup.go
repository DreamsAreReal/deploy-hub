package gotmpl

const (
	inText = iota
	inOpen
	inTag
	inQuote
)

type markup struct {
	state int
	quote rune
	last  rune
}

func (m *markup) feed(text string) {
	for _, r := range text {
		m.step(r)
		m.last = r
	}
}

func (m *markup) step(r rune) {
	switch m.state {
	case inText:
		if r == '<' {
			m.state = inOpen
		}
	case inOpen:
		m.state = opened(r)
	case inTag:
		m.inTag(r)
	default:
		if r == m.quote {
			m.state = inTag
		}
	}
}

func opened(r rune) int {
	if r == '/' || r == '!' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
		return inTag
	}
	return inText
}

func (m *markup) inTag(r rune) {
	switch r {
	case '>':
		m.state = inText
	case '"', '\'':
		m.state, m.quote = inQuote, r
	}
}

func (m *markup) takesValue() bool {
	return m.state != inTag || m.last == '='
}
