// Package htmlmini adapts the existing HTML/CSS simplifier to the neutral
// gateway Transformer port. The implementation is a migration seam: it can be
// moved out of oms incrementally without changing protocol encoders.
package htmlmini

import (
	"context"
	"fmt"
	"net/http"

	"operetta/gateway"
	"operetta/oms"
)

type Transformer struct{}

func (Transformer) Transform(ctx context.Context, req gateway.TransformRequest) (gateway.TransformResult, error) {
	if err := ctx.Err(); err != nil {
		return gateway.TransformResult{}, err
	}
	if req.Source == nil {
		return gateway.TransformResult{}, fmt.Errorf("html transform: nil source")
	}
	maxInlineKB := 96
	if req.Profile.MaxInlineBytes > 0 {
		maxInlineKB = (req.Profile.MaxInlineBytes + 1023) / 1024
	}
	imageMIME := req.Profile.PreferredImage
	if imageMIME == "" {
		imageMIME = "image/jpeg"
	}
	opts := &oms.RenderOptions{
		ImagesOn:       req.Profile.Images,
		HighQuality:    req.Profile.HighQuality,
		ImageMIME:      imageMIME,
		MaxInlineKB:    maxInlineKB,
		ScreenW:        req.Profile.ViewportWidth,
		ScreenH:        req.Profile.ViewportHeight,
		NumColors:      req.Profile.Colors,
		AlphaLevels:    req.Profile.AlphaLevels,
		HeapBytes:      req.Profile.MemoryBytes,
		CachePartition: req.Session,
	}
	headers := make(http.Header, len(req.Headers))
	for name, values := range req.Headers {
		headers[name] = append([]string(nil), values...)
	}
	model, err := oms.TransformDocument(req.Source, headers, opts)
	if err != nil {
		return gateway.TransformResult{}, err
	}
	meta := gateway.Metadata{
		SetCookies:          append([]string(nil), model.SetCookies...),
		FormHidden:          cloneHidden(model.FormHidden),
		OriginTransferBytes: model.Metrics.OriginTransferBytes,
		OriginDecodedBytes:  model.Metrics.OriginDecodedBytes,
		Session:             req.Session,
		CacheScope:          gateway.CachePublic,
	}
	if model.NoCache {
		meta.CacheScope = gateway.CacheDisabled
	} else if req.Session != "" || len(model.SetCookies) > 0 || len(model.FormHidden) > 0 {
		meta.CacheScope = gateway.CacheSession
	}
	return gateway.TransformResult{Document: model, Metadata: meta}, nil
}

func cloneHidden(src map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(src))
	for action, fields := range src {
		copyFields := make(map[string]string, len(fields))
		for name, value := range fields {
			copyFields[name] = value
		}
		out[action] = copyFields
	}
	return out
}
