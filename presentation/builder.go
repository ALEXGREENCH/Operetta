package presentation

import "strings"

// Builder records protocol-independent operations while preserving the tag
// count semantics expected by the legacy transformer during migration.
type Builder struct {
	doc       *Document
	unitCount int
}

func NewBuilder(url string) *Builder {
	return &Builder{doc: &Document{
		URL:        url,
		FormHidden: make(map[string]map[string]string),
	}}
}

func (b *Builder) Document() *Document { return b.doc }
func (b *Builder) UnitCount() int      { return b.unitCount }

func (b *Builder) append(op Operation, units int) {
	if op.Data != nil {
		op.Data = append([]byte(nil), op.Data...)
	}
	b.doc.Operations = append(b.doc.Operations, op)
	b.unitCount += units
}

func (b *Builder) AddStyle(style TextStyle) {
	b.append(Operation{Kind: Style, Style: style}, 1)
}

func (b *Builder) AddText(text string) {
	text = strings.TrimLeft(text, "\r\n")
	if text == "" {
		return
	}
	b.append(Operation{Kind: Text, Text: text}, 1)
}

func (b *Builder) AddBreak() { b.append(Operation{Kind: Break}, 1) }

func (b *Builder) BeginLink(url string) {
	b.append(Operation{Kind: LinkStart, URL: url}, 1)
}

func (b *Builder) EndLink() { b.append(Operation{Kind: LinkEnd}, 1) }

func (b *Builder) AddLink(url, text string) {
	b.BeginLink(url)
	b.AddText(text)
	b.AddBreak()
	b.EndLink()
}

func (b *Builder) AddBackground(color string) {
	if color != "" {
		b.append(Operation{Kind: Background, Color: color}, 1)
	}
}

func (b *Builder) AddHorizontalRule(color string) {
	b.append(Operation{Kind: HorizontalRule, Color: color}, 1)
}

func (b *Builder) AddImagePlaceholder(width, height int) {
	b.append(Operation{Kind: ImagePlaceholder, Width: width, Height: height}, 1)
}

func (b *Builder) AddImageInline(width, height int, data []byte) {
	b.append(Operation{Kind: ImageInline, Width: width, Height: height, Data: data}, 1)
}

func (b *Builder) AddBlockSeparator() { b.append(Operation{Kind: BlockSeparator}, 1) }
func (b *Builder) AddParagraph()      { b.append(Operation{Kind: Paragraph}, 1) }

func (b *Builder) AddForm(action, method string) {
	b.append(Operation{Kind: FormStart, URL: action, Method: method}, 1)
}

func (b *Builder) AddTextInput(name, value string) {
	b.append(Operation{Kind: TextInput, Name: name, Value: value}, 1)
}

func (b *Builder) AddPasswordInput(name, value string) {
	b.append(Operation{Kind: PasswordInput, Name: name, Value: value}, 1)
}

func (b *Builder) AddCheckbox(name, value string, checked bool) {
	b.append(Operation{Kind: Checkbox, Name: name, Value: value, Checked: checked}, 1)
}

func (b *Builder) AddSubmit(name, value string) {
	b.append(Operation{Kind: Submit, Name: name, Value: value}, 1)
}

func (b *Builder) AddRadio(name, value string, checked bool) {
	b.append(Operation{Kind: Radio, Name: name, Value: value, Checked: checked}, 1)
}

func (b *Builder) AddHidden(name, value string) {
	b.append(Operation{Kind: HiddenInput, Name: name, Value: value}, 1)
}

func (b *Builder) BeginSelect(name string, multiple bool, count int) {
	b.append(Operation{Kind: SelectStart, Name: name, Multiple: multiple, Count: count}, 1)
}

func (b *Builder) AddOption(value, label string, selected bool) {
	b.append(Operation{Kind: Option, Value: value, Text: label, Selected: selected}, 1)
}

func (b *Builder) EndSelect() { b.append(Operation{Kind: SelectEnd}, 1) }

func (b *Builder) AddButton(name, value string) {
	b.append(Operation{Kind: Button, Name: name, Value: value}, 1)
}

func (b *Builder) AddReset(name, value string) {
	b.append(Operation{Kind: Reset, Name: name, Value: value}, 1)
}

func (b *Builder) RecordHidden(action, name, value string) {
	if action == "" || name == "" {
		return
	}
	if b.doc.FormHidden[action] == nil {
		b.doc.FormHidden[action] = make(map[string]string)
	}
	if _, exists := b.doc.FormHidden[action][name]; !exists {
		b.doc.FormHidden[action][name] = value
	}
}
