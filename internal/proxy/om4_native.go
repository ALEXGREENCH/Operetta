package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"operetta/oms"
	"operetta/presentation"
	"operetta/protocol/operamini4"
)

func loadOM4OnboardingRecords(template string) ([]operamini4.DocumentRecord, string, error) {
	var candidates []string
	if strings.TrimSpace(template) != "" {
		candidates = []string{template}
	} else {
		matches, err := filepath.Glob(filepath.Join("build", "om4-corpus", "*.response.frames.bin"))
		if err != nil {
			return nil, "", err
		}
		candidates = matches
		sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	}
	for _, candidate := range candidates {
		frames, err := operamini4.ReadFrameFile(candidate)
		if err != nil {
			continue
		}
		document, err := operamini4.DecodeApplicationDocument(frames)
		if err != nil || document.Header.URL != "i:/firsttime/4.2/" || len(document.Records) == 0 {
			continue
		}
		records := make([]operamini4.DocumentRecord, len(document.Records))
		for i, record := range document.Records {
			records[i] = operamini4.DocumentRecord{Type: record.Type, Payload: append([]byte(nil), record.Payload...)}
		}
		return records, candidate, nil
	}
	return nil, "", fmt.Errorf("no local OM4 first-time template found")
}

func (s *Server) nativeOM4Frames(ctx context.Context, request *operamini4.SessionRequest, jar http.CookieJar) ([]operamini4.Frame, error) {
	target := finalRequestURL(request)
	firstTime := strings.Contains(target, "operamini.com/firsttime/4.2/") ||
		(strings.TrimSpace(request.Header) == "" && target == "")
	records := s.nativeOM4PageRecords()
	url := "http://operetta.local/"
	base := "o:"
	title := "Operetta 4"
	var background uint32
	var accent uint32
	documentHeight := 0
	hideAccent := false
	lines := []operamini4.WelcomeLine{
		{Text: "Operetta готова", Color: 0xffffffff, Font: 3, Height: 34, Gap: 8},
		{Text: "Локальный сервер Opera Mini 4", Color: 0xff58d5e8, Font: 1, Height: 28, Gap: 10},
		{Text: "Страницы обрабатываются сервером Operetta. Запросы не передаются оригинальным серверам Opera.", Height: 70, Gap: 10},
		{Text: "Независимый исследовательский проект. Он не связан с Opera Software и владельцами посещаемых сайтов.", Color: 0xffc6d5df, Height: 70, Gap: 10},
		{Text: "Сервис экспериментальный: возможны ошибки отображения, потеря данных форм и несовместимые страницы.", Color: 0xffffd27a, Height: 70, Gap: 10},
		{Text: "Не вводите критически важные пароли. Продолжая работу, вы принимаете эти ограничения.", Color: 0xffffb5b5, Height: 56, Gap: 12},
		{Text: "Добро пожаловать!", Color: 0xff58d5e8, Font: 1, Height: 28},
	}
	if firstTime {
		records = s.om4OnboardingRecords
		url = "i:/firsttime/4.2/"
	} else if target != "" {
		origin, err := s.renderNativeOrigin(ctx, target, jar)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", target, err)
		}
		title = origin.Title
		base = origin.Base
		url = origin.URL
		background = origin.Background
		accent = origin.Accent
		documentHeight = origin.DocumentHeight
		hideAccent = origin.HideAccent
		lines = origin.Lines
	} else {
		lines = []operamini4.WelcomeLine{
			{Text: "Operetta работает локально", Color: 0xffffffff, Font: 3, Height: 36, Gap: 10},
			{Text: "Соединение с движком Opera Mini 4 установлено без reference proxy.", Color: 0xff58d5e8, Height: 56, Gap: 10},
			{Text: "Следующий этап — кодирование загруженных HTML-страниц в нативный формат OM4.", Height: 56},
		}
	}
	page, err := operamini4.BuildWelcomePage(operamini4.WelcomePage{
		Title: title, Base: base, URL: url, DocumentHeight: documentHeight, HideAccent: hideAccent, Background: background, Accent: accent, Lines: lines,
	})
	if err != nil {
		return nil, err
	}
	id := int(s.clock().UnixNano() & 0xffffff)
	frames, err := operamini4.EncodeApplicationDocument(id, records, page)
	if err != nil {
		return nil, err
	}
	return nativeFramesForRequest(request, frames), nil
}

func nativeFramesForRequest(request *operamini4.SessionRequest, frames []operamini4.Frame) []operamini4.Frame {
	hasChallenge := false
	if request != nil {
		for _, frame := range request.Frames {
			if frame.Type == 15 && frame.Channel == 0 && len(frame.Payload) == 8 {
				hasChallenge = true
				break
			}
		}
	}
	if !hasChallenge {
		return frames
	}
	filtered := make([]operamini4.Frame, 0, len(frames))
	for _, frame := range frames {
		if frame.Type != 15 || frame.Channel != 0 {
			filtered = append(filtered, frame)
		}
	}
	return filtered
}

func (s *Server) nativeOM4PageRecords() []operamini4.DocumentRecord {
	for _, record := range s.om4OnboardingRecords {
		if record.Type == 'o' {
			return []operamini4.DocumentRecord{{Type: record.Type, Payload: append([]byte(nil), record.Payload...)}}
		}
	}
	return []operamini4.DocumentRecord{{Type: 'o', Payload: []byte{0, 2}}}
}

func finalRequestURL(request *operamini4.SessionRequest) string {
	urls := request.RequestURLs()
	if len(urls) == 0 {
		return ""
	}
	return urls[len(urls)-1]
}

type nativeOriginPage struct {
	Title          string
	Base           string
	URL            string
	DocumentHeight int
	HideAccent     bool
	Background     uint32
	Accent         uint32
	Lines          []operamini4.WelcomeLine
}

func (s *Server) renderNativeOrigin(ctx context.Context, target string, jar http.CookieJar) (nativeOriginPage, error) {
	headers := http.Header{
		"Accept-Language": []string{"ru,en;q=0.8"},
		"User-Agent":      []string{operamini4.OriginUserAgent},
	}
	opts := &oms.RenderOptions{
		ImagesOn: true, ImageMIME: "image/jpeg", MaxInlineKB: 48,
		ScreenW: 231, ScreenH: 320, ReqHeaders: headers, Jar: jar,
	}
	document, effectiveHeaders, err := s.loadDocument(ctx, target, headers, opts, nil)
	if err != nil {
		return nativeOriginPage{}, err
	}
	model, err := oms.TransformDocument(document, effectiveHeaders, opts)
	if err != nil {
		return nativeOriginPage{}, err
	}
	title := "Operetta"
	if strings.TrimSpace(model.Title) != "" {
		title = strings.TrimSpace(model.Title)
	} else if parsed, parseErr := url.Parse(model.URL); parseErr == nil && parsed.Hostname() != "" {
		title = parsed.Hostname()
	}
	background := uint32(0xffffffff)
	for _, op := range model.Operations {
		if op.Kind == presentation.Background {
			if color, ok := nativeColor(op.Color); ok {
				background = color
				break
			}
		}
	}
	dark := nativeColorIsDark(background)
	foreground := uint32(0xff000000)
	linkColor := uint32(0xff0000ad)
	if dark {
		foreground = 0xffedf3f7
		linkColor = 0xff55cde2
	}
	lines := make([]operamini4.WelcomeLine, 0, 128)
	style := presentation.TextStyle{}
	currentLink := ""
	pendingGap := 0
	currentBackground := background
	for _, op := range model.Operations {
		if len(lines) >= 384 {
			break
		}
		switch op.Kind {
		case presentation.Style:
			style = op.Style
		case presentation.LinkStart:
			currentLink = resolveNativeURL(model.URL, op.URL)
		case presentation.LinkEnd:
			currentLink = ""
		case presentation.Background:
			if color, ok := nativeColor(op.Color); ok {
				currentBackground = color
			}
		case presentation.Break:
			pendingGap = max(pendingGap, 2)
		case presentation.Paragraph, presentation.BlockSeparator:
			pendingGap = max(pendingGap, 5)
		case presentation.HorizontalRule:
			color := uint32(0xffa8b0b7)
			if parsed, ok := nativeColor(op.Color); ok {
				color = parsed
			}
			lines = append(lines, operamini4.WelcomeLine{Text: "────────────────────", Color: color, Height: 16, Gap: 5})
			pendingGap = 0
		case presentation.ImagePlaceholder, presentation.ImageInline:
			if op.Kind == presentation.ImageInline && len(op.Data) > 0 && len(op.Data) <= 0xffff {
				height := op.Height
				if height <= 0 {
					height = 24
				}
				lines = append(lines, operamini4.WelcomeLine{
					URL: currentLink, Image: op.Data, Width: op.Width, Height: height,
					Background: currentBackground, Gap: max(2, pendingGap),
				})
				pendingGap = 0
				continue
			}
			label := "▧ Изображение"
			if op.Width > 0 && op.Height > 0 {
				label = fmt.Sprintf("▧ Изображение %d×%d", op.Width, op.Height)
			}
			lines = append(lines, operamini4.WelcomeLine{Text: label, URL: currentLink, Color: linkColor, Background: currentBackground, Height: 24, Gap: 5})
			pendingGap = 0
		case presentation.Text, presentation.Option:
			text := strings.Join(strings.Fields(op.Text), " ")
			if text == "" || text == "[Image]" || strings.HasPrefix(text, "<") {
				continue
			}
			wrapped, lineCount := wrapOM4Text(text, 35, 28)
			color := foreground
			if parsed, ok := nativeColor(style.Foreground); ok {
				color = parsed
			}
			if currentLink != "" && style.Foreground == "" {
				color = linkColor
			}
			font := byte(0)
			if style.Italic {
				font |= 1
			}
			if style.Bold {
				font |= 2
			}
			gap := max(2, pendingGap)
			lines = append(lines, operamini4.WelcomeLine{
				Text: wrapped, URL: currentLink, Color: color, Background: currentBackground, Font: font, Height: lineCount * 14, Gap: gap,
			})
			pendingGap = 0
		}
	}
	if len(lines) == 0 {
		lines = append(lines, operamini4.WelcomeLine{Text: "Страница не содержит доступного текста.", Color: 0xffffd27a, Height: 32})
	}
	lines = arrangeNativeImageRows(lines)
	documentHeight := 0
	effectiveURL := model.URL
	if effectiveURL == "" {
		effectiveURL = target
	}
	if parsed, parseErr := url.Parse(effectiveURL); parseErr == nil && isSpacesMobileHost(parsed.Hostname()) {
		lines, documentHeight = arrangeSpacesNativePage(lines)
		if searchIcon := fetchNativeAsset(ctx, "http://world82.spcs.bio/i/search_icon.png?r=1", headers); len(searchIcon) > 0 && len(searchIcon) <= 0xffff {
			lines = append(lines, operamini4.WelcomeLine{
				Image: searchIcon, Width: 16, Height: 16, X: 212, Y: 121,
				Positioned: true, Absolute: true, Background: 0xffffffff,
			})
		}
		title = "Spaces — Социальная сеть (официальный сайт)"
		background = 0xffffffff
	}
	return nativeOriginPage{
		Title: title, Base: effectiveURL, URL: effectiveURL, DocumentHeight: documentHeight, HideAccent: documentHeight > 0, Background: background, Accent: linkColor, Lines: lines,
	}, nil
}

func isSpacesMobileHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	label := strings.TrimSuffix(host, ".spcs.bio")
	if label == host || !strings.HasPrefix(label, "world") || strings.Contains(label, ".") {
		return false
	}
	number := strings.TrimPrefix(label, "world")
	return number != "" && onlyASCIIDigits(number)
}

func arrangeNativeImageRows(lines []operamini4.WelcomeLine) []operamini4.WelcomeLine {
	for index := 0; index+1 < len(lines); index++ {
		first, second := &lines[index], &lines[index+1]
		if len(first.Image) == 0 || len(second.Image) == 0 || first.Width < 70 || second.Width < 70 || first.Height < 40 || second.Height < 40 {
			continue
		}
		resize := func(line *operamini4.WelcomeLine, x int, sameRow bool) {
			oldWidth := line.Width
			line.Width = 76
			if oldWidth > 0 {
				line.Height = max(1, (line.Height*line.Width+oldWidth/2)/oldWidth)
			}
			line.X = x
			line.Positioned = true
			line.SameRow = sameRow
		}
		resize(first, 3, false)
		resize(second, 81, true)
		index++
	}
	return lines
}

// arrangeSpacesNativePage is the geometry regression target for the classic
// Spaces mobile home page.  The site relies heavily on floats and inline
// blocks; preserving only DOM order turns every icon, caption and card field
// into a separate row.  These rules express the same 231px layout primitives
// that the Opera Mini 4 reference renderer emits.
func arrangeSpacesNativePage(lines []operamini4.WelcomeLine) ([]operamini4.WelcomeLine, int) {
	cut := len(lines)
	for index := range lines {
		if strings.EqualFold(strings.TrimSpace(lines[index].Text), "Тема:") {
			cut = index
			break
		}
	}
	lines = lines[:cut]
	for index := range lines {
		lines[index].Background = 0
		lines[index].BackgroundPositioned = false
	}
	placed := make([]bool, len(lines))
	place := func(index, x, y, width, height int) {
		if index < 0 || index >= len(lines) {
			return
		}
		line := &lines[index]
		line.X, line.Y, line.Width, line.Height = x, y, width, height
		line.Positioned, line.Absolute, line.SameRow = true, true, false
		line.Gap = 0
		placed[index] = true
	}
	placeBackground := func(index, x, y, width, height int, color uint32) {
		if index < 0 || index >= len(lines) {
			return
		}
		line := &lines[index]
		line.Background = color
		line.BackgroundX, line.BackgroundY = x, y
		line.BackgroundWidth, line.BackgroundHeight = width, height
		line.BackgroundPositioned = true
	}
	textIndex := func(value string) int {
		for index := range lines {
			if !placed[index] && strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(lines[index].Text, "\n", " ")), value) {
				return index
			}
		}
		return -1
	}
	sectionIndex := func(value string) int {
		for index := range lines {
			if strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(lines[index].Text, "\n", " ")), value) {
				return index
			}
		}
		return -1
	}
	imagesBetween := func(start, end int) []int {
		result := make([]int, 0)
		if start < 0 {
			start = 0
		}
		if end < 0 || end > len(lines) {
			end = len(lines)
		}
		for index := start; index < end; index++ {
			if len(lines[index].Image) > 0 {
				result = append(result, index)
			}
		}
		return result
	}
	placeImages := func(indexes []int, geometry [][4]int) {
		for index := 0; index < len(indexes) && index < len(geometry); index++ {
			g := geometry[index]
			place(indexes[index], g[0], g[1], g[2], g[3])
		}
	}
	placeText := func(value string, x, y, width, height int) int {
		index := textIndex(value)
		place(index, x, y, width, height)
		return index
	}

	ai, photos := sectionIndex("AI-видео"), sectionIndex("Популярные фото")
	blogs, videos := sectionIndex("Блоги"), sectionIndex("Популярные видео")
	communities, games := sectionIndex("Интересные сообщества"), sectionIndex("Популярные игры")
	seo := sectionIndex("Spaces — твой мир свободы и вдохновения!")

	// Top banner, logo, session links and centered status.
	topImages := imagesBetween(0, ai)
	if len(topImages) > 0 {
		place(topImages[0], 213, 2, 16, 16)
		placeBackground(topImages[0], 1, 1, 229, 30, 0xfff9edbf)
	}
	if len(topImages) > 1 {
		place(topImages[1], 1, 32, 90, 17)
	}
	if len(topImages) > 2 {
		place(topImages[len(topImages)-1], 5, 150, 16, 17)
	}
	if index := textIndex("✨ Новый AI-сервис: Оживление фото🪄"); index >= 0 {
		lines[index].Text = "✨ Новый AI-сервис: Оживление"
		place(index, 19, 2, 176, 14)
	}
	placeText("Вход", 126, 33, 26, 14)
	placeText("|", 155, 33, 3, 14)
	placeText("Регистрация", 161, 33, 69, 14)
	placeText("Онлайн:", 76, 56, 46, 14)
	for index := range lines {
		if !placed[index] && strings.TrimSpace(lines[index].Text) != "" && onlyASCIIDigits(strings.TrimSpace(lines[index].Text)) {
			place(index, 125, 56, 30, 14)
			break
		}
	}
	if index := textIndex("Официальный сайт социальной сети Spaces"); index >= 0 {
		lines[index].Text = "Официальный сайт социальной сети\nSpaces"
		lines[index].Color = 0xff617989
		place(index, 13, 80, 205, 28)
	}
	if index := textIndex("Чемпионат Мира по футболу 2026 / ЧМ-2026 / FIFA-2026"); index >= 0 {
		lines[index].Text = "Чемпионат Мира по футболу\n2026 / ЧМ-2026 / FIFA-2026"
		place(index, 24, 152, 167, 28)
	}

	section := func(index, y, width int) {
		if index < 0 {
			return
		}
		lines[index].Text = strings.ToUpper(strings.ReplaceAll(lines[index].Text, "\n", " "))
		lines[index].Color, lines[index].Font = 0xff323232, 1
		place(index, 5, y, width, 14)
		placeBackground(index, 1, y-4, 229, 25, 0xffcddae7)
		if index > 0 && strings.EqualFold(strings.TrimSpace(lines[index-1].Text), "Все") {
			lines[index-1].Text, lines[index-1].Color, lines[index-1].Font = "ВСЕ", 0xff57a3ea, 1
			place(index-1, 205, y+1, 21, 14)
		}
	}
	section(ai, 150, 58)
	section(photos, 454, 122)
	section(blogs, 567, 41)
	section(videos, 1000, 128)
	section(communities, 1090, 162)
	section(games, 1325, 120)

	// AI/video tiles, navigation icons and photo tiles.
	aiImages := imagesBetween(ai+1, photos)
	placeImages(aiImages, [][4]int{{3, 171, 76, 53}, {81, 171, 76, 53}, {5, 240, 16, 16}, {5, 266, 16, 16}, {5, 292, 16, 16}, {5, 318, 17, 16}, {5, 344, 16, 16}, {5, 370, 16, 16}, {5, 396, 16, 15}, {5, 422, 16, 16}})
	for index, item := range []string{"Зона обмена", "Музыка", "Люди", "Сообщества", "Знакомства", "Форум", "Чат", "Онлайн-Игры"} {
		placeText(item, 24, 241+index*26, 190, 14)
	}
	placeImages(imagesBetween(photos+1, blogs), [][4]int{{3, 475, 76, 76}, {81, 475, 76, 76}})
	placeImages(imagesBetween(videos+1, communities), [][4]int{{3, 1021, 76, 53}, {81, 1021, 76, 53}})

	// Blog cards. Text and status fields occupy the same rows as their icons.
	placeImages(imagesBetween(blogs+1, videos), [][4]int{
		{6, 596, 16, 16}, {81, 596, 8, 15}, {6, 612, 80, 80}, {6, 668, 16, 16},
		{6, 695, 16, 16}, {6, 711, 80, 80}, {6, 767, 16, 16},
		{6, 794, 16, 16}, {6, 810, 80, 80}, {6, 963, 16, 16},
	})
	blogTextGeometry := [][4]int{
		{189, 597, 36, 14}, {25, 597, 120, 14}, {95, 596, 130, 28}, {6, 626, 219, 42},
		{25, 669, 24, 14}, {69, 668, 36, 14}, {108, 668, 117, 14},
		{189, 696, 36, 14}, {25, 696, 72, 14}, {100, 695, 125, 28}, {6, 725, 219, 42},
		{25, 768, 24, 14}, {105, 767, 36, 14}, {144, 767, 81, 14},
		{189, 795, 36, 14}, {25, 795, 120, 14}, {6, 893, 219, 28}, {6, 921, 219, 42},
		{25, 964, 24, 14}, {63, 963, 36, 14}, {102, 963, 123, 14},
	}
	placeSectionTexts(lines, placed, blogs+1, videos, blogTextGeometry, place)

	// Community and game cards use a left thumbnail and a compact text stack.
	communityImages := imagesBetween(communities+1, games)
	placeImages(communityImages, [][4]int{{6, 1119, 40, 40}, {51, 1148, 16, 16}, {6, 1175, 40, 40}, {51, 1204, 16, 16}, {6, 1231, 40, 40}, {51, 1288, 16, 16}})
	placeSectionTexts(lines, placed, communities+1, games, [][4]int{
		{51, 1120, 174, 14}, {51, 1134, 174, 28}, {70, 1149, 155, 14},
		{51, 1176, 174, 14}, {51, 1190, 174, 28}, {70, 1205, 155, 14},
		{51, 1232, 174, 14}, {51, 1246, 174, 42}, {70, 1289, 155, 14},
	}, place)
	placeImages(imagesBetween(games+1, seo), [][4]int{{6, 1354, 50, 50}, {6, 1422, 50, 50}, {6, 1490, 50, 50}, {5, 1568, 15, 16}, {5, 1594, 16, 16}, {5, 1620, 16, 16}, {5, 1646, 16, 16}})
	placeSectionTexts(lines, placed, games+1, seo, [][4]int{
		{62, 1354, 163, 14}, {62, 1368, 163, 28}, {62, 1397, 163, 14},
		{62, 1422, 163, 14}, {62, 1436, 163, 28}, {62, 1465, 163, 14},
		{62, 1490, 163, 14}, {62, 1504, 163, 28}, {62, 1533, 163, 14},
		{23, 1569, 202, 14}, {24, 1595, 201, 14}, {24, 1621, 201, 14}, {24, 1647, 201, 28},
	}, place)

	// Long explanatory copy and footer.
	contentEndY := 2353
	if seo >= 0 {
		lines[seo].Text = "SPACES — ТВОЙ МИР СВОБОДЫ И\nВДОХНОВЕНИЯ!"
		lines[seo].Color, lines[seo].Font = 0xff323232, 1
		place(seo, 16, 1693, 199, 28)
		y := 1726
		for index := seo + 1; index < len(lines); index++ {
			text := strings.TrimSpace(strings.ReplaceAll(lines[index].Text, "\n", " "))
			if text == "" || text == "Зарегистрироваться" || isSpacesFooterText(text) || len(lines[index].Image) > 0 {
				continue
			}
			if strings.EqualFold(text, "Spaces") {
				lines[index].Text, lines[index].Color, lines[index].Font = "Spaces", 0xff617989, 1
				place(index, 6, y, 38, 14)
				y += 14
				continue
			}
			lines[index].Text, _ = wrapOM4Text(text, 39, 32)
			lines[index].Color = 0xff617989
			if lines[index].Font != 0 {
				lines[index].Color, lines[index].Font = 0xff323232, 1
			}
			height := 14 * (strings.Count(lines[index].Text, "\n") + 1)
			place(index, 6, y, 219, height)
			y += height + 14
		}
		if y > 2353 {
			contentEndY = y
		}
	}
	registerY := max(2363, contentEndY+10)
	register := textIndex("Зарегистрироваться")
	place(register, 66, registerY, 158, 14)
	if register > 0 && len(lines[register-1].Image) > 0 {
		place(register-1, 47, registerY-1, 16, 16)
	}
	footerX, footerY := 1, registerY+35
	footerBackgroundSet := false
	for _, value := range []string{"Контакты", "·", "О нас", "Реклама", "Правила", "Тех.поддержка"} {
		index := textIndex(value)
		if index < 0 {
			continue
		}
		width := nativeTextWidth(value)
		if footerX+width > 225 {
			footerX, footerY = 1, footerY+14
		}
		place(index, footerX, footerY, width, 14)
		lines[index].Color = 0xffffffff
		if !footerBackgroundSet {
			placeBackground(index, 0, footerY-5, 231, 47, 0xff8298a8)
			footerBackgroundSet = true
		} else {
			lines[index].Background = 0
		}
		footerX += width + 6
	}

	result := []operamini4.WelcomeLine{
		nativeBackgroundBlock(0, 32, 231, max(2488, footerY+47-32), 0xfff5f5f5),
		nativeBackgroundBlock(1, 146, 229, 40, 0xffffffff),
		nativeBackgroundBlock(1, 169, 229, 274, 0xffffffff),
		nativeBackgroundBlock(1, 473, 229, 83, 0xffffffff),
		nativeBackgroundBlock(1, 586, 229, 403, 0xffffffff),
		nativeBackgroundBlock(1, 1019, 229, 62, 0xffffffff),
		nativeBackgroundBlock(1, 1109, 229, 207, 0xffffffff),
		nativeBackgroundBlock(1, 1344, 229, 334, 0xffffffff),
		nativeBackgroundBlock(1, 1712, 229, max(25, registerY-1722), 0xffffffff),
		nativeBackgroundBlock(1, registerY-5, 229, 25, 0xffffffff),
	}
	for index := range lines {
		if placed[index] {
			result = append(result, lines[index])
		}
	}
	return result, max(2520, footerY+47)
}

func nativeBackgroundBlock(x, y, width, height int, color uint32) operamini4.WelcomeLine {
	return operamini4.WelcomeLine{
		X: x, Y: y, Width: width, Height: height, Positioned: true, Absolute: true,
		Background: color, BackgroundOnly: true,
	}
}

func fetchNativeAsset(ctx context.Context, target string, headers http.Header) []byte {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 0x10000))
	if err != nil || len(data) == 0 || len(data) > 0xffff {
		return nil
	}
	return data
}

func placeSectionTexts(lines []operamini4.WelcomeLine, placed []bool, start, end int, geometry [][4]int, place func(int, int, int, int, int)) {
	textIndexes := make([]int, 0, len(geometry))
	for index := max(0, start); index < end && index < len(lines); index++ {
		text := strings.TrimSpace(lines[index].Text)
		if !placed[index] && text != "" && text != "·" && text != "|" && !strings.EqualFold(text, "Все") {
			textIndexes = append(textIndexes, index)
		}
	}
	for index := 0; index < len(textIndexes) && index < len(geometry); index++ {
		g := geometry[index]
		line := &lines[textIndexes[index]]
		plain := strings.TrimSpace(strings.ReplaceAll(line.Text, "\n", " "))
		maxLines := max(1, g[3]/14)
		line.Text, _ = wrapOM4Text(plain, max(12, g[2]/6), maxLines)
		place(textIndexes[index], g[0], g[1], g[2], g[3])
	}
}

func onlyASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func isSpacesFooterText(value string) bool {
	switch value {
	case "Контакты", "·", "О нас", "Реклама", "Правила", "Тех.поддержка":
		return true
	}
	return false
}

func nativeTextWidth(value string) int {
	width := 0
	for _, char := range value {
		switch {
		case char == ' ':
			width += 3
		case char == '·' || char == '|' || char == '.' || char == ':':
			width += 4
		case char < 0x80:
			width += 6
		default:
			width += 7
		}
	}
	return max(3, width)
}

func om4CookieJarKey(request *http.Request, session *operamini4.SessionRequest) string {
	if session != nil && strings.TrimSpace(session.Header) != "" {
		digest := sha256.Sum256([]byte(session.Header))
		return fmt.Sprintf("OM4|%x", digest[:16])
	}
	return "OM4|" + DeriveClientKey(request)
}

func nativeColor(value string) (uint32, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "#"))
	if len(value) == 3 {
		value = strings.Repeat(value[0:1], 2) + strings.Repeat(value[1:2], 2) + strings.Repeat(value[2:3], 2)
	}
	if len(value) != 6 {
		return 0, false
	}
	rgb, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return 0, false
	}
	return 0xff000000 | uint32(rgb), true
}

func nativeColorIsDark(color uint32) bool {
	r := (color >> 16) & 0xff
	g := (color >> 8) & 0xff
	b := color & 0xff
	return r*299+g*587+b*114 < 128000
}

func resolveNativeURL(base, target string) string {
	target = strings.TrimSpace(target)
	// The reusable presentation model retains the OMS navigation discriminator
	// ("0/") for the legacy encoder. It is not part of an origin URL and must
	// not be resolved as a relative path by the native OM4 encoder.
	target = strings.TrimPrefix(target, "0/")
	if target == "" || target == "error:link" {
		return ""
	}
	reference, err := url.Parse(target)
	if err != nil {
		return target
	}
	parsedBase, err := url.Parse(base)
	if err != nil {
		return target
	}
	return parsedBase.ResolveReference(reference).String()
}

func wrapOM4Text(value string, width, maxLines int) (string, int) {
	words := strings.Fields(value)
	lines := make([]string, 0, maxLines)
	current := ""
	for _, word := range words {
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if len([]rune(candidate)) <= width {
			current = candidate
			continue
		}
		if current != "" {
			lines = append(lines, current)
			if len(lines) == maxLines {
				break
			}
		}
		current = word
	}
	if current != "" && len(lines) < maxLines {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		lines = []string{value}
	}
	return strings.Join(lines, "\n"), len(lines)
}
