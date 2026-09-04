package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"operetta/oms"
	"operetta/protocol/operamini4"
)

const defaultOM4OfficialURL = "http://server4.operamini.com/"

var om4SessionHeaderPattern = regexp.MustCompile(`[0-9a-f]{64}`)

// handlePlainOM4 is the deliberately small transport used by Sky Operetta.
// The handset receives genuine OM4 MSS/application bytes, but expensive RSA,
// RC4, HMAC and raw-DEFLATE work stays on the gateway. This also gives one
// bounded client path for native Operetta pages and the explicitly selected
// official Opera Mini service.
func (s *Server) handlePlainOM4(w http.ResponseWriter, r *http.Request, params map[string]string) {
	rawTarget := strings.TrimSpace(params["u"])
	target, _, _ := normalizeObmlURLWithPart(rawTarget)
	if target == "" {
		http.Error(w, "OM4 target URL is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var frames []operamini4.Frame
	var err error
	backend := strings.ToLower(strings.TrimSpace(params["backend"]))
	if backend == "official" {
		frames, err = s.plainOfficialOM4Frames(ctx, target)
	} else {
		request := plainOM4NavigationRequest(target)
		jar := s.cookieJars.Get("OM4PLAIN|" + DeriveClientKey(r))
		frames, err = s.nativeOM4Frames(ctx, request, jar)
		if err == nil {
			frames, err = normalizeOM4BridgeFrames(frames)
		}
	}
	if err != nil {
		s.logger.Printf("OM4 plaintext %s error for %q: %v", backend, target, err)
		http.Error(w, "OM4 bridge failed", http.StatusBadGateway)
		return
	}
	payload, err := encodePlainOM4Response(frames)
	if err != nil {
		s.logger.Printf("OM4 plaintext encode error for %q: %v", target, err)
		http.Error(w, "OM4 response encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-operetta-om4")
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	w.Header().Set("X-Operetta-OM4-Backend", map[bool]string{true: "official", false: "native"}[backend == "official"])
	_, _ = w.Write(payload)
}

func plainOM4NavigationRequest(target string) *operamini4.SessionRequest {
	encoded := []byte(target)
	if len(encoded) > 255 {
		encoded = encoded[:255]
	}
	payload := make([]byte, 1+len(encoded))
	payload[0] = byte(len(encoded))
	copy(payload[1:], encoded)
	return &operamini4.SessionRequest{Frames: []operamini4.Frame{{Type: 8, Channel: 1, Payload: payload}}}
}

// encodePlainOM4Response normalizes both native and official compressed
// documents to one uncompressed type-0x0a MSS frame understood by the bounded
// C89 decoder. The three-byte document id remains before the application
// envelope, exactly as in the original OM4 stream.
func encodePlainOM4Response(frames []operamini4.Frame) ([]byte, error) {
	document, err := operamini4.DecodeApplicationDocument(frames)
	if err != nil {
		return nil, err
	}
	application := make([]byte, 3+len(document.Inflated))
	application[0] = byte(document.ID >> 16)
	application[1] = byte(document.ID >> 8)
	application[2] = byte(document.ID)
	copy(application[3:], document.Inflated)
	result := make([]byte, 0, len(application)+4*((len(application)+65532)/65533))
	for len(application) > 0 {
		chunk := len(application)
		if chunk > 65533 {
			chunk = 65533
		}
		header := make([]byte, 4)
		binary.BigEndian.PutUint16(header[:2], uint16(chunk+2))
		header[2] = 0x0a
		header[3] = 1
		result = append(result, header...)
		result = append(result, application[:chunk]...)
		application = application[chunk:]
	}
	return result, nil
}

func (s *Server) plainOfficialOM4Frames(ctx context.Context, target string) ([]operamini4.Frame, error) {
	template, err := loadPlainOM4StartupRequest()
	if err != nil {
		return nil, err
	}
	reference := s.om4Reference
	owned := false
	if reference == nil {
		endpoint := strings.TrimSpace(os.Getenv("OMS_OM4_OFFICIAL_URL"))
		if endpoint == "" {
			endpoint = defaultOM4OfficialURL
		}
		reference, err = operamini4.NewReferenceClient(endpoint)
		if err != nil {
			return nil, err
		}
		owned = true
	}
	if owned {
		defer reference.CloseIdleConnections()
	}

	bootstrap := cloneOM4Request(template)
	bootstrap.Header = ""
	firstFrames, err := reference.Exchange(ctx, bootstrap)
	if err != nil {
		return nil, fmt.Errorf("official onboarding: %w", err)
	}
	firstDocument, err := operamini4.DecodeApplicationDocument(firstFrames)
	if err != nil {
		return nil, fmt.Errorf("decode official onboarding: %w", err)
	}
	header := om4SessionHeaderPattern.Find(firstDocument.Inflated)
	if len(header) != 64 {
		return nil, errors.New("official onboarding returned no application session header")
	}
	navigation := cloneOM4Request(template)
	navigation.Header = string(header)
	if !replacePlainOM4Target(navigation, target) {
		return nil, errors.New("OM4 startup template has no replaceable URL")
	}
	frames, err := reference.Exchange(ctx, navigation)
	if err != nil {
		return nil, fmt.Errorf("official navigation: %w", err)
	}
	return normalizeOM4BridgeFrames(frames)
}

// normalizeOM4BridgeFrames translates the broader historical drawing
// dialect to the stable B/T/I/S/L subset shared by the native encoder and the
// small C89 client. Geometry, text, colors, links and bounded image resources
// are preserved; transport/session-only records stay on the gateway.
func normalizeOM4BridgeFrames(frames []operamini4.Frame) ([]operamini4.Frame, error) {
	document, err := operamini4.DecodeApplicationDocument(frames)
	if err != nil {
		return nil, err
	}
	lines := make([]operamini4.WelcomeLine, 0, len(document.Drawings))
	background := uint32(0xffffffff)
	imageBudget := 64 << 10
	for _, drawing := range document.Drawings {
		line := operamini4.WelcomeLine{
			X: drawing.X, Y: drawing.Y, Width: drawing.Width,
			Height: drawing.Height, Positioned: true, Absolute: true,
			Color: drawing.Color,
		}
		switch drawing.Kind {
		case 'B':
			line.Background = drawing.Color
			line.BackgroundOnly = true
			if drawing.X == 0 && drawing.Y == 0 &&
				drawing.Width >= document.Header.ViewportWidth {
				background = drawing.Color
			}
		case 'T':
			line.Text = drawing.Text
			line.Font = drawing.Font
			line.URL = officialDrawingURL(drawing, document.Links,
				document.Header.URL)
		case 'I':
			resource := officialImageResource(document.Page, drawing.ImagePointer)
			if len(resource) != 0 && imageBudget > 0 {
				limit := 16 << 10
				if limit > imageBudget {
					limit = imageBudget
				}
				encoded, width, height, encodeErr := oms.EncodeRGB565AlphaResource(
					resource, drawing.Width, drawing.Height, limit)
				if encodeErr == nil {
					line.Image = encoded
					line.Width, line.Height = width, height
					imageBudget -= len(encoded)
				}
			}
			line.URL = officialDrawingURL(drawing, document.Links,
				document.Header.URL)
			if len(line.Image) == 0 {
				line.Background = drawing.Color
				line.BackgroundOnly = true
			}
		default:
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, operamini4.WelcomeLine{
			Text:  "Официальный сервер вернул страницу без поддерживаемых команд.",
			Color: 0xff000000, Height: 42,
		})
	}
	page, err := operamini4.BuildWelcomePage(operamini4.WelcomePage{
		Title: document.Header.Title, Base: document.Header.Base,
		URL: document.Header.URL, DocumentHeight: document.Header.DocumentHeight,
		HideAccent: true, Background: background, Lines: lines,
	})
	if err != nil {
		return nil, err
	}
	return operamini4.EncodeApplicationDocument(document.ID, nil, page)
}

func officialDrawingURL(drawing operamini4.DrawingElement, links []operamini4.LinkElement, base string) string {
	for _, link := range links {
		if drawing.X < link.X+link.Width && drawing.X+drawing.Width > link.X &&
			drawing.Y < link.Y+link.Height && drawing.Y+drawing.Height > link.Y {
			return resolvePlainOM4URL(base, link.URL)
		}
	}
	return ""
}

func resolvePlainOM4URL(base, target string) string {
	target = strings.TrimLeft(target, "\x00")
	if len(target) > 2 && (strings.HasPrefix(target, "0/") ||
		strings.HasPrefix(target, "1/")) {
		target = target[2:]
	}
	reference, err := url.Parse(target)
	if err != nil {
		return target
	}
	if reference.IsAbs() {
		return reference.String()
	}
	parsedBase, err := url.Parse(base)
	if err != nil {
		return target
	}
	return parsedBase.ResolveReference(reference).String()
}

func officialImageResource(page []byte, pointer int) []byte {
	if pointer <= 0 || pointer+2 > len(page) {
		return nil
	}
	length := int(binary.BigEndian.Uint16(page[pointer : pointer+2]))
	if length <= 0 || pointer+2+length > len(page) {
		return nil
	}
	return append([]byte(nil), page[pointer+2:pointer+2+length]...)
}

func loadPlainOM4StartupRequest() (*operamini4.SessionRequest, error) {
	path := strings.TrimSpace(os.Getenv("OMS_OM4_STARTUP_REQUEST"))
	if path != "" {
		if strings.HasSuffix(strings.ToLower(path), ".frames.bin") {
			frames, err := operamini4.ReadFrameFile(path)
			if err != nil {
				return nil, fmt.Errorf("read OM4 startup frames %q: %w", path, err)
			}
			return &operamini4.SessionRequest{Frames: frames}, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read OM4 startup template %q: %w", path, err)
		}
		request, err := operamini4.ParseSessionRequest(data)
		if err != nil {
			return nil, fmt.Errorf("parse OM4 startup template %q: %w", path, err)
		}
		return request, nil
	}
	return operamini4.DefaultStartupRequest()
}

func cloneOM4Request(source *operamini4.SessionRequest) *operamini4.SessionRequest {
	result := &operamini4.SessionRequest{Header: source.Header, Frames: make([]operamini4.Frame, len(source.Frames))}
	for index, frame := range source.Frames {
		result.Frames[index] = operamini4.Frame{Type: frame.Type, Channel: frame.Channel, Payload: append([]byte(nil), frame.Payload...)}
	}
	return result
}

func replacePlainOM4Target(request *operamini4.SessionRequest, target string) bool {
	replaced := false
	for index := range request.Frames {
		frame := &request.Frames[index]
		if frame.Channel != 1 || frame.Type != 8 {
			continue
		}
		start := bytes.LastIndex(frame.Payload, []byte("http"))
		if start <= 0 {
			continue
		}
		oldLength := int(frame.Payload[start-1])
		if oldLength < 7 || start+oldLength > len(frame.Payload) {
			continue
		}
		encoded := []byte(target)
		if len(encoded) > 255 {
			encoded = encoded[:255]
		}
		payload := make([]byte, 0, len(frame.Payload)-oldLength+len(encoded))
		payload = append(payload, frame.Payload[:start-1]...)
		payload = append(payload, byte(len(encoded)))
		payload = append(payload, encoded...)
		payload = append(payload, frame.Payload[start+oldLength:]...)
		if len(payload) >= 4 {
			binary.BigEndian.PutUint32(payload[:4], uint32(len(payload)-4))
		}
		frame.Payload = payload
		replaced = true
	}
	return replaced
}
