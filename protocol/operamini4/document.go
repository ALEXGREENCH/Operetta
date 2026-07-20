package operamini4

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

// DocumentRecord is a length-delimited control record preceding an OM4 page.
type DocumentRecord struct {
	Type    byte
	Payload []byte
}

// PageHeader contains the stable fields at the beginning of an OM4 page
// bytecode stream.
type PageHeader struct {
	Version        byte
	ViewportWidth  int
	DocumentHeight int
	Title          string
	Base           string
	URL            string
	Flags          byte
	ContentOffset  int
}

// TextElement is a decoded OM4 text drawing instruction.
type TextElement struct {
	X, Y, Width, Height int
	Style               uint32
	Font                byte
	Text                string
}

// LinkElement is one focusable OM4 link hit region decoded from an S section.
type LinkElement struct {
	X, Y, Width, Height int
	Tooltip             string
	URL                 string
}

// DrawingElement is one absolute drawing command after OM4's signed delta
// coordinates have been accumulated. ImagePointer addresses a uint16-sized
// embedded JPEG/PNG blob in the same page.
type DrawingElement struct {
	Kind                byte
	X, Y, Width, Height int
	Color               uint32
	Font                byte
	Text                string
	ImageX, ImageY      int
	ImageFlags          byte
	ImagePointer        int
	ControlKind         byte
	ControlFlags        byte
}

// ApplicationDocument is the decompressed application payload carried by one
// OM4 response channel.
type ApplicationDocument struct {
	ID             int
	Inflated       []byte
	Records        []DocumentRecord
	Page           []byte
	Header         PageHeader
	Texts          []TextElement
	Links          []LinkElement
	Drawings       []DrawingElement
	InflateWarning error
}

// DecodeApplicationDocument reconstructs the type-10/type-11 application
// stream and decodes the stable OM4 document envelope. Reference servers end
// raw-DEFLATE responses with a sync flush, so io.ErrUnexpectedEOF is retained
// as a warning when all declared document sections are nevertheless complete.
func DecodeApplicationDocument(frames []Frame) (*ApplicationDocument, error) {
	var prefix bytes.Buffer
	var compressed bytes.Buffer
	for _, frame := range frames {
		if frame.Channel != 1 {
			continue
		}
		switch frame.Type {
		case 10:
			_, _ = prefix.Write(frame.Payload)
		case 11:
			_, _ = compressed.Write(frame.Payload)
		}
	}
	if prefix.Len() != 3 {
		return nil, fmt.Errorf("OM4 document prefix is %d bytes, want 3", prefix.Len())
	}
	if compressed.Len() == 0 {
		return nil, errors.New("OM4 document has no compressed frames")
	}
	reader := flate.NewReader(bytes.NewReader(compressed.Bytes()))
	inflated, inflateErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if inflateErr == nil {
		inflateErr = closeErr
	}
	if inflateErr != nil && !errors.Is(inflateErr, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("inflate OM4 document: %w", inflateErr)
	}
	document := &ApplicationDocument{
		ID:             readUint24(prefix.Bytes()),
		Inflated:       inflated,
		InflateWarning: inflateErr,
	}
	if err := document.decodeEnvelope(); err != nil {
		return nil, err
	}
	return document, nil
}

func (document *ApplicationDocument) decodeEnvelope() error {
	if len(document.Inflated) < 3 {
		return errors.New("truncated OM4 document record section")
	}
	recordBytes := readUint24(document.Inflated[:3])
	recordEnd := 3 + recordBytes
	if recordEnd > len(document.Inflated) {
		return fmt.Errorf("truncated OM4 records: declared=%d available=%d", recordBytes, len(document.Inflated)-3)
	}
	for offset := 3; offset < recordEnd; {
		if offset+3 > recordEnd {
			return errors.New("truncated OM4 control record header")
		}
		recordType := document.Inflated[offset]
		length := int(binary.BigEndian.Uint16(document.Inflated[offset+1 : offset+3]))
		offset += 3
		if offset+length > recordEnd {
			return fmt.Errorf("truncated OM4 control record %q", recordType)
		}
		document.Records = append(document.Records, DocumentRecord{
			Type:    recordType,
			Payload: append([]byte(nil), document.Inflated[offset:offset+length]...),
		})
		offset += length
	}
	if recordEnd+3 > len(document.Inflated) {
		return errors.New("missing OM4 page length")
	}
	pageBytes := readUint24(document.Inflated[recordEnd : recordEnd+3])
	pageStart := recordEnd + 3
	if pageStart+pageBytes > len(document.Inflated) {
		return fmt.Errorf("truncated OM4 page: declared=%d available=%d", pageBytes, len(document.Inflated)-pageStart)
	}
	document.Page = append([]byte(nil), document.Inflated[pageStart:pageStart+pageBytes]...)
	header, err := DecodePageHeader(document.Page)
	if err != nil {
		return fmt.Errorf("decode OM4 page header: %w", err)
	}
	document.Header = header
	document.Texts = DecodeTextElements(document.Page, header.ContentOffset)
	document.Links = DecodeLinkElements(document.Page, header.ContentOffset)
	document.Drawings = DecodeDrawingElements(document.Page, header.ContentOffset)
	return nil
}

// DecodeDrawingElements decodes the stable B/T/I drawing subset used by the
// native encoder and reference pages. OM4 stores x/y as signed deltas and
// rewrites them to absolute coordinates during the MIDlet's initial pass.
func DecodeDrawingElements(page []byte, offset int) []DrawingElement {
	drawings := make([]DrawingElement, 0)
	x, y := 0, 0
	for offset < len(page) {
		tag := page[offset]
		offset++
		switch tag {
		case 'S':
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return drawings
			}
			offset = next + length
			continue
		case 'L':
			if offset+9 > len(page) {
				return drawings
			}
			offset += 9
			continue
		case 'M':
			if offset >= len(page) {
				return drawings
			}
			offset++
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return drawings
			}
			offset = next + length
			continue
		}
		if offset+10 > len(page) {
			return drawings
		}
		dx := int(int16(binary.BigEndian.Uint16(page[offset : offset+2])))
		dy := signedUint24(page[offset+2 : offset+5])
		width := int(binary.BigEndian.Uint16(page[offset+5 : offset+7]))
		height := readUint24(page[offset+7 : offset+10])
		offset += 10
		x += dx
		y += dy
		element := DrawingElement{Kind: tag, X: x, Y: y, Width: width, Height: height}
		switch tag {
		case 'B':
			if offset+4 > len(page) {
				return drawings
			}
			element.Color = binary.BigEndian.Uint32(page[offset : offset+4])
			offset += 4
		case 'T':
			if offset+5 > len(page) {
				return drawings
			}
			element.Color = binary.BigEndian.Uint32(page[offset : offset+4])
			element.Font = page[offset+4]
			offset += 5
			value, next, err := readModifiedUTF(page, offset)
			if err != nil {
				return drawings
			}
			element.Text = value
			offset = next
		case 'I':
			if offset+10 > len(page) {
				return drawings
			}
			element.Color = binary.BigEndian.Uint32(page[offset : offset+4])
			element.ImageX = int(page[offset+4])
			element.ImageY = int(page[offset+5])
			element.ImageFlags = page[offset+6]
			element.ImagePointer = readUint24(page[offset+7 : offset+10])
			offset += 10
		case 'F':
			if offset+6 > len(page) {
				return drawings
			}
			element.Color = binary.BigEndian.Uint32(page[offset : offset+4])
			element.ControlKind = page[offset+4]
			element.ControlFlags = page[offset+5]
			offset += 6 // color, control kind and flags
			_, next, err := readModifiedUTF(page, offset)
			if err != nil {
				return drawings
			}
			_, next, err = readModifiedUTF(page, next)
			if err != nil || next+3 > len(page) {
				return drawings
			}
			offset = next + 3
		default:
			// Unknown drawing payload length cannot be recovered safely.
			return drawings
		}
		drawings = append(drawings, element)
	}
	return drawings
}

func signedUint24(data []byte) int {
	value := readUint24(data)
	if value&0x800000 != 0 {
		value -= 1 << 24
	}
	return value
}

// DecodePageHeader decodes fields read by Opera Mini 4 before it starts
// processing the page drawing bytecode.
func DecodePageHeader(page []byte) (PageHeader, error) {
	var header PageHeader
	if len(page) < 11 {
		return header, errors.New("page header is too short")
	}
	header.Version = page[0]
	header.ViewportWidth = int(binary.BigEndian.Uint16(page[1:3]))
	header.DocumentHeight = readUint24(page[3:6])
	offset := 11 // version, width, height, three flags and one uint16 field
	var err error
	header.Title, offset, err = readModifiedUTF(page, offset)
	if err != nil {
		return header, err
	}
	if offset+2 > len(page) {
		return header, errors.New("missing OM4 favicon length")
	}
	faviconBytes := int(binary.BigEndian.Uint16(page[offset : offset+2]))
	offset += 2
	if offset+faviconBytes > len(page) {
		return header, errors.New("truncated OM4 favicon")
	}
	offset += faviconBytes
	header.Base, offset, err = readModifiedUTF(page, offset)
	if err != nil {
		return header, err
	}
	header.URL, offset, err = readModifiedUTF(page, offset)
	if err != nil {
		return header, err
	}
	if len(header.URL) > 0 && header.URL[0] == 0 {
		header.URL = header.Base + header.URL[1:]
	}
	if offset >= len(page) {
		return header, errors.New("missing OM4 page flags")
	}
	header.Flags = page[offset]
	header.ContentOffset = offset + 1
	return header, nil
}

// DecodeTextElements performs a conservative pass over stable drawing
// instructions. Unknown instructions stop only the current scan; envelope and
// header decoding remain useful while more opcodes are learned.
func DecodeTextElements(page []byte, offset int) []TextElement {
	texts := make([]TextElement, 0)
	for offset < len(page) {
		tag := page[offset]
		offset++
		switch tag {
		case 'S':
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return texts
			}
			offset = next + length
			continue
		case 'L':
			if offset+9 > len(page) {
				return texts
			}
			offset += 9
			continue
		case 'M':
			if offset >= len(page) {
				return texts
			}
			offset++ // subtype
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return texts
			}
			offset = next + length
			continue
		}
		if offset+10 > len(page) {
			return texts
		}
		x := int(binary.BigEndian.Uint16(page[offset : offset+2]))
		y := readUint24(page[offset+2 : offset+5])
		width := int(binary.BigEndian.Uint16(page[offset+5 : offset+7]))
		height := readUint24(page[offset+7 : offset+10])
		offset += 10
		switch tag {
		case 'T':
			if offset+5 > len(page) {
				return texts
			}
			style := binary.BigEndian.Uint32(page[offset : offset+4])
			font := page[offset+4]
			offset += 5
			value, next, err := readModifiedUTF(page, offset)
			if err != nil {
				return texts
			}
			offset = next
			texts = append(texts, TextElement{X: x, Y: y, Width: width, Height: height, Style: style, Font: font, Text: value})
		case 'B':
			if offset+4 > len(page) {
				return texts
			}
			offset += 4
		case 'I':
			if offset+10 > len(page) {
				return texts
			}
			offset += 10
		case 'F':
			if offset+6 > len(page) {
				return texts
			}
			offset += 6
			_, next, err := readModifiedUTF(page, offset)
			if err != nil {
				return texts
			}
			_, next, err = readModifiedUTF(page, next)
			if err != nil || next+3 > len(page) {
				return texts
			}
			offset = next + 3
		case 'L':
			if offset+9 > len(page) {
				return texts
			}
			offset += 9
		default:
			// Several drawing opcodes have no payload beyond their rectangle.
		}
	}
	return texts
}

// DecodeLinkElements conservatively decodes the L entries emitted inside an
// interactive S section. Unknown interaction records end only that section.
func DecodeLinkElements(page []byte, offset int) []LinkElement {
	links := make([]LinkElement, 0)
	for offset < len(page) {
		tag := page[offset]
		offset++
		switch tag {
		case 'S':
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return links
			}
			end := next + length
			for next < end {
				kind := page[next]
				next++
				if next >= end {
					return links
				}
				count := int(page[next])
				next++
				if count <= 0 || next+count*10 > end {
					return links
				}
				x := int(binary.BigEndian.Uint16(page[next : next+2]))
				y := readUint24(page[next+2 : next+5])
				width := int(binary.BigEndian.Uint16(page[next+5 : next+7]))
				height := readUint24(page[next+7 : next+10])
				next += count * 10
				tooltip, after, err := readModifiedUTF(page, next)
				if err != nil || after > end {
					return links
				}
				value, after, err := readModifiedUTF(page, after)
				if err != nil || after > end {
					return links
				}
				if kind == 'L' {
					links = append(links, LinkElement{X: x, Y: y, Width: width, Height: height, Tooltip: tooltip, URL: value})
				}
				next = after
			}
			offset = end
			continue
		case 'L':
			if offset+9 > len(page) {
				return links
			}
			offset += 9
			continue
		case 'M':
			if offset >= len(page) {
				return links
			}
			offset++
			length, next, ok := takeUint24(page, offset)
			if !ok || next+length > len(page) {
				return links
			}
			offset = next + length
			continue
		}
		// The link section is emitted before drawing instructions. Once drawing
		// starts, no further interactive metadata is expected from this encoder.
		return links
	}
	return links
}

func readUint24(data []byte) int {
	return int(data[0])<<16 | int(data[1])<<8 | int(data[2])
}

func takeUint24(data []byte, offset int) (int, int, bool) {
	if offset+3 > len(data) {
		return 0, offset, false
	}
	return readUint24(data[offset : offset+3]), offset + 3, true
}

func readModifiedUTF(data []byte, offset int) (string, int, error) {
	if offset+2 > len(data) {
		return "", offset, errors.New("truncated modified UTF length")
	}
	length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
	offset += 2
	if offset+length > len(data) {
		return "", offset, errors.New("truncated modified UTF value")
	}
	encoded := data[offset : offset+length]
	units := make([]uint16, 0, len(encoded))
	for i := 0; i < len(encoded); {
		value := encoded[i]
		switch {
		case value&0x80 == 0:
			units = append(units, uint16(value))
			i++
		case value&0xe0 == 0xc0 && i+1 < len(encoded):
			units = append(units, uint16(value&0x1f)<<6|uint16(encoded[i+1]&0x3f))
			i += 2
		case value&0xf0 == 0xe0 && i+2 < len(encoded):
			units = append(units, uint16(value&0x0f)<<12|uint16(encoded[i+1]&0x3f)<<6|uint16(encoded[i+2]&0x3f))
			i += 3
		default:
			return "", offset, errors.New("invalid modified UTF value")
		}
	}
	return string(utf16.Decode(units)), offset + length, nil
}
