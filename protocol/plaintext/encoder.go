// Package plaintext is a deliberately small alternative encoder. Besides being
// useful on constrained transports, it is a conformance check that presentation
// transformation does not depend on Opera Mini framing.
package plaintext

import (
	"context"
	"fmt"
	"strings"

	"operetta/gateway"
	"operetta/presentation"
)

const Format gateway.FormatID = "plain-text"

type Encoder struct{}

func (Encoder) ID() gateway.FormatID { return Format }

func (Encoder) Encode(ctx context.Context, result gateway.TransformResult, _ gateway.EncodeRequest) (gateway.Artifact, error) {
	if result.Document == nil {
		return gateway.Artifact{}, fmt.Errorf("plain text: nil document")
	}
	var out strings.Builder
	var links []string
	for _, op := range result.Document.Operations {
		if err := ctx.Err(); err != nil {
			return gateway.Artifact{}, err
		}
		switch op.Kind {
		case presentation.Text:
			out.WriteString(op.Text)
		case presentation.Break, presentation.Paragraph, presentation.BlockSeparator:
			writeNewline(&out)
		case presentation.LinkStart:
			links = append(links, op.URL)
		case presentation.LinkEnd:
			if len(links) > 0 {
				url := links[len(links)-1]
				links = links[:len(links)-1]
				if url != "" {
					out.WriteString(" <")
					out.WriteString(url)
					out.WriteByte('>')
				}
			}
		case presentation.ImagePlaceholder, presentation.ImageInline:
			out.WriteString("[image]")
		case presentation.TextInput, presentation.PasswordInput:
			out.WriteString("[")
			out.WriteString(op.Name)
			out.WriteString("]")
		case presentation.Submit, presentation.Button, presentation.Reset:
			out.WriteString("[")
			out.WriteString(op.Value)
			out.WriteString("]")
		}
	}
	return gateway.Artifact{
		Format:    Format,
		MediaType: "text/plain; charset=utf-8",
		Data:      []byte(strings.TrimSpace(out.String())),
		Part:      1,
		Parts:     1,
		Metadata:  result.Metadata,
	}, nil
}

func writeNewline(out *strings.Builder) {
	if out.Len() == 0 {
		return
	}
	s := out.String()
	if s[len(s)-1] != '\n' {
		out.WriteByte('\n')
	}
}
