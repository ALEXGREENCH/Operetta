// Package presentation defines the display list exchanged between content
// transformation and client protocol encoders. It is intentionally smaller
// than a browser DOM but remains independent from OMS/OBML wire tags.
package presentation

// Kind identifies a protocol-independent rendering operation.
type Kind uint8

const (
	Style Kind = iota + 1
	Text
	Break
	LinkStart
	LinkEnd
	Background
	HorizontalRule
	ImagePlaceholder
	ImageInline
	BlockSeparator
	Paragraph
	FormStart
	TextInput
	PasswordInput
	Checkbox
	Submit
	Radio
	HiddenInput
	SelectStart
	Option
	SelectEnd
	Button
	Reset
)

// Alignment is a logical text alignment, independent of a wire protocol.
type Alignment uint8

const (
	AlignDefault Alignment = iota
	AlignCenter
	AlignRight
)

// TextStyle is the normalized style understood by content transformers and
// mapped to the closest capability by each client protocol encoder.
type TextStyle struct {
	Italic     bool
	Bold       bool
	Underline  bool
	Alignment  Alignment
	Foreground string // canonical #rrggbb when set
}

// Operation is one item in the transformed page stream. Fields are populated
// according to Kind; keeping the value flat makes encoders small and avoids a
// protocol-specific DOM in the core.
type Operation struct {
	Kind     Kind
	Text     string
	URL      string
	Name     string
	Value    string
	Method   string
	Color    string
	Style    TextStyle
	Width    int
	Height   int
	Data     []byte
	Checked  bool
	Selected bool
	Multiple bool
	Count    int
}

// Metrics describes the representation before protocol framing.
type Metrics struct {
	OriginTransferBytes int
	OriginDecodedBytes  int
}

// Document is the reusable presentation plan. Protocol session data (auth
// tokens, compression and wire version) deliberately does not live here and is
// supplied only to an encoder.
type Document struct {
	URL          string
	Title        string
	Operations   []Operation
	SetCookies   []string
	FormHidden   map[string]map[string]string
	Metrics      Metrics
	NoCache      bool
	DownloadOnly bool
}
