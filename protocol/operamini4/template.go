package operamini4

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
)

// defaultStartupFrames is a generic, decrypted Opera Mini 4.2 application
// startup request. It contains no transport keys, MACs or application-session
// header; ReferenceClient creates fresh transport state for every exchange.
const defaultStartupFrames = "gAIAAAAIAGxpbmtpZAAQAAAAAAAFAQAAAAAIAQAAAR4AAAEagAYBCENMREMtMS4xAghNSURQLTIuMAMSTm9raWE2NzAwYy0xLzEzLjEwBAVlbi1VUwUKSVNPLTg4NTktMUYAAAAgbVsYgsvbRGa76EJLu5AKUBIrrJj2Wn+o7PM2nJQ5p4gHFHNlYXJjaDpnb29nbGUwOD9xPSVz0AAH6AGT59QAAAEalWQWBzIwNzI1ODCXAtgAAJgVoAchACIGbGlua2lkKAAxAnJ1NSdodHRwOi8vd3d3Lm9wZXJhbWluaS5jb20vZmlyc3R0aW1lLzQuMi82MU9wZXJhIE1pbmkvNC4yLjE1NDEwTW9kLmJ5LkhhbmRsZXIvaGlmaS9saW5raWQvZW45APpqWQH6Ow1FdXJvcGUvTW9zY293"

// DefaultStartupRequest returns a fresh copy of the safe built-in startup
// request used by the plaintext bridge and OM4 comparison tooling.
func DefaultStartupRequest() (*SessionRequest, error) {
	data, err := base64.StdEncoding.DecodeString(defaultStartupFrames)
	if err != nil {
		return nil, err
	}
	frames := make([]Frame, 0, 4)
	for offset := 0; offset < len(data); {
		if offset+6 > len(data) {
			return nil, errors.New("truncated embedded OM4 frame header")
		}
		length := int(binary.BigEndian.Uint32(data[offset+2 : offset+6]))
		if offset+6+length > len(data) {
			return nil, errors.New("truncated embedded OM4 frame payload")
		}
		frames = append(frames, Frame{
			Type: data[offset], Channel: data[offset+1],
			Payload: append([]byte(nil), data[offset+6:offset+6+length]...),
		})
		offset += 6 + length
	}
	return &SessionRequest{Frames: frames}, nil
}
