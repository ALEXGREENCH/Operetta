package operamini4

import (
	"crypto/hmac"
	"crypto/rc4"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// RequestURLs returns length-prefixed HTTP(S) strings carried by channel-one
// navigation frames. The client can include both the current and destination
// URL; callers normally use the final item.
func (request *SessionRequest) RequestURLs() []string {
	if request == nil {
		return nil
	}
	var result []string
	seen := make(map[string]struct{})
	for _, frame := range request.Frames {
		if frame.Channel != 1 || frame.Type != 8 {
			continue
		}
		payload := frame.Payload
		addCandidate := func(candidate string) {
			candidate = strings.TrimSpace(candidate)
			if !strings.HasPrefix(candidate, "http://") && !strings.HasPrefix(candidate, "https://") {
				if !looksLikeDomainTarget(candidate) {
					return
				}
				candidate = "http://" + candidate
			}
			parsed, err := url.Parse(candidate)
			if err != nil || parsed.Host == "" || strings.ContainsAny(candidate, "\x00\r\n\t") {
				return
			}
			if _, ok := seen[candidate]; !ok {
				seen[candidate] = struct{}{}
				result = append(result, candidate)
			}
		}
		for offset := 0; offset < len(payload); offset++ {
			// Application strings use a compact one-byte length for ordinary
			// URLs; some request variants use a Java-style uint16 length.
			length8 := int(payload[offset])
			if length8 >= len("http://x") && offset+1+length8 <= len(payload) {
				addCandidate(string(payload[offset+1 : offset+1+length8]))
			}
			if offset+2 <= len(payload) {
				length16 := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
				if length16 >= len("http://x") && offset+2+length16 <= len(payload) {
					addCandidate(string(payload[offset+2 : offset+2+length16]))
				}
			}
		}
	}
	return result
}

func looksLikeDomainTarget(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n\x00") {
		return false
	}
	host := value
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || len(tld) > 24 {
		return false
	}
	for _, label := range labels {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	for _, char := range tld {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
}

// Frame is one decrypted OM4 multiplexed transport record.
type Frame struct {
	Type    byte
	Channel byte
	Payload []byte
}

// SessionRequest contains the authenticated, decrypted first request.
type SessionRequest struct {
	Sequence       uint16
	Token          []byte
	Header         string
	Plaintext      []byte
	Frames         []Frame
	responseKey    []byte
	responseMACKey []byte
	responseAuth   []byte
}

func IsSessionRequest(payload []byte) bool {
	return len(payload) > 3 && payload[0] == 1 && payload[1] == 1 && payload[2] == 1
}

func ParseSessionRequest(payload []byte) (*SessionRequest, error) {
	if !IsSessionRequest(payload) || len(payload) < 12 {
		return nil, errors.New("not an OM4 session request")
	}
	tokenLength := int(payload[11])
	sequenceOffset := 12 + tokenLength
	cipherOffset := sequenceOffset + 2
	cryptOffset := cipherOffset + transportBytes
	if tokenLength < 8 || cryptOffset+18 > len(payload) {
		return nil, fmt.Errorf("invalid OM4 header lengths: token=%d packet=%d", tokenLength, len(payload))
	}
	token := append([]byte(nil), payload[12:sequenceOffset]...)
	sequence := binary.BigEndian.Uint16(payload[sequenceOffset:cipherOffset])
	rsaBlock, err := DecryptTransportBlock(payload[cipherOffset:cryptOffset])
	if err != nil {
		return nil, err
	}
	if rsaBlock[0] != 0 || rsaBlock[1] != 2 || rsaBlock[transportBytes-33] != 0 {
		return nil, errors.New("invalid OM4 RSA session envelope")
	}
	for _, value := range rsaBlock[2 : transportBytes-33] {
		if value == 0 {
			return nil, errors.New("invalid zero in OM4 RSA session padding")
		}
	}
	seed := rsaBlock[transportBytes-32:]
	keys := deriveSessionKeys(seed, sequence)

	plain := append([]byte(nil), payload...)
	stream, err := rc4.NewCipher(keys[2])
	if err != nil {
		return nil, err
	}
	discard := make([]byte, 1024)
	stream.XORKeyStream(discard, discard)
	stream.XORKeyStream(plain[cryptOffset:], plain[cryptOffset:])

	utfLength := int(binary.BigEndian.Uint16(plain[cryptOffset : cryptOffset+2]))
	macOffset := cryptOffset + 2 + utfLength
	if macOffset+16 > len(plain) {
		return nil, errors.New("truncated OM4 encrypted session header")
	}
	header := string(plain[cryptOffset+2 : macOffset])
	mac := hmac.New(sha256.New, keys[0])
	_, _ = mac.Write(token[:8])
	_, _ = mac.Write(plain[2:macOffset])
	wantMAC := mac.Sum(nil)[:16]
	if subtle.ConstantTimeCompare(wantMAC, plain[macOffset:macOffset+16]) != 1 {
		return nil, errors.New("OM4 session header MAC mismatch")
	}

	frames, err := parseFrames(plain[macOffset+16:])
	if err != nil {
		return nil, err
	}
	return &SessionRequest{
		Sequence:       sequence,
		Token:          token,
		Header:         header,
		Plaintext:      plain,
		Frames:         frames,
		responseKey:    append([]byte(nil), keys[3]...),
		responseMACKey: append([]byte(nil), keys[1]...),
		responseAuth:   append(append([]byte(nil), token[:8]...), plain[2:macOffset]...),
	}, nil
}

// ParseDecryptedSessionRequest reads the historical corpus representation in
// which the transport prefix is unchanged but the RC4-protected tail has
// already been decrypted. It is intended only for migration and diagnostics;
// response cryptographic state cannot be reconstructed from this form.
func ParseDecryptedSessionRequest(payload []byte) (*SessionRequest, error) {
	if !IsSessionRequest(payload) || len(payload) < 12 {
		return nil, errors.New("not a decrypted OM4 session request")
	}
	tokenLength := int(payload[11])
	sequenceOffset := 12 + tokenLength
	cipherOffset := sequenceOffset + 2
	cryptOffset := cipherOffset + transportBytes
	if tokenLength < 8 || cryptOffset+18 > len(payload) {
		return nil, fmt.Errorf("invalid decrypted OM4 header lengths: token=%d packet=%d", tokenLength, len(payload))
	}
	utfLength := int(binary.BigEndian.Uint16(payload[cryptOffset : cryptOffset+2]))
	macOffset := cryptOffset + 2 + utfLength
	if macOffset+16 > len(payload) {
		return nil, errors.New("truncated decrypted OM4 session header")
	}
	frames, err := parseFrames(payload[macOffset+16:])
	if err != nil {
		return nil, err
	}
	return &SessionRequest{
		Sequence:  binary.BigEndian.Uint16(payload[sequenceOffset:cipherOffset]),
		Token:     append([]byte(nil), payload[12:sequenceOffset]...),
		Header:    string(payload[cryptOffset+2 : macOffset]),
		Plaintext: append([]byte(nil), payload...),
		Frames:    frames,
	}, nil
}

// BuildApplicationResponse wraps application bytes in authenticated OM4
// records and encrypts them for the current request. Type 10 carries stream
// bytes; type 6 closes the logical channel.
func (request *SessionRequest) BuildApplicationResponse(payload []byte) ([]byte, error) {
	return request.BuildResponseFrames([]Frame{
		{Type: 10, Channel: 1, Payload: payload},
		{Type: 6, Channel: 1},
	})
}

// BuildResponseFrames authenticates and encrypts server-to-client records.
func (request *SessionRequest) BuildResponseFrames(frames []Frame) ([]byte, error) {
	if request == nil || len(request.responseKey) != 32 {
		return nil, errors.New("OM4 request has no response crypto context")
	}
	ack := hmacSHA256(request.responseMACKey, request.responseAuth)[:4]
	plain := append([]byte(nil), ack...)
	for counter, frame := range frames {
		if len(frame.Payload) > 65533 {
			return nil, errors.New("OM4 response frame is too large")
		}
		plain = append(plain, encodeResponseFrame(request.responseMACKey, uint32(counter), frame)...)
	}

	stream, err := rc4.NewCipher(request.responseKey)
	if err != nil {
		return nil, err
	}
	discard := make([]byte, 1024)
	stream.XORKeyStream(discard, discard)
	stream.XORKeyStream(plain, plain)
	return append([]byte{3}, plain...), nil
}

func encodeResponseFrame(macKey []byte, counter uint32, frame Frame) []byte {
	length := len(frame.Payload) + 2
	body := make([]byte, 2+length)
	binary.BigEndian.PutUint16(body, uint16(length))
	body[2] = frame.Type
	body[3] = frame.Channel
	copy(body[4:], frame.Payload)
	counterBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(counterBytes, counter)
	macInput := append(counterBytes, body...)
	result := append([]byte(nil), hmacSHA256(macKey, macInput)[:4]...)
	return append(result, body...)
}

func deriveSessionKeys(seed []byte, sequence uint16) [4][]byte {
	block := make([]byte, 64)
	block[0] = byte(sequence >> 8)
	block[1] = byte(sequence)
	value := hmacSHA256(seed, block)
	var keys [4][]byte
	for i := range keys {
		message := make([]byte, 0, len(value)+len(block))
		message = append(message, value...)
		message = append(message, block...)
		keys[i] = hmacSHA256(seed, message)
		value = hmacSHA256(seed, value)
	}
	return keys
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func parseFrames(data []byte) ([]Frame, error) {
	frames := make([]Frame, 0, 4)
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, fmt.Errorf("truncated OM4 frame header: %d trailing bytes", len(data))
		}
		length := int(binary.BigEndian.Uint16(data[4:6]))
		if length < 2 || 6+length > len(data) {
			return nil, fmt.Errorf("invalid OM4 frame length %d with %d bytes remaining", length, len(data))
		}
		frames = append(frames, Frame{
			Type:    data[6],
			Channel: data[7],
			Payload: append([]byte(nil), data[8:6+length]...),
		})
		data = data[6+length:]
	}
	return frames, nil
}
