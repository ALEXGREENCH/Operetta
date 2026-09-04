package operamini4

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"
)

// WelcomePage describes the small native page shown before any origin page is
// requested. It deliberately contains no reference-server-specific state.
type WelcomePage struct {
	Title string
	Base  string
	URL   string
	// DocumentHeight overrides the calculated page height for layouts whose
	// absolute geometry is already known (for example an OM4 comparison page).
	DocumentHeight int
	HideAccent     bool
	Background     uint32
	Accent         uint32
	Lines          []WelcomeLine
}

// WelcomeLine is one styled block in a native OM4 welcome page.
type WelcomeLine struct {
	Text string
	URL  string
	// LinkGroup identifies drawing fragments produced by the same source
	// anchor. OM4 exposes one focus target for the whole anchor even when it
	// contains both an icon and text. Zero keeps the legacy one-line behavior.
	LinkGroup            int
	LinkX                int
	LinkY                int
	LinkWidth            int
	LinkHeight           int
	LinkPositioned       bool
	Image                []byte
	Width                int
	X                    int
	Y                    int
	Positioned           bool
	Absolute             bool
	SameRow              bool
	Color                uint32
	Background           uint32
	BackgroundX          int
	BackgroundY          int
	BackgroundWidth      int
	BackgroundHeight     int
	BackgroundPositioned bool
	BackgroundOnly       bool
	Font                 byte
	Gap                  int
	Height               int
}

// BuildWelcomePage creates a compact OM4 drawing stream without going through
// an OMS/OBML encoder. It is intentionally small while the complete OM4
// presentation encoder is being implemented.
func BuildWelcomePage(spec WelcomePage) ([]byte, error) {
	if spec.Title == "" {
		spec.Title = "Operetta"
	}
	if spec.URL == "" {
		spec.URL = "i:/firsttime/4.2/"
	}
	if spec.Base == "" {
		spec.Base = "o:"
	}
	if spec.Background == 0 {
		spec.Background = 0xff102033
	}
	if spec.Accent == 0 {
		spec.Accent = 0xff13b9d6
	}
	if len(spec.Lines) == 0 {
		return nil, fmt.Errorf("OM4 welcome page has no lines")
	}

	positionedLines, documentHeight := positionWelcomeLines(spec.Lines)
	if spec.DocumentHeight > 0 {
		documentHeight = spec.DocumentHeight
	}
	if documentHeight < 282 {
		documentHeight = 282
	}

	var page bytes.Buffer
	page.WriteByte(6)
	writeUint16(&page, 231)
	writeUint24(&page, documentHeight)
	page.Write([]byte{0x53, 0x00, 0x00})
	writeUint16(&page, 0xffff)
	if err := writeModifiedUTF(&page, spec.Title); err != nil {
		return nil, err
	}
	writeUint16(&page, 0) // no favicon
	if err := writeModifiedUTF(&page, spec.Base); err != nil {
		return nil, err
	}
	if err := writeModifiedUTF(&page, spec.URL); err != nil {
		return nil, err
	}
	page.WriteByte(0x0b)

	// Interactive elements live in an S section. Each L entry contains one or
	// more absolute hit rectangles, followed by a tooltip and destination URL.
	// The top-level L marker below the section points at the first, range start,
	// and last entry. Opera Mini uses those offsets for focus navigation and
	// pointer activation.
	type linkPosition struct {
		offset int
		x      int
		y      int
		width  int
		height int
		url    string
	}
	links := make([]linkPosition, 0)
	groupIndexes := make(map[int]int)
	for _, positioned := range positionedLines {
		line := positioned.line
		if line.URL == "" {
			continue
		}
		linkX, linkY, linkWidth, linkHeight := positioned.x, positioned.y, positioned.width, positioned.height
		if line.LinkPositioned {
			linkX, linkY, linkWidth, linkHeight = line.LinkX, line.LinkY, line.LinkWidth, line.LinkHeight
		}
		if line.LinkGroup > 0 {
			if index, ok := groupIndexes[line.LinkGroup]; ok && links[index].url == line.URL {
				left := min(links[index].x, linkX)
				top := min(links[index].y, linkY)
				right := max(links[index].x+links[index].width, linkX+linkWidth)
				bottom := max(links[index].y+links[index].height, linkY+linkHeight)
				links[index].x, links[index].y = left, top
				links[index].width, links[index].height = right-left, bottom-top
				continue
			}
			groupIndexes[line.LinkGroup] = len(links)
		}
		links = append(links, linkPosition{x: linkX, y: linkY, width: linkWidth, height: linkHeight, url: line.URL})
	}
	if len(links) > 0 {
		var interactions bytes.Buffer
		for i := range links {
			links[i].offset = interactions.Len()
			interactions.WriteByte('L')
			interactions.WriteByte(1)
			writeRectangleBody(&interactions, links[i].x, links[i].y, links[i].width, links[i].height)
			if err := writeModifiedUTF(&interactions, ""); err != nil {
				return nil, err
			}
			if err := writeModifiedUTF(&interactions, links[i].url); err != nil {
				return nil, err
			}
		}
		sectionStart := page.Len()
		page.WriteByte('S')
		writeUint24(&page, interactions.Len())
		page.Write(interactions.Bytes())
		first := sectionStart + 4 + links[0].offset
		last := sectionStart + 4 + links[len(links)-1].offset
		page.WriteByte('L')
		writeUint24(&page, first)
		writeUint24(&page, first)
		writeUint24(&page, last)
	}

	// One visible layout row containing every drawing instruction. Two
	// background rectangles plus the text blocks are indexed in that row.
	instructionCount := 1
	if !spec.HideAccent {
		instructionCount++
	}
	for _, line := range spec.Lines {
		if line.Background != 0 {
			instructionCount++
		}
		if !line.BackgroundOnly {
			instructionCount++
		}
	}
	page.Write([]byte{'M', 'C'})
	writeUint24(&page, 14)
	// The first value is the total number of per-row instruction index
	// entries. Setting it to zero leaves only the seven row metadata slots;
	// the MIDlet then faults while indexing the first drawing command and the
	// header is shown above an empty page.
	writeUint16(&page, instructionCount)
	page.WriteByte(0x1d)
	writeUint16(&page, 0)
	writeUint24(&page, 0)
	writeUint24(&page, documentHeight)
	page.WriteByte(3)
	writeUint16(&page, instructionCount)

	// Dark blue canvas and a cyan title stripe.
	writeRectangle(&page, 'B', 0, 0, 231, documentHeight)
	writeUint32(&page, spec.Background)
	if !spec.HideAccent {
		writeRectangle(&page, 'B', 0, 0, 231, 6)
		writeUint32(&page, spec.Accent)
	}

	type imageResource struct {
		pointerOffset int
		data          []byte
	}
	resources := make([]imageResource, 0)
	previousX, previousY := 0, 0
	for _, positioned := range positionedLines {
		line := positioned.line
		x, y, width, height := positioned.x, positioned.y, positioned.width, positioned.height
		if line.Background != 0 {
			backgroundX, backgroundY, backgroundWidth, backgroundHeight := x, y, width, height
			if line.BackgroundPositioned {
				backgroundX, backgroundY = line.BackgroundX, line.BackgroundY
				backgroundWidth, backgroundHeight = line.BackgroundWidth, line.BackgroundHeight
			}
			writeRectangle(&page, 'B', backgroundX-previousX, backgroundY-previousY, backgroundWidth, backgroundHeight)
			writeUint32(&page, line.Background)
			previousX, previousY = backgroundX, backgroundY
		}
		if line.BackgroundOnly {
			continue
		}
		if len(line.Image) > 0 {
			if len(line.Image) > 0xffff {
				return nil, fmt.Errorf("OM4 inline image is too large: %d", len(line.Image))
			}
			writeRectangle(&page, 'I', x-previousX, y-previousY, width, height)
			fallback := line.Color
			if fallback == 0 {
				fallback = spec.Background
			}
			writeUint32(&page, fallback)
			page.Write([]byte{0, 0, 0}) // x/y source offset and image flags
			pointerOffset := page.Len()
			writeUint24(&page, 0)
			resources = append(resources, imageResource{pointerOffset: pointerOffset, data: append([]byte(nil), line.Image...)})
			previousX, previousY = x, y
			continue
		}
		writeRectangle(&page, 'T', x-previousX, y-previousY, width, height)
		color := line.Color
		if color == 0 {
			color = 0xffe8f1f8
		}
		writeUint32(&page, color)
		page.WriteByte(line.Font)
		if err := writeModifiedUTF(&page, line.Text); err != nil {
			return nil, err
		}
		previousX, previousY = x, y
	}
	for _, resource := range resources {
		pointer := page.Len()
		writeUint16(&page, len(resource.data))
		page.Write(resource.data)
		encoded := page.Bytes()
		encoded[resource.pointerOffset] = byte(pointer >> 16)
		encoded[resource.pointerOffset+1] = byte(pointer >> 8)
		encoded[resource.pointerOffset+2] = byte(pointer)
	}
	return page.Bytes(), nil
}

type positionedWelcomeLine struct {
	line                WelcomeLine
	x, y, width, height int
}

func positionWelcomeLines(lines []WelcomeLine) ([]positionedWelcomeLine, int) {
	result := make([]positionedWelcomeLine, 0, len(lines))
	rowY := 2
	rowHeight, rowGap := 0, 0
	documentBottom := 0
	for index, line := range lines {
		if line.Absolute {
			height := line.Height
			if height <= 0 {
				height = 14
			}
			x, width := welcomeLineGeometry(line)
			result = append(result, positionedWelcomeLine{line: line, x: x, y: line.Y, width: width, height: height})
			if !line.BackgroundOnly {
				documentBottom = max(documentBottom, line.Y+height)
			}
			continue
		}
		if index > 0 && !line.SameRow {
			rowY += rowHeight + rowGap
			rowHeight, rowGap = 0, 0
		}
		height := line.Height
		if height <= 0 {
			height = 28
		}
		x, width := welcomeLineGeometry(line)
		result = append(result, positionedWelcomeLine{line: line, x: x, y: rowY, width: width, height: height})
		rowHeight = max(rowHeight, height)
		rowGap = max(rowGap, line.Gap)
		documentBottom = max(documentBottom, rowY+height)
	}
	return result, max(documentBottom+28, rowY+rowHeight+rowGap+28)
}

func welcomeLineGeometry(line WelcomeLine) (int, int) {
	if line.Positioned {
		width := line.Width
		if width <= 0 || width > 231 {
			width = 231
		}
		return line.X, width
	}
	if len(line.Image) == 0 {
		return 12, 207
	}
	width := line.Width
	if width <= 0 || width > 231 {
		width = 231
	}
	return (231 - width) / 2, width
}

// EncodeApplicationDocument wraps an OM4 page and control records into the
// channel frames consumed by the 4.x MIDlet.
func EncodeApplicationDocument(id int, records []DocumentRecord, page []byte) ([]Frame, error) {
	if len(page) == 0 {
		return nil, fmt.Errorf("cannot encode an empty OM4 page")
	}
	var recordData bytes.Buffer
	for _, record := range records {
		if len(record.Payload) > 0xffff {
			return nil, fmt.Errorf("OM4 control record %q is too large", record.Type)
		}
		recordData.WriteByte(record.Type)
		writeUint16(&recordData, len(record.Payload))
		recordData.Write(record.Payload)
	}
	var inflated bytes.Buffer
	writeUint24(&inflated, recordData.Len())
	inflated.Write(recordData.Bytes())
	writeUint24(&inflated, len(page))
	inflated.Write(page)

	var compressed bytes.Buffer
	deflater, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(deflater, bytes.NewReader(inflated.Bytes())); err != nil {
		_ = deflater.Close()
		return nil, err
	}
	if err = deflater.Close(); err != nil {
		return nil, err
	}

	token := make([]byte, 8)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	frames := []Frame{
		{Type: 15, Channel: 0, Payload: token},
		{Type: 17, Channel: 0},
		{Type: 9, Channel: 1},
		{Type: 10, Channel: 1, Payload: []byte{byte(id >> 16), byte(id >> 8), byte(id)}},
	}
	data := compressed.Bytes()
	for len(data) > 0 {
		chunk := len(data)
		if chunk > 1000 {
			chunk = 1000
		}
		frames = append(frames, Frame{Type: 11, Channel: 1, Payload: append([]byte(nil), data[:chunk]...)})
		data = data[chunk:]
	}
	frames = append(frames, Frame{Type: 6, Channel: 1})
	return frames, nil
}

func writeRectangle(out *bytes.Buffer, tag byte, x, y, width, height int) {
	out.WriteByte(tag)
	writeRectangleBody(out, x, y, width, height)
}

func writeRectangleBody(out *bytes.Buffer, x, y, width, height int) {
	writeUint16(out, x)
	writeUint24(out, y)
	writeUint16(out, width)
	writeUint24(out, height)
}

func writeUint16(out *bytes.Buffer, value int) {
	var data [2]byte
	binary.BigEndian.PutUint16(data[:], uint16(value))
	out.Write(data[:])
}

func writeUint24(out *bytes.Buffer, value int) {
	out.WriteByte(byte(value >> 16))
	out.WriteByte(byte(value >> 8))
	out.WriteByte(byte(value))
}

func writeUint32(out *bytes.Buffer, value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	out.Write(data[:])
}

func writeModifiedUTF(out *bytes.Buffer, value string) error {
	var encoded bytes.Buffer
	for _, r := range value {
		switch {
		case r == 0:
			encoded.Write([]byte{0xc0, 0x80})
		case r <= 0x7f:
			encoded.WriteByte(byte(r))
		case r <= 0x7ff:
			encoded.WriteByte(0xc0 | byte(r>>6))
			encoded.WriteByte(0x80 | byte(r&0x3f))
		case r <= 0xffff:
			encoded.WriteByte(0xe0 | byte(r>>12))
			encoded.WriteByte(0x80 | byte((r>>6)&0x3f))
			encoded.WriteByte(0x80 | byte(r&0x3f))
		default:
			for _, surrogate := range utf16.Encode([]rune{r}) {
				encoded.WriteByte(0xe0 | byte(surrogate>>12))
				encoded.WriteByte(0x80 | byte((surrogate>>6)&0x3f))
				encoded.WriteByte(0x80 | byte(surrogate&0x3f))
			}
		}
	}
	if encoded.Len() > 0xffff {
		return fmt.Errorf("modified UTF string is too long: %d", encoded.Len())
	}
	writeUint16(out, encoded.Len())
	out.Write(encoded.Bytes())
	return nil
}
