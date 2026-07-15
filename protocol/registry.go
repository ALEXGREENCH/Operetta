// Package protocol provides composition-time registration of output formats.
package protocol

import (
	"fmt"
	"sort"

	"operetta/gateway"
)

// Registry is immutable after construction and safe for concurrent lookups.
type Registry struct {
	encoders map[gateway.FormatID]gateway.Encoder
}

func NewRegistry(encoders ...gateway.Encoder) (*Registry, error) {
	r := &Registry{encoders: make(map[gateway.FormatID]gateway.Encoder, len(encoders))}
	for _, encoder := range encoders {
		if encoder == nil {
			return nil, fmt.Errorf("protocol registry: nil encoder")
		}
		id := encoder.ID()
		if id == "" {
			return nil, fmt.Errorf("protocol registry: encoder with empty id")
		}
		if _, exists := r.encoders[id]; exists {
			return nil, fmt.Errorf("protocol registry: duplicate encoder %q", id)
		}
		r.encoders[id] = encoder
	}
	return r, nil
}

func MustRegistry(encoders ...gateway.Encoder) *Registry {
	r, err := NewRegistry(encoders...)
	if err != nil {
		panic(err)
	}
	return r
}

func (r *Registry) Resolve(id gateway.FormatID) (gateway.Encoder, bool) {
	if r == nil {
		return nil, false
	}
	encoder, ok := r.encoders[id]
	return encoder, ok
}

func (r *Registry) Formats() []gateway.FormatID {
	if r == nil {
		return nil
	}
	ids := make([]gateway.FormatID, 0, len(r.encoders))
	for id := range r.encoders {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
