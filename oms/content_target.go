package oms

import (
	"fmt"

	"operetta/presentation"
)

// renderTarget is the protocol-neutral output port used by the HTML/CSS
// transformer. Page and contentTarget both implement it: Page keeps old helper
// paths working while contentTarget records a reusable intermediate document.
type renderTarget interface {
	AddStyle(uint32)
	AddText(string)
	AddBreak()
	BeginLink(string)
	EndLink()
	AddLink(string, string)
	AddBgcolor(string)
	AddHr(string)
	AddImagePlaceholder(int, int)
	AddImageInline(int, int, []byte)
	AddPlus()
	AddParagraph()
	AddForm(string)
	AddFormWithMethod(string, string)
	AddTextInput(string, string)
	AddPassInput(string, string)
	AddCheckbox(string, string, bool)
	AddSubmit(string, string)
	AddRadio(string, string, bool)
	AddHidden(string, string)
	BeginSelect(string, bool, int)
	AddOption(string, string, bool)
	EndSelect()
	AddButton(string, string)
	AddReset(string, string)
	UnitCount() int
	RecordHidden(string, string, string)
}

type contentTarget struct {
	builder *presentation.Builder
}

func newContentTarget(url string) *contentTarget {
	return &contentTarget{builder: presentation.NewBuilder(url)}
}

func (t *contentTarget) Document() *presentation.Document { return t.builder.Document() }
func (t *contentTarget) UnitCount() int                   { return t.builder.UnitCount() }

func textStyleFromWord(word uint32) presentation.TextStyle {
	flags := byte(word)
	style := presentation.TextStyle{
		Italic:    flags&byte(styleItalicBit) != 0,
		Bold:      flags&byte(styleBoldBit) != 0,
		Underline: flags&byte(styleUnderBit) != 0,
	}
	switch {
	case flags&byte(styleRightBit) != 0:
		style.Alignment = presentation.AlignRight
	case flags&byte(styleCenterBit) != 0:
		style.Alignment = presentation.AlignCenter
	}
	if color := uint16((word >> 8) & 0xffff); color != 0 {
		style.Foreground = fmt.Sprintf("#%06x", rgb565ToRGB24(color))
	}
	return style
}

func styleWordFromTextStyle(style presentation.TextStyle) uint32 {
	var flags byte
	if style.Italic {
		flags |= byte(styleItalicBit)
	}
	if style.Bold {
		flags |= byte(styleBoldBit)
	}
	if style.Underline {
		flags |= byte(styleUnderBit)
	}
	switch style.Alignment {
	case presentation.AlignCenter:
		flags |= byte(styleCenterBit)
	case presentation.AlignRight:
		flags |= byte(styleRightBit)
	}
	return uint32(flags) | uint32(calcColor(style.Foreground))<<8
}

func (t *contentTarget) AddStyle(style uint32) { t.builder.AddStyle(textStyleFromWord(style)) }

// AddExactStyle preserves the CSS color for protocol encoders that are not
// constrained to the legacy RGB565 palette. Flags still come from the shared
// style word so the old Page renderer keeps identical behavior.
func (t *contentTarget) AddExactStyle(style uint32, color string) {
	textStyle := textStyleFromWord(style)
	if normalized := cssToHex(color); normalized != "" {
		textStyle.Foreground = normalized
	}
	t.builder.AddStyle(textStyle)
}
func (t *contentTarget) AddText(text string)  { t.builder.AddText(text) }
func (t *contentTarget) AddBreak()            { t.builder.AddBreak() }
func (t *contentTarget) BeginLink(url string) { t.builder.BeginLink(url) }
func (t *contentTarget) EndLink()             { t.builder.EndLink() }
func (t *contentTarget) AddLink(url, text string) {
	t.builder.AddLink(url, text)
}
func (t *contentTarget) AddBgcolor(color string) { t.builder.AddBackground(color) }
func (t *contentTarget) AddHr(color string)      { t.builder.AddHorizontalRule(color) }
func (t *contentTarget) AddImagePlaceholder(width, height int) {
	t.builder.AddImagePlaceholder(width, height)
}
func (t *contentTarget) AddImageInline(width, height int, data []byte) {
	t.builder.AddImageInline(width, height, data)
}
func (t *contentTarget) AddPlus()      { t.builder.AddBlockSeparator() }
func (t *contentTarget) AddParagraph() { t.builder.AddParagraph() }
func (t *contentTarget) AddForm(action string) {
	t.builder.AddForm(action, "get")
}
func (t *contentTarget) AddFormWithMethod(action, method string) {
	t.builder.AddForm(action, method)
}
func (t *contentTarget) AddTextInput(name, value string) {
	t.builder.AddTextInput(name, value)
}
func (t *contentTarget) AddPassInput(name, value string) {
	t.builder.AddPasswordInput(name, value)
}
func (t *contentTarget) AddCheckbox(name, value string, checked bool) {
	t.builder.AddCheckbox(name, value, checked)
}
func (t *contentTarget) AddSubmit(name, value string) { t.builder.AddSubmit(name, value) }
func (t *contentTarget) AddRadio(name, value string, checked bool) {
	t.builder.AddRadio(name, value, checked)
}
func (t *contentTarget) AddHidden(name, value string) { t.builder.AddHidden(name, value) }
func (t *contentTarget) BeginSelect(name string, multiple bool, count int) {
	t.builder.BeginSelect(name, multiple, count)
}
func (t *contentTarget) AddOption(value, label string, selected bool) {
	t.builder.AddOption(value, label, selected)
}
func (t *contentTarget) EndSelect() { t.builder.EndSelect() }
func (t *contentTarget) AddButton(name, value string) {
	t.builder.AddButton(name, value)
}
func (t *contentTarget) AddReset(name, value string) { t.builder.AddReset(name, value) }
func (t *contentTarget) RecordHidden(action, name, value string) {
	t.builder.RecordHidden(action, name, value)
}

func encodeContentOperations(page *Page, doc *presentation.Document) {
	for _, op := range doc.Operations {
		switch op.Kind {
		case presentation.Style:
			page.AddStyle(styleWordFromTextStyle(op.Style))
		case presentation.Text:
			page.AddText(op.Text)
		case presentation.Break:
			page.AddBreak()
		case presentation.LinkStart:
			page.BeginLink(op.URL)
		case presentation.LinkEnd:
			page.EndLink()
		case presentation.Background:
			page.AddBgcolor(op.Color)
		case presentation.HorizontalRule:
			page.AddHr(op.Color)
		case presentation.ImagePlaceholder:
			page.AddImagePlaceholder(op.Width, op.Height)
		case presentation.ImageInline:
			page.AddImageInline(op.Width, op.Height, op.Data)
		case presentation.BlockSeparator:
			page.AddPlus()
		case presentation.Paragraph:
			page.AddParagraph()
		case presentation.FormStart:
			page.AddFormWithMethod(op.URL, op.Method)
		case presentation.TextInput:
			page.AddTextInput(op.Name, op.Value)
		case presentation.PasswordInput:
			page.AddPassInput(op.Name, op.Value)
		case presentation.Checkbox:
			page.AddCheckbox(op.Name, op.Value, op.Checked)
		case presentation.Submit:
			page.AddSubmit(op.Name, op.Value)
		case presentation.Radio:
			page.AddRadio(op.Name, op.Value, op.Checked)
		case presentation.HiddenInput:
			page.AddHidden(op.Name, op.Value)
		case presentation.SelectStart:
			page.BeginSelect(op.Name, op.Multiple, op.Count)
		case presentation.Option:
			page.AddOption(op.Value, op.Text, op.Selected)
		case presentation.SelectEnd:
			page.EndSelect()
		case presentation.Button:
			page.AddButton(op.Name, op.Value)
		case presentation.Reset:
			page.AddReset(op.Name, op.Value)
		}
	}
}

func cloneHiddenFields(src map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(src))
	for action, fields := range src {
		cloned := make(map[string]string, len(fields))
		for name, value := range fields {
			cloned[name] = value
		}
		out[action] = cloned
	}
	return out
}
