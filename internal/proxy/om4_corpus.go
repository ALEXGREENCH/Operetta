package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"operetta/protocol/operamini4"
)

type om4CorpusMeta struct {
	CreatedAt      string               `json:"created_at"`
	Sequence       uint16               `json:"sequence"`
	HeaderHash     string               `json:"application_header_sha256,omitempty"`
	Request        string               `json:"request_file"`
	Response       string               `json:"response_file"`
	RequestFrames  []om4CorpusFrameMeta `json:"request_frames"`
	ResponseFrames []om4CorpusFrameMeta `json:"response_frames"`
}

type om4CorpusFrameMeta struct {
	Type    byte `json:"type"`
	Channel byte `json:"channel"`
	Bytes   int  `json:"payload_bytes"`
}

func (s *Server) saveOM4Corpus(request *operamini4.SessionRequest, frames []operamini4.Frame) {
	if s == nil || request == nil || s.cfg.OM4CorpusDir == "" {
		return
	}
	if err := os.MkdirAll(s.cfg.OM4CorpusDir, 0o700); err != nil {
		s.logger.Printf("OM4 corpus create error: %v", err)
		return
	}
	hash := sha256.Sum256(request.Plaintext)
	base := fmt.Sprintf("%s-seq%05d-%s", s.clock().UTC().Format("20060102T150405.000000000Z"), request.Sequence, hex.EncodeToString(hash[:6]))
	requestName := base + ".request.frames.bin"
	responseName := base + ".response.frames.bin"
	metaName := base + ".json"

	safeRequestFrames := sanitizeOM4RequestFrames(request.Frames)
	requestData, requestMeta := encodeOM4CorpusFrames(safeRequestFrames)
	responseData, responseMeta := encodeOM4CorpusFrames(frames)
	headerHash := sha256.Sum256([]byte(request.Header))
	meta := om4CorpusMeta{
		CreatedAt:      s.clock().UTC().Format(time.RFC3339Nano),
		Sequence:       request.Sequence,
		HeaderHash:     hex.EncodeToString(headerHash[:]),
		Request:        requestName,
		Response:       responseName,
		RequestFrames:  requestMeta,
		ResponseFrames: responseMeta,
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		s.logger.Printf("OM4 corpus metadata error: %v", err)
		return
	}
	writes := []struct {
		name string
		data []byte
	}{
		{requestName, requestData},
		{responseName, responseData},
		{metaName, append(metaJSON, '\n')},
	}
	for _, item := range writes {
		if err := os.WriteFile(filepath.Join(s.cfg.OM4CorpusDir, item.name), item.data, 0o600); err != nil {
			s.logger.Printf("OM4 corpus write error: %v", err)
			return
		}
	}
	s.logger.Printf("OM4 corpus saved %s request_frames=%d response_frames=%d", base, len(request.Frames), len(frames))
}

func sanitizeOM4RequestFrames(frames []operamini4.Frame) []operamini4.Frame {
	safe := make([]operamini4.Frame, len(frames))
	for i, frame := range frames {
		safe[i] = operamini4.Frame{Type: frame.Type, Channel: frame.Channel, Payload: append([]byte(nil), frame.Payload...)}
		if frame.Channel == 1 && frame.Type == 8 {
			redactOM4FormValues(safe[i].Payload)
		}
	}
	return safe
}

func redactOM4FormValues(payload []byte) {
	lower := bytes.ToLower(payload)
	keys := [][]byte{
		[]byte("password="), []byte("passwd="), []byte("pwd="),
		[]byte("pass="), []byte("token="), []byte("secret="),
	}
	for _, key := range keys {
		for searchFrom := 0; searchFrom < len(lower); {
			relative := bytes.Index(lower[searchFrom:], key)
			if relative < 0 {
				break
			}
			valueStart := searchFrom + relative + len(key)
			valueEnd := valueStart
			for valueEnd < len(payload) && payload[valueEnd] != '&' && payload[valueEnd] != 0 {
				payload[valueEnd] = '*'
				lower[valueEnd] = '*'
				valueEnd++
			}
			searchFrom = valueEnd + 1
		}
	}
}

func encodeOM4CorpusFrames(frames []operamini4.Frame) ([]byte, []om4CorpusFrameMeta) {
	data := make([]byte, 0)
	meta := make([]om4CorpusFrameMeta, 0, len(frames))
	for _, frame := range frames {
		record := make([]byte, 6+len(frame.Payload))
		record[0], record[1] = frame.Type, frame.Channel
		binary.BigEndian.PutUint32(record[2:6], uint32(len(frame.Payload)))
		copy(record[6:], frame.Payload)
		data = append(data, record...)
		meta = append(meta, om4CorpusFrameMeta{Type: frame.Type, Channel: frame.Channel, Bytes: len(frame.Payload)})
	}
	return data, meta
}
