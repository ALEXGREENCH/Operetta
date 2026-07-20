// Command om4compare collects anonymous reference documents and compares their
// semantic text with Operetta's protocol-neutral HTML transformation.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"operetta/oms"
	"operetta/presentation"
	"operetta/protocol/operamini4"
)

type comparison struct {
	RequestedURL        string   `json:"requested_url"`
	ReferenceDurationMS int64    `json:"reference_duration_ms"`
	NativeDurationMS    int64    `json:"native_duration_ms"`
	ReferenceURL        string   `json:"reference_url,omitempty"`
	ReferenceTitle      string   `json:"reference_title,omitempty"`
	ReferenceTexts      []string `json:"reference_texts,omitempty"`
	ReferenceTextCount  int      `json:"reference_text_count"`
	ReferenceRecords    int      `json:"reference_records"`
	ReferenceBytes      int      `json:"reference_page_bytes"`
	ReferenceHeight     int      `json:"reference_height"`
	ReferenceLinks      int      `json:"reference_links"`
	ReferenceStyles     []string `json:"reference_styles,omitempty"`
	ReferenceImages     int      `json:"reference_native_images"`
	ReferenceBlocks     int      `json:"reference_native_blocks"`
	OperettaURL         string   `json:"operetta_url,omitempty"`
	OperettaTexts       []string `json:"operetta_texts,omitempty"`
	OperettaTextCount   int      `json:"operetta_text_count"`
	OperettaLinks       int      `json:"operetta_links"`
	OperettaImages      int      `json:"operetta_images"`
	OperettaForms       int      `json:"operetta_forms"`
	OperettaStyles      []string `json:"operetta_styles,omitempty"`
	NativeURL           string   `json:"native_url,omitempty"`
	NativeTitle         string   `json:"native_title,omitempty"`
	NativeTexts         []string `json:"native_texts,omitempty"`
	NativeTextCount     int      `json:"native_text_count"`
	NativeLinks         int      `json:"native_links"`
	NativeRecords       int      `json:"native_records"`
	NativeBytes         int      `json:"native_page_bytes"`
	NativeHeight        int      `json:"native_height"`
	NativeStyles        []string `json:"native_styles,omitempty"`
	NativeImages        int      `json:"native_images"`
	NativeBlocks        int      `json:"native_blocks"`
	TokenCoverage       float64  `json:"reference_token_coverage"`
	NativeCoverage      float64  `json:"reference_native_token_coverage"`
	StyleCoverage       float64  `json:"reference_native_style_coverage"`
	ReferenceError      string   `json:"reference_error,omitempty"`
	OperettaError       string   `json:"operetta_error,omitempty"`
	NativeError         string   `json:"native_error,omitempty"`
	NextLinkID          string   `json:"-"`
	NextRequestToken    []byte   `json:"-"`
	NextNativeLinkID    string   `json:"-"`
	NextNativeToken     []byte   `json:"-"`
}

type exchangeFunc func(context.Context, *operamini4.SessionRequest) ([]operamini4.Frame, error)

type exchangeResult struct {
	frames   []operamini4.Frame
	err      error
	duration time.Duration
}

var safeNamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func main() {
	manifest := flag.String("sites", "config/om4-sites.txt", "newline-delimited URL manifest")
	only := flag.String("only", "", "comma-separated URLs instead of the manifest")
	outDir := flag.String("out", "build/om4-compare", "comparison output directory")
	endpoint := flag.String("reference", "http://server4.operamini.com/", "compatible OM4 reference endpoint")
	localEndpoint := flag.String("operetta", "http://127.0.0.1:8081/", "local Operetta OM4 endpoint")
	bootstrapRequest := flag.String("bootstrap-request", "build/latest-om4-request.bin", "clean captured first-time OM4 wire request")
	startupRequest := flag.String("startup-request", "", "historical decrypted startup request.bin; auto-detected when empty")
	navigationTemplate := flag.String("navigation-request", "", "decrypted navigation request.frames.bin; auto-detected when empty")
	limit := flag.Int("limit", 0, "maximum number of sites; zero means all")
	timeout := flag.Duration("timeout", 75*time.Second, "per-side request timeout")
	upstreamUA := flag.String("upstream-ua", operamini4.OriginUserAgent, "User-Agent used by Operetta's origin fetch")
	flag.Parse()
	var urls []string
	var err error
	if strings.TrimSpace(*only) != "" {
		for _, item := range strings.Split(*only, ",") {
			if item = strings.TrimSpace(item); item != "" {
				urls = append(urls, item)
			}
		}
	} else {
		urls, err = readManifest(*manifest)
		check(err)
	}
	if *limit > 0 && len(urls) > *limit {
		urls = urls[:*limit]
	}
	check(os.MkdirAll(*outDir, 0o700))
	reference, err := operamini4.NewReferenceClient(*endpoint)
	check(err)
	defer reference.CloseIdleConnections()
	local, err := operamini4.NewReferenceClient(*localEndpoint)
	check(err)
	defer local.CloseIdleConnections()
	header, err := initializeReference(reference, *bootstrapRequest, *timeout)
	check(err)
	localHeader, err := initializeReference(local, *bootstrapRequest, *timeout)
	check(err)
	startup, err := loadStartupRequest(*startupRequest, "build/om4-corpus")
	check(err)
	navigation, err := loadNavigationTemplate(*navigationTemplate, "build/om4-corpus")
	check(err)
	fmt.Printf("sessions initialized: reference=%s… local=%s…\n", header[:8], localHeader[:8])
	report := make([]comparison, 0, len(urls))
	linkID := "h22-02-04"
	var requestToken []byte
	localLinkID := "h22-02-04"
	var localRequestToken []byte
	for index, target := range urls {
		fmt.Printf("[%d/%d] %s\n", index+1, len(urls), target)
		var request *operamini4.SessionRequest
		var localRequest *operamini4.SessionRequest
		if index == 0 {
			request, err = startupNavigationRequest(startup, header, target)
			check(err)
			localRequest, err = startupNavigationRequest(startup, localHeader, target)
			check(err)
		} else {
			request, err = navigationRequestFromTemplate(navigation, header, linkID, requestToken, target)
			check(err)
			localRequest, err = navigationRequestFromTemplate(navigation, localHeader, localLinkID, localRequestToken, target)
			check(err)
		}
		item := compare(target, request, reference, localRequest, local, *outDir, *timeout, *upstreamUA)
		if item.NextLinkID != "" {
			linkID = item.NextLinkID
		}
		if len(item.NextRequestToken) == 8 {
			requestToken = append(requestToken[:0], item.NextRequestToken...)
		}
		if item.NextNativeLinkID != "" {
			localLinkID = item.NextNativeLinkID
		}
		if len(item.NextNativeToken) == 8 {
			localRequestToken = append(localRequestToken[:0], item.NextNativeToken...)
		}
		report = append(report, item)
		fmt.Printf("  reference=%d model=%d native=%d links=%d/%d coverage=%.1f%% native=%.1f%% ref_error=%q native_error=%q own_error=%q\n",
			item.ReferenceTextCount, item.OperettaTextCount, item.NativeTextCount, item.ReferenceLinks, item.NativeLinks,
			item.TokenCoverage*100, item.NativeCoverage*100, item.ReferenceError, item.NativeError, item.OperettaError)
		data, marshalErr := json.MarshalIndent(item, "", "  ")
		check(marshalErr)
		check(os.WriteFile(filepath.Join(*outDir, fmt.Sprintf("%02d-%s.json", index+1, safeName(target))), append(data, '\n'), 0o600))
	}
	data, err := json.MarshalIndent(report, "", "  ")
	check(err)
	check(os.WriteFile(filepath.Join(*outDir, "report.json"), append(data, '\n'), 0o600))
}

func compare(target string, request *operamini4.SessionRequest, reference *operamini4.ReferenceClient, localRequest *operamini4.SessionRequest, local *operamini4.ReferenceClient, outDir string, timeout time.Duration, upstreamUA string) comparison {
	result := comparison{RequestedURL: target}
	referenceExchange, nativeExchange := exchangePair(
		reference.Exchange, request,
		local.Exchange, localRequest,
		timeout,
	)
	result.ReferenceDurationMS = referenceExchange.duration.Milliseconds()
	result.NativeDurationMS = nativeExchange.duration.Milliseconds()
	frames, err := referenceExchange.frames, referenceExchange.err
	if err != nil {
		result.ReferenceError = err.Error()
	} else {
		for _, frame := range frames {
			if frame.Channel == 0 && frame.Type == 15 && len(frame.Payload) == 8 {
				result.NextRequestToken = append([]byte(nil), frame.Payload...)
				break
			}
		}
		name := safeName(target) + ".reference.frames.bin"
		if writeErr := operamini4.WriteFrameFile(filepath.Join(outDir, name), frames); writeErr != nil {
			result.ReferenceError = writeErr.Error()
		} else if document, decodeErr := operamini4.DecodeApplicationDocument(frames); decodeErr != nil {
			result.ReferenceError = decodeErr.Error()
		} else {
			result.ReferenceURL = document.Header.URL
			result.ReferenceTitle = document.Header.Title
			result.ReferenceRecords = len(document.Records)
			result.ReferenceBytes = len(document.Page)
			result.ReferenceHeight = document.Header.DocumentHeight
			result.ReferenceLinks = len(document.Links)
			result.ReferenceStyles = uniqueStyles(document.Texts)
			result.ReferenceImages, result.ReferenceBlocks = drawingCounts(document.Drawings)
			result.ReferenceTexts = uniqueTexts(document.Texts)
			result.ReferenceTextCount = len(result.ReferenceTexts)
			if match := regexp.MustCompile(`h[0-9]{2}-[0-9]{2}-[0-9]{2}`).Find(document.Page); len(match) > 0 {
				result.NextLinkID = string(match)
			}
			sceneName := safeName(target) + ".reference.scene.json"
			writeComparisonScene(filepath.Join(outDir, sceneName), "reference", document, &result.ReferenceError)
		}
	}

	localFrames, localErr := nativeExchange.frames, nativeExchange.err
	if localErr != nil {
		result.NativeError = localErr.Error()
	} else {
		for _, frame := range localFrames {
			if frame.Channel == 0 && frame.Type == 15 && len(frame.Payload) == 8 {
				result.NextNativeToken = append([]byte(nil), frame.Payload...)
				break
			}
		}
		name := safeName(target) + ".operetta.frames.bin"
		if writeErr := operamini4.WriteFrameFile(filepath.Join(outDir, name), localFrames); writeErr != nil {
			result.NativeError = writeErr.Error()
		} else if document, decodeErr := operamini4.DecodeApplicationDocument(localFrames); decodeErr != nil {
			result.NativeError = decodeErr.Error()
		} else {
			result.NativeURL = document.Header.URL
			result.NativeTitle = document.Header.Title
			result.NativeRecords = len(document.Records)
			result.NativeBytes = len(document.Page)
			result.NativeHeight = document.Header.DocumentHeight
			result.NativeTexts = uniqueTexts(document.Texts)
			result.NativeTextCount = len(result.NativeTexts)
			result.NativeLinks = len(document.Links)
			result.NativeStyles = uniqueStyles(document.Texts)
			result.NativeImages, result.NativeBlocks = drawingCounts(document.Drawings)
			if match := regexp.MustCompile(`h[0-9]{2}-[0-9]{2}-[0-9]{2}`).Find(document.Page); len(match) > 0 {
				result.NextNativeLinkID = string(match)
			}
			sceneName := safeName(target) + ".operetta.scene.json"
			writeComparisonScene(filepath.Join(outDir, sceneName), "operetta", document, &result.NativeError)
		}
	}

	ownCtx, cancelOwn := context.WithTimeout(context.Background(), timeout)
	options := &oms.RenderOptions{
		ImagesOn: false,
		ScreenW:  240,
		ScreenH:  320,
		ReqHeaders: http.Header{
			"Accept-Language": []string{"ru,en;q=0.8"},
			"User-Agent":      []string{upstreamUA},
		},
	}
	document, err := oms.FetchDocumentWithHeadersAndOptionsCtx(ownCtx, target, options.ReqHeaders, options)
	if err == nil {
		var model *presentation.Document
		model, err = oms.TransformDocument(document, options.ReqHeaders, options)
		if err == nil {
			result.OperettaURL = model.URL
			result.OperettaTexts, result.OperettaLinks, result.OperettaImages, result.OperettaForms = summarizeModel(model)
			result.OperettaStyles = summarizePresentationStyles(model)
			result.OperettaTextCount = len(result.OperettaTexts)
		}
	}
	cancelOwn()
	if err != nil {
		result.OperettaError = err.Error()
	}
	result.TokenCoverage = tokenCoverage(result.ReferenceTexts, result.OperettaTexts)
	result.NativeCoverage = tokenCoverage(result.ReferenceTexts, result.NativeTexts)
	result.StyleCoverage = stringCoverage(result.ReferenceStyles, result.NativeStyles)
	return result
}

// exchangePair starts both sides of one comparison case together. Each side
// owns its timeout context and reports through a buffered channel, so the
// goroutines never mutate shared comparison state and are both joined before
// decoding or session-token chaining continues.
func exchangePair(reference exchangeFunc, referenceRequest *operamini4.SessionRequest, native exchangeFunc, nativeRequest *operamini4.SessionRequest, timeout time.Duration) (exchangeResult, exchangeResult) {
	start := make(chan struct{})
	referenceResult := make(chan exchangeResult, 1)
	nativeResult := make(chan exchangeResult, 1)

	go func() {
		<-start
		referenceResult <- exchangeWithTimeout(reference, referenceRequest, timeout)
	}()
	go func() {
		<-start
		nativeResult <- exchangeWithTimeout(native, nativeRequest, timeout)
	}()
	close(start)

	return <-referenceResult, <-nativeResult
}

func exchangeWithTimeout(exchange exchangeFunc, request *operamini4.SessionRequest, timeout time.Duration) exchangeResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	frames, err := exchange(ctx, request)
	return exchangeResult{frames: frames, err: err, duration: time.Since(started)}
}

func writeSceneFile(path string, document *operamini4.ApplicationDocument) error {
	data, err := operamini4.MarshalScene(document)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func writeComparisonScene(path, side string, document *operamini4.ApplicationDocument, errorField *string) {
	if err := writeSceneFile(path, document); err != nil {
		*errorField = fmt.Sprintf("write %s scene: %v", side, err)
	}
}

func loadStartupRequest(path, corpusDir string) (*operamini4.SessionRequest, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return operamini4.ParseDecryptedSessionRequest(data)
	}
	candidates, err := filepath.Glob(filepath.Join(corpusDir, "*.request.bin"))
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			continue
		}
		request, parseErr := operamini4.ParseDecryptedSessionRequest(data)
		if parseErr != nil || request.Header == "" {
			continue
		}
		hasStartup, hasClientInfo := false, false
		for _, frame := range request.Frames {
			hasStartup = hasStartup || (frame.Channel == 0 && frame.Type == 16)
			hasClientInfo = hasClientInfo || bytes.Contains(frame.Payload, []byte("Opera Mini/4.2"))
		}
		if hasStartup && hasClientInfo {
			return request, nil
		}
	}
	return nil, errors.New("no historical OM4 startup request found; use -startup-request")
}

func loadNavigationTemplate(path, corpusDir string) (*operamini4.SessionRequest, error) {
	if path != "" {
		frames, err := operamini4.ReadFrameFile(path)
		return &operamini4.SessionRequest{Frames: frames}, err
	}
	candidates, err := filepath.Glob(filepath.Join(corpusDir, "*.request.frames.bin"))
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		frames, readErr := operamini4.ReadFrameFile(candidate)
		if readErr != nil {
			continue
		}
		hasNavigation, hasRequestMarker := false, false
		for _, frame := range frames {
			hasRequestMarker = hasRequestMarker || (frame.Channel == 0 && frame.Type == 15)
			hasNavigation = hasNavigation || (frame.Channel == 1 && frame.Type == 8 && bytes.Contains(frame.Payload, []byte("www.wikipedia.org")))
		}
		if hasNavigation && hasRequestMarker {
			return &operamini4.SessionRequest{Frames: frames}, nil
		}
	}
	return nil, errors.New("no safe OM4 navigation template found; use -navigation-request")
}

func startupNavigationRequest(template *operamini4.SessionRequest, header, target string) (*operamini4.SessionRequest, error) {
	if template == nil {
		return nil, errors.New("nil OM4 startup template")
	}
	request := &operamini4.SessionRequest{Header: header, Frames: make([]operamini4.Frame, len(template.Frames))}
	replaced := false
	for index, frame := range template.Frames {
		request.Frames[index] = operamini4.Frame{Type: frame.Type, Channel: frame.Channel, Payload: append([]byte(nil), frame.Payload...)}
		if frame.Channel != 1 || frame.Type != 8 {
			continue
		}
		payload, ok := replaceLastLengthPrefixedURL(request.Frames[index].Payload, target)
		if ok {
			request.Frames[index].Payload = payload
			replaced = true
		}
	}
	if !replaced {
		return nil, errors.New("startup template contains no replaceable URL")
	}
	return request, nil
}

func replaceLastLengthPrefixedURL(payload []byte, target string) ([]byte, bool) {
	start := bytes.LastIndex(payload, []byte("http"))
	if start <= 0 {
		for index := len(payload) - 2; index >= 4; index-- {
			if payload[index] != 0x35 {
				continue
			}
			length := int(payload[index+1])
			valueStart := index + 2
			if length >= 3 && valueStart+length <= len(payload) && bytes.Contains(payload[valueStart:valueStart+length], []byte(".")) {
				start = valueStart
				break
			}
		}
	}
	if start <= 0 {
		return payload, false
	}
	oldLength := int(payload[start-1])
	if oldLength < 7 || start+oldLength > len(payload) {
		return payload, false
	}
	encoded := []byte(target)
	if len(encoded) > 255 {
		encoded = encoded[:255]
	}
	replaced := make([]byte, 0, len(payload)-oldLength+len(encoded))
	replaced = append(replaced, payload[:start-1]...)
	replaced = append(replaced, byte(len(encoded)))
	replaced = append(replaced, encoded...)
	replaced = append(replaced, payload[start+oldLength:]...)
	if len(replaced) >= 4 {
		binary.BigEndian.PutUint32(replaced[:4], uint32(len(replaced)-4))
	}
	return replaced, true
}

func navigationRequestFromTemplate(template *operamini4.SessionRequest, header, linkID string, requestToken []byte, target string) (*operamini4.SessionRequest, error) {
	if template == nil {
		return nil, errors.New("nil OM4 navigation template")
	}
	request := &operamini4.SessionRequest{Header: header, Frames: make([]operamini4.Frame, len(template.Frames))}
	replaced := false
	for index, frame := range template.Frames {
		request.Frames[index] = operamini4.Frame{Type: frame.Type, Channel: frame.Channel, Payload: append([]byte(nil), frame.Payload...)}
		switch {
		case frame.Channel == 2 && frame.Type == 128:
			request.Frames[index].Payload = []byte(linkID + "\x00linkid\x00")
		case frame.Channel == 0 && frame.Type == 15:
			if len(requestToken) == len(request.Frames[index].Payload) {
				copy(request.Frames[index].Payload, requestToken)
			}
		case frame.Channel == 1 && frame.Type == 8:
			payload, ok := replaceLastLengthPrefixedURL(request.Frames[index].Payload, target)
			if ok {
				request.Frames[index].Payload = payload
				replaced = true
			}
		}
	}
	if !replaced {
		return nil, errors.New("navigation template contains no replaceable URL")
	}
	return request, nil
}

func readManifest(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	items := make([]string, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		item := strings.TrimSpace(scanner.Text())
		if item == "" || strings.HasPrefix(item, "#") {
			continue
		}
		parsed, parseErr := url.Parse(item)
		if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid manifest URL %q", item)
		}
		items = append(items, parsed.String())
	}
	return items, scanner.Err()
}

func initializeReference(reference *operamini4.ReferenceClient, requestPath string, timeout time.Duration) (string, error) {
	raw, err := os.ReadFile(requestPath)
	if err != nil {
		return "", fmt.Errorf("read bootstrap request: %w", err)
	}
	source, err := operamini4.ParseSessionRequest(raw)
	if err != nil {
		return "", fmt.Errorf("parse bootstrap request: %w", err)
	}
	// A reusable batch must always start anonymously, even if the capture file
	// was produced while another application session existed.
	source.Header = ""
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	frames, err := reference.Exchange(ctx, source)
	cancel()
	if err != nil {
		return "", fmt.Errorf("reference first-time exchange: %w", err)
	}
	document, err := operamini4.DecodeApplicationDocument(frames)
	if err != nil {
		return "", fmt.Errorf("decode reference first-time document: %w", err)
	}
	match := regexp.MustCompile(`[0-9a-f]{64}`).Find(document.Inflated)
	if len(match) != 64 {
		return "", fmt.Errorf("reference first-time document contains no session header")
	}
	return string(match), nil
}

func uniqueTexts(elements []operamini4.TextElement) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(elements))
	for _, element := range elements {
		value := normalizeText(element.Text)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStyles(elements []operamini4.TextElement) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, element := range elements {
		style := fmt.Sprintf("%08x/%d", element.Style, element.Font)
		if _, ok := seen[style]; ok {
			continue
		}
		seen[style] = struct{}{}
		result = append(result, style)
	}
	return result
}

func drawingCounts(elements []operamini4.DrawingElement) (images, blocks int) {
	for _, element := range elements {
		switch element.Kind {
		case 'I':
			images++
		case 'B':
			blocks++
		}
	}
	return images, blocks
}

func stringCoverage(reference, own []string) float64 {
	if len(reference) == 0 {
		return 0
	}
	values := make(map[string]struct{}, len(own))
	for _, value := range own {
		values[value] = struct{}{}
	}
	matched := 0
	for _, value := range reference {
		if _, ok := values[value]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(reference))
}

func summarizeModel(model *presentation.Document) ([]string, int, int, int) {
	texts := make([]string, 0)
	seen := make(map[string]struct{})
	links, images, forms := 0, 0, 0
	for _, operation := range model.Operations {
		switch operation.Kind {
		case presentation.Text, presentation.Option:
			value := normalizeText(operation.Text)
			if value != "" {
				if _, ok := seen[value]; !ok {
					seen[value] = struct{}{}
					texts = append(texts, value)
				}
			}
		case presentation.LinkStart:
			links++
		case presentation.ImagePlaceholder, presentation.ImageInline:
			images++
		case presentation.FormStart:
			forms++
		}
	}
	return texts, links, images, forms
}

func summarizePresentationStyles(model *presentation.Document) []string {
	seen := make(map[string]struct{})
	styles := make([]string, 0)
	for _, operation := range model.Operations {
		if operation.Kind != presentation.Style {
			continue
		}
		value := fmt.Sprintf("%s/i%t/b%t/u%t", operation.Style.Foreground, operation.Style.Italic, operation.Style.Bold, operation.Style.Underline)
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		styles = append(styles, value)
	}
	return styles
}

func tokenCoverage(referenceTexts, ownTexts []string) float64 {
	referenceTokens := tokenSet(referenceTexts)
	if len(referenceTokens) == 0 {
		return 0
	}
	ownTokens := tokenSet(ownTexts)
	matched := 0
	for token := range referenceTokens {
		if _, ok := ownTokens[token]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(referenceTokens))
}

func tokenSet(texts []string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, text := range texts {
		for _, token := range strings.Fields(strings.ToLower(text)) {
			token = strings.Trim(token, ".,:;!?()[]{}<>«»\"'|/\\—–-+•")
			if len([]rune(token)) >= 2 {
				result[token] = struct{}{}
			}
		}
	}
	return result
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func safeName(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		raw = parsed.Host + parsed.Path
	}
	raw = strings.Trim(safeNamePattern.ReplaceAllString(raw, "-"), "-.")
	if raw == "" {
		return "site"
	}
	return raw
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "om4compare:", err)
		os.Exit(1)
	}
}
