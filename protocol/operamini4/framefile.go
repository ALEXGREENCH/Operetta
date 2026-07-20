package operamini4

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// ReadFrameFile reads the decrypted length-delimited corpus frame format.
func ReadFrameFile(path string) ([]Frame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	frames := make([]Frame, 0)
	for offset := 0; offset < len(data); {
		if offset+6 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		length := int(binary.BigEndian.Uint32(data[offset+2 : offset+6]))
		if length < 0 || offset+6+length > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		frames = append(frames, Frame{
			Type:    data[offset],
			Channel: data[offset+1],
			Payload: append([]byte(nil), data[offset+6:offset+6+length]...),
		})
		offset += 6 + length
	}
	return frames, nil
}

// WriteFrameFile writes decrypted frames without transport keys or MACs.
func WriteFrameFile(path string, frames []Frame) error {
	data := make([]byte, 0)
	for _, frame := range frames {
		if uint64(len(frame.Payload)) > uint64(^uint32(0)) {
			return fmt.Errorf("OM4 frame payload is too large: %d", len(frame.Payload))
		}
		record := make([]byte, 6+len(frame.Payload))
		record[0], record[1] = frame.Type, frame.Channel
		binary.BigEndian.PutUint32(record[2:6], uint32(len(frame.Payload)))
		copy(record[6:], frame.Payload)
		data = append(data, record...)
	}
	return os.WriteFile(path, data, 0o600)
}
