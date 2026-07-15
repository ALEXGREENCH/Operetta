package protocol

import (
	"context"

	"operetta/gateway"
)

type ID string

// Envelope is the transport-facing packet seen by a client protocol adapter.
// HTTP headers, mesh routing fields or another carrier's metadata stay opaque
// to the gateway use case.
type Envelope struct {
	Transport string
	Payload   []byte
	Metadata  map[string][]string
}

type Match struct {
	Matched    bool
	Confidence int
}

// Adapter owns a complete client protocol: ingress parsing, dialect
// negotiation and response wrapping. A format-only change can implement just
// gateway.Encoder and use Registry; UC Web/Bolt-style handshakes implement this
// larger contract.
type Adapter interface {
	ID() ID
	Probe(Envelope) Match
	Decode(context.Context, Envelope) (gateway.Request, error)
	Wrap(context.Context, gateway.Artifact, gateway.Request) (Envelope, error)
}

// AdapterRegistry selects an ingress protocol without coupling it to content
// transformation. Registration is immutable after construction.
type AdapterRegistry struct {
	adapters []Adapter
}

func NewAdapterRegistry(adapters ...Adapter) *AdapterRegistry {
	return &AdapterRegistry{adapters: append([]Adapter(nil), adapters...)}
}

func (r *AdapterRegistry) Match(envelope Envelope) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	var selected Adapter
	best := -1
	for _, adapter := range r.adapters {
		if adapter == nil {
			continue
		}
		match := adapter.Probe(envelope)
		if match.Matched && match.Confidence > best {
			selected = adapter
			best = match.Confidence
		}
	}
	return selected, selected != nil
}
