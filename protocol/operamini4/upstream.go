package operamini4

import (
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// UpstreamSession is a diagnostic OM4 client session used to compare
// Operetta's output with a reference server.
type UpstreamSession struct {
	responseKey    []byte
	responseMACKey []byte
	responseAuth   []byte
}

// BuildUpstreamRequest re-wraps decrypted frames for an OM4 bootstrap reply.
// The caller is responsible for obtaining that reply from the reference
// endpoint with the supplied eight-byte hello nonce.
func BuildUpstreamRequest(bootstrap, helloNonce []byte, source *SessionRequest) ([]byte, *UpstreamSession, error) {
	if len(helloNonce) != 8 || source == nil {
		return nil, nil, errors.New("OM4 upstream request requires an eight-byte nonce and source request")
	}
	if len(bootstrap) < 3 || bootstrap[0] != 2 {
		return nil, nil, errors.New("invalid OM4 upstream bootstrap response")
	}
	modulusBytes := 128 + int(bootstrap[1])*4
	tokenLength := int(bootstrap[2])
	modulusOffset := 163 + tokenLength
	if modulusOffset+modulusBytes+8 > len(bootstrap) {
		return nil, nil, errors.New("truncated OM4 upstream bootstrap response")
	}
	token := append([]byte(nil), bootstrap[2:3+tokenLength]...)
	modulus := new(big.Int).SetBytes(bootstrap[modulusOffset : modulusOffset+modulusBytes])

	rsaPlain := make([]byte, modulusBytes)
	rsaPlain[0], rsaPlain[1] = 0, 2
	for i := 2; i < modulusBytes-33; i++ {
		for rsaPlain[i] == 0 {
			if _, err := rand.Read(rsaPlain[i : i+1]); err != nil {
				return nil, nil, err
			}
		}
	}
	rsaPlain[modulusBytes-33] = 0
	seed := rsaPlain[modulusBytes-32:]
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, err
	}
	rsaCipher := new(big.Int).Exp(new(big.Int).SetBytes(rsaPlain), big.NewInt(3), modulus)
	rsaCipherBytes := fixedBytes(rsaCipher, modulusBytes)
	keys := deriveSessionKeys(seed, 0)

	clientRandom := make([]byte, 8)
	if _, err := rand.Read(clientRandom); err != nil {
		return nil, nil, err
	}
	packet := []byte{1, 1, 1}
	packet = append(packet, clientRandom...)
	packet = append(packet, token...)
	packet = append(packet, 0, 0)
	packet = append(packet, rsaCipherBytes...)
	cryptOffset := len(packet)
	if len(source.Header) > 65535 {
		return nil, nil, errors.New("OM4 upstream header is too long")
	}
	headerLength := make([]byte, 2)
	binary.BigEndian.PutUint16(headerLength, uint16(len(source.Header)))
	packet = append(packet, headerLength...)
	packet = append(packet, source.Header...)

	requestAuth := append(append([]byte(nil), helloNonce...), packet[2:]...)
	packet = append(packet, hmacSHA256(keys[0], requestAuth)[:16]...)
	for counter, frame := range source.Frames {
		packet = append(packet, encodeResponseFrame(keys[0], uint32(counter), frame)...)
	}
	stream, err := rc4.NewCipher(keys[2])
	if err != nil {
		return nil, nil, err
	}
	discard := make([]byte, 1024)
	stream.XORKeyStream(discard, discard)
	stream.XORKeyStream(packet[cryptOffset:], packet[cryptOffset:])
	return packet, &UpstreamSession{
		responseKey:    append([]byte(nil), keys[3]...),
		responseMACKey: append([]byte(nil), keys[1]...),
		responseAuth:   requestAuth,
	}, nil
}

// ReadResponse decrypts reference-server records until the logical request
// channel is closed and returns its application byte stream.
func (session *UpstreamSession) ReadResponse(reader io.Reader) ([]byte, []Frame, error) {
	if session == nil {
		return nil, nil, errors.New("nil OM4 upstream session")
	}
	prefix := []byte{0}
	if _, err := io.ReadFull(reader, prefix); err != nil {
		return nil, nil, err
	}
	if prefix[0] != 3 {
		return nil, nil, fmt.Errorf("unexpected OM4 upstream response type %d", prefix[0])
	}
	stream, err := rc4.NewCipher(session.responseKey)
	if err != nil {
		return nil, nil, err
	}
	discard := make([]byte, 1024)
	stream.XORKeyStream(discard, discard)
	ack := make([]byte, 4)
	if _, err := io.ReadFull(reader, ack); err != nil {
		return nil, nil, err
	}
	stream.XORKeyStream(ack, ack)
	wantAck := hmacSHA256(session.responseMACKey, session.responseAuth)[:4]
	if subtle.ConstantTimeCompare(ack, wantAck) != 1 {
		return nil, nil, errors.New("OM4 upstream acknowledgement MAC mismatch")
	}

	var application bytes.Buffer
	frames := make([]Frame, 0, 8)
	for counter := uint32(0); ; counter++ {
		header := make([]byte, 6)
		if _, err := io.ReadFull(reader, header); err != nil {
			return application.Bytes(), frames, err
		}
		stream.XORKeyStream(header, header)
		length := int(binary.BigEndian.Uint16(header[4:6]))
		if length < 2 || length > 65535 {
			return nil, frames, fmt.Errorf("invalid OM4 upstream frame length %d", length)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return application.Bytes(), frames, err
		}
		stream.XORKeyStream(body, body)
		macBody := append(append([]byte(nil), header[4:6]...), body...)
		counterBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(counterBytes, counter)
		wantMAC := hmacSHA256(session.responseMACKey, append(counterBytes, macBody...))[:4]
		if subtle.ConstantTimeCompare(header[:4], wantMAC) != 1 {
			return nil, frames, fmt.Errorf("OM4 upstream frame %d MAC mismatch", counter)
		}
		frame := Frame{Type: body[0], Channel: body[1], Payload: append([]byte(nil), body[2:]...)}
		frames = append(frames, frame)
		if frame.Channel == 1 && frame.Type == 10 {
			_, _ = application.Write(frame.Payload)
		}
		if frame.Channel == 1 && frame.Type == 6 {
			return application.Bytes(), frames, nil
		}
	}
}
