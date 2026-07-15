package operamini

import (
	"context"
	"fmt"

	"operetta/gateway"
	"operetta/oms"
)

const Format gateway.FormatID = "opera-mini-oms"

type Encoder struct {
	DefaultVersion string
}

func (Encoder) ID() gateway.FormatID { return Format }

func (e Encoder) Encode(ctx context.Context, result gateway.TransformResult, req gateway.EncodeRequest) (gateway.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return gateway.Artifact{}, err
	}
	if result.Document == nil {
		return gateway.Artifact{}, fmt.Errorf("opera mini: nil presentation document")
	}
	versionText := req.Dialect
	if versionText == "" {
		versionText = e.DefaultVersion
	}
	if versionText == "" {
		versionText = "2.06"
	}
	dialect, err := Negotiate(versionText)
	if err != nil {
		return gateway.Artifact{}, err
	}
	maxInlineKB := 0
	if req.Profile.MaxInlineBytes > 0 {
		maxInlineKB = (req.Profile.MaxInlineBytes + 1023) / 1024
	}
	options := &oms.RenderOptions{
		ImagesOn:       req.Profile.Images,
		HighQuality:    req.Profile.HighQuality,
		ImageMIME:      req.Profile.PreferredImage,
		MaxInlineKB:    maxInlineKB,
		Compression:    oms.CompressionFromParam(req.Compression),
		ClientVersion:  dialect.Family,
		ScreenW:        req.Profile.ViewportWidth,
		ScreenH:        req.Profile.ViewportHeight,
		NumColors:      req.Profile.Colors,
		AlphaLevels:    req.Profile.AlphaLevels,
		HeapBytes:      req.Profile.MemoryBytes,
		Page:           req.Page,
		MaxTagsPerPage: req.PageUnits,
		ServerBase:     req.ServerBase,
	}
	if options.ImageMIME == "" {
		options.ImageMIME = "image/jpeg"
	}
	if req.Parameters != nil {
		options.AuthCode = req.Parameters["auth_code"]
		options.AuthPrefix = req.Parameters["auth_prefix"]
	}
	page, err := oms.EncodeDocument(result.Document, options)
	if err != nil {
		return gateway.Artifact{}, err
	}
	current, total := page.PartInfo()
	return gateway.Artifact{
		Format:      Format,
		MediaType:   "application/octet-stream",
		Data:        append([]byte(nil), page.Data...),
		Part:        current,
		Parts:       total,
		CachePacked: append([]byte(nil), page.CachePacked...),
		Metadata:    result.Metadata,
	}, nil
}
