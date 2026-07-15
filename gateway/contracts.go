// Package gateway contains the protocol-neutral conversion use case. Transport
// and client protocol adapters translate their requests into these contracts.
package gateway

import (
	"context"
	"fmt"

	"operetta/origin"
	"operetta/presentation"
)

type FormatID string

// FetchRequest describes origin acquisition without exposing a client wire
// protocol. Headers must already be filtered for the target origin.
type FetchRequest struct {
	URL     string
	Method  string
	Headers map[string][]string
	Body    []byte
	Session string
}

// ClientProfile contains capabilities useful to every presentation format.
type ClientProfile struct {
	ViewportWidth   int
	ViewportHeight  int
	Colors          int
	AlphaLevels     int
	MemoryBytes     int
	Images          bool
	HighQuality     bool
	PreferredImage  string
	MaxInlineBytes  int
	MaxDocumentUnit int
}

type TransformRequest struct {
	Source  *origin.Response
	Profile ClientProfile
	Headers map[string][]string
	Session string
}

type CacheScope uint8

const (
	CacheDisabled CacheScope = iota
	CachePublic
	CacheSession
)

// Metadata stays outside the display list because cookies, form state and
// cache policy are application/session concerns rather than encoded content.
type Metadata struct {
	SetCookies          []string
	FormHidden          map[string]map[string]string
	OriginTransferBytes int
	OriginDecodedBytes  int
	CacheScope          CacheScope
	Session             string
}

type TransformResult struct {
	Document *presentation.Document
	Metadata Metadata
}

type EncodeRequest struct {
	Profile     ClientProfile
	Dialect     string
	Compression string
	Page        int
	PageUnits   int
	ServerBase  string
	// Parameters are owned and validated by the selected protocol adapter.
	// The application core never interprets them.
	Parameters map[string]string
}

type Artifact struct {
	Format      FormatID
	MediaType   string
	Data        []byte
	Part        int
	Parts       int
	CachePacked []byte
	Metadata    Metadata
}

type Fetcher interface {
	Fetch(context.Context, FetchRequest) (*origin.Response, error)
}

type Transformer interface {
	Transform(context.Context, TransformRequest) (TransformResult, error)
}

type Encoder interface {
	ID() FormatID
	Encode(context.Context, TransformResult, EncodeRequest) (Artifact, error)
}

type EncoderResolver interface {
	Resolve(FormatID) (Encoder, bool)
}

type Request struct {
	Fetch  FetchRequest
	Format FormatID
	Encode EncodeRequest
}

type Service struct {
	Fetcher     Fetcher
	Transformer Transformer
	Encoders    EncoderResolver
}

func (s Service) Convert(ctx context.Context, req Request) (Artifact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.Fetcher == nil || s.Transformer == nil || s.Encoders == nil {
		return Artifact{}, fmt.Errorf("gateway: incomplete service wiring")
	}
	if req.Fetch.URL == "" {
		return Artifact{}, fmt.Errorf("gateway: empty origin URL")
	}
	encoder, ok := s.Encoders.Resolve(req.Format)
	if !ok {
		return Artifact{}, fmt.Errorf("gateway: unsupported format %q", req.Format)
	}
	source, err := s.Fetcher.Fetch(ctx, req.Fetch)
	if err != nil {
		return Artifact{}, fmt.Errorf("gateway: fetch: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	transformed, err := s.Transformer.Transform(ctx, TransformRequest{
		Source:  source,
		Profile: req.Encode.Profile,
		Headers: req.Fetch.Headers,
		Session: req.Fetch.Session,
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("gateway: transform: %w", err)
	}
	if transformed.Document == nil {
		return Artifact{}, fmt.Errorf("gateway: transformer returned nil document")
	}
	artifact, err := encoder.Encode(ctx, transformed, req.Encode)
	if err != nil {
		return Artifact{}, fmt.Errorf("gateway: encode %q: %w", req.Format, err)
	}
	artifact.Format = encoder.ID()
	if artifact.Metadata.Session == "" {
		artifact.Metadata.Session = transformed.Metadata.Session
	}
	return artifact, nil
}
