// microfiche — MCP server that serves large read-only files as rendered
// images ("optical compression"), cutting LLM input-token cost.
//
// Every design constraint here is benchmark-derived (see README.md):
//   - density stays above the ~35-40 px²/char legibility cliff; past it,
//     models silently confabulate exact strings instead of failing loudly
//   - a verbatim factsheet of exact-looking strings rides along as text
//   - single-pass read instruction; Read-tool fallback is the safety valve
//   - auto-bail to plain text below break-even (~5k tokens) or <1.25x
//   - density profiles are per model family (-profile fable|opus)
//
// Register: claude mcp add microfiche -- /path/to/microfiche
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	maxPages     = 4
	pageChars    = 95_000
	pdfMode      = false
	tokenBudget  = 25_000
	maxFileChars = 200_000
)

const (
	lineSpacing    = 2
	charsPerToken  = 3.5
	pxPerToken     = 750 // Claude: image tokens ≈ w*h/750
	maxEdge        = 2400
	minTextTokens  = 5000 // measured break-even inside Claude Code
	minRatio       = 1.25
	factsheetLimit = 25
)

// fontSize is set by -profile: "fable" = 8 (~48 px²/char, needs
// Fable-class vision), "opus" = 10 (~72 px²/char — Opus 4.x was accurate
// at >=60 px²/char in the density sweep and failed at ~38-48).
var fontSize = 8
var opusProfile = false

var fontPaths = []string{
	"/System/Library/Fonts/Menlo.ttc",
	"/System/Library/Fonts/Monaco.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
	"C:\\Windows\\Fonts\\consola.ttf",
}

var lineColors = []color.RGBA{
	{0, 0, 0, 255}, {0, 0, 120, 255}, {100, 0, 0, 255}, {0, 80, 0, 255},
}

var emphColor = color.RGBA{150, 0, 0, 255}

var exactPatterns = []struct {
	re   *regexp.Regexp
	kind string
}{
	{regexp.MustCompile(`\b[0-9a-fA-F]{8,64}\b`), "hash/hex"},
	{regexp.MustCompile(`\b\d+\.\d+\.\d+[\w.-]*\b`), "version"},
	{regexp.MustCompile(`\b[A-Z][A-Z0-9_]{4,}\b`), "constant"},
	{regexp.MustCompile(`https?://\S+`), "url"},
	{regexp.MustCompile(`\b\d{4,}\b`), "number"},
}

// ---------------------------------------------------------------- font ---

var (
	face      font.Face
	charW     float64
	faceMu    sync.Mutex // font.Face is not safe for concurrent use
	fontError error
)

func loadFont() {
	for _, p := range fontPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f *opentype.Font
		if strings.HasSuffix(p, ".ttc") {
			coll, err := opentype.ParseCollection(data)
			if err != nil {
				continue
			}
			f, err = coll.Font(0)
			if err != nil {
				continue
			}
		} else {
			f, err = opentype.Parse(data)
			if err != nil {
				continue
			}
		}
		face, fontError = opentype.NewFace(f, &opentype.FaceOptions{
			Size: float64(fontSize), DPI: 72, Hinting: font.HintingFull,
		})
		if fontError == nil {
			adv, _ := face.GlyphAdvance('M')
			charW = float64(adv) / 64.0
			return
		}
	}
	fontError = fmt.Errorf("no usable monospace font found")
}

// ----------------------------------------------------------- factsheet ---

type exactToken struct{ kind, tok string }

func extractExactTokens(text string) []exactToken {
	seen := map[string]bool{}
	var out []exactToken
	for _, p := range exactPatterns {
		for _, m := range p.re.FindAllString(text, -1) {
			if !seen[m] && len(out) < factsheetLimit {
				seen[m] = true
				out = append(out, exactToken{p.kind, m})
			}
		}
	}
	return out
}

func formatFactsheet(tokens []exactToken) string {
	if len(tokens) == 0 {
		return "  (none detected)"
	}
	var b strings.Builder
	for i, t := range tokens {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  %s: %s", t.kind, t.tok)
	}
	return b.String()
}

// ------------------------------------------------------------ renderer ---

type wrappedLine struct {
	text string
	emph bool
}

func wrapLines(text string, colChars int, emphasize []string) []wrappedLine {
	var lines []wrappedLine
	blank := false
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(strings.ReplaceAll(raw, "\t", "    "), " ")
		if raw == "" {
			if !blank { // collapse runs of blank lines
				lines = append(lines, wrappedLine{"", false})
			}
			blank = true
			continue
		}
		blank = false
		emph := false
		for _, tok := range emphasize {
			if strings.Contains(raw, tok) {
				emph = true
				break
			}
		}
		for len(raw) > colChars {
			cut := strings.LastIndex(raw[colChars/2:colChars+1], " ")
			if cut == -1 {
				cut = colChars
			} else {
				cut += colChars / 2
			}
			lines = append(lines, wrappedLine{raw[:cut], emph})
			raw = strings.TrimLeft(raw[cut:], " ")
		}
		lines = append(lines, wrappedLine{raw, emph})
	}
	// selective line repetition (snapcompact): double exact-value lines
	out := make([]wrappedLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, l)
		if l.emph {
			out = append(out, l)
		}
	}
	return out
}

func render(text string, emphasize []string) *image.RGBA {
	faceMu.Lock()
	defer faceMu.Unlock()

	pad, gutter := 6, 12
	lineH := fontSize + lineSpacing

	// column width from the content's own line lengths
	rawLines := strings.Split(text, "\n")
	lens := make([]int, len(rawLines))
	for i, l := range rawLines {
		lens[i] = len(strings.ReplaceAll(l, "\t", "    "))
	}
	sort.Ints(lens)
	p95 := 80
	if len(lens) > 0 {
		p95 = lens[int(float64(len(lens))*0.95)]
	}
	colChars := min(max(p95, 40), 110)
	lines := wrapLines(text, colChars, emphasize)
	colW := int(charW*float64(colChars)) + 4

	// pick the column count giving the squarest image within maxEdge
	bestCols, bestW, bestH, bestPerCol := 1, 0, 0, 0
	bestScore := [2]int{1 << 30, 1 << 30}
	for c := 1; c <= 4; c++ {
		perCol := max((len(lines)+c-1)/c, 1)
		w := c*colW + (c-1)*gutter + 2*pad
		h := perCol*lineH + 2*pad
		if w > maxEdge {
			break
		}
		over := 0
		if h > maxEdge {
			over = 1
		}
		score := [2]int{over, abs(w - h)}
		if score[0] < bestScore[0] ||
			(score[0] == bestScore[0] && score[1] < bestScore[1]) {
			bestScore, bestCols, bestW, bestH, bestPerCol =
				score, c, w, h, perCol
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, bestW, bestH))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	d := &font.Drawer{Dst: img, Face: face}
	for c := 0; c < bestCols; c++ {
		x0 := pad + c*(colW+gutter)
		start := c * bestPerCol
		end := min(start+bestPerCol, len(lines))
		for i, l := range lines[start:end] {
			col := lineColors[(start+i)%len(lineColors)]
			if l.emph {
				col = emphColor
			}
			d.Src = image.NewUniform(col)
			d.Dot = fixed.P(x0, pad+i*lineH+fontSize)
			d.DrawString(l.text)
		}
	}
	return img
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ----------------------------------------------------------- telemetry ---

var logMu sync.Mutex

func logCall(entry map[string]any) {
	logMu.Lock()
	defer logMu.Unlock()
	dir := filepath.Join(homeDir(), ".microfiche")
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	entry["ts"] = time.Now().Unix()
	b, _ := json.Marshal(entry)
	f.Write(append(b, '\n'))
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// ----------------------------------------------------------------- tool ---

type microficheInput struct {
	FilePath  string `json:"file_path" jsonschema:"absolute path of the file to read"`
	Page      int    `json:"page,omitempty" jsonschema:"1-based page number for files above ~60k characters"`
	LineStart int    `json:"line_start,omitempty" jsonschema:"optional 1-based first line — read only a slice of the file (use after locating a region with Grep)"`
	LineEnd   int    `json:"line_end,omitempty" jsonschema:"optional 1-based last line of the slice"`
}

type cacheKey struct {
	path  string
	mtime int64
	page  int
}

var (
	cacheMu     sync.Mutex
	renderCache = map[cacheKey]*mcp.CallToolResult{}
)

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

func microfiche(ctx context.Context, req *mcp.CallToolRequest,
	in microficheInput) (*mcp.CallToolResult, any, error) {
	p := in.FilePath
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(homeDir(), p[2:])
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return textResult(fmt.Sprintf("microfiche: not a file: %s", p)),
			nil, nil
	}

	page := max(in.Page, 1)
	key := cacheKey{p, st.ModTime().UnixNano(), page}
	cacheMu.Lock()
	if r, ok := renderCache[key]; ok {
		cacheMu.Unlock()
		return r, nil, nil
	}
	cacheMu.Unlock()

	data, err := os.ReadFile(p)
	if err != nil {
		return textResult(fmt.Sprintf("microfiche: %v", err)), nil, nil
	}
	text := string(data)
	slice := ""
	if in.LineStart > 0 || in.LineEnd > 0 {
		lines := strings.Split(text, "\n")
		lo := max(in.LineStart, 1)
		hi := len(lines)
		if in.LineEnd > 0 {
			hi = min(in.LineEnd, hi)
		}
		if lo > hi {
			return textResult(fmt.Sprintf(
				"microfiche: empty line range %d-%d (file has %d lines)",
				in.LineStart, in.LineEnd, len(lines))), nil, nil
		}
		text = strings.Join(lines[lo-1:hi], "\n")
		slice = fmt.Sprintf(" [lines %d-%d]", lo, hi)
	}
	if len(text) > maxFileChars && slice == "" {
		logCall(map[string]any{"file": p, "skipped": true,
			"reason": "too_large", "chars": len(text)})
		return textResult(fmt.Sprintf(
			"microfiche: %s is too large to ingest efficiently (%d chars; "+
				"full-file imaging degrades past ~200KB). Locate what you "+
				"need with Grep, then call microfiche again with "+
				"line_start/line_end for that region — or Read a targeted "+
				"slice.", p, len(text))), nil, nil
	}
	nPages := max((len(text)+pageChars-1)/pageChars, 1)
	page = min(page, nPages)
	last := min(page+maxPages-1, nPages)
	// token budget: stop adding pages once the estimated image cost of the
	// response would exceed it (one page is always returned)
	var chunks []string
	estSoFar := 0
	for pg := page; pg <= last; pg++ {
		c := text[(pg-1)*pageChars : min(pg*pageChars, len(text))]
		pageTok := int(float64(len(c)) / charsPerToken / 2.5)
		if len(chunks) > 0 && estSoFar+pageTok > tokenBudget {
			last = pg - 1
			break
		}
		estSoFar += pageTok
		chunks = append(chunks, c)
	}
	combined := strings.Join(chunks, "")
	exact := extractExactTokens(combined)
	emphasize := make([]string, len(exact))
	for i, t := range exact {
		emphasize[i] = t.tok
	}

	imgs := make([]*image.RGBA, len(chunks))
	estImgTok := 0
	for i, c := range chunks {
		imgs[i] = render(c, emphasize)
		b := imgs[i].Bounds()
		estImgTok += b.Dx() * b.Dy() / pxPerToken
	}
	estTextTok := int(float64(len(combined)) / charsPerToken)

	// Bail to plain text when imaging can't win (sparse content, or below
	// the measured break-even size inside Claude Code).
	if float64(estImgTok) > float64(estTextTok)/minRatio ||
		estTextTok < minTextTokens {
		reason := "too_small"
		if estTextTok >= minTextTokens {
			reason = "sparse"
		}
		logCall(map[string]any{"file": p, "page": page, "bailed": true,
			"reason": reason, "est_text_tokens": estTextTok,
			"est_image_tokens": estImgTok})
		return textResult(fmt.Sprintf(
				"microfiche: %s is below the size/compression threshold where "+
					"imaging pays off (~%d text vs ~%d image tokens); returning "+
					"plain text.\n\n%s", p, estTextTok, estImgTok, combined)),
			nil, nil
	}

	pngs := make([][]byte, len(imgs))
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	for i, img := range imgs {
		var pngBuf bytes.Buffer
		if err := enc.Encode(&pngBuf, img); err != nil {
			return textResult(
				fmt.Sprintf("microfiche: render failed: %v", err)), nil, nil
		}
		pngs[i] = pngBuf.Bytes()
	}
	logCall(map[string]any{"file": p, "page": page, "last_page": last,
		"pages": nPages, "chars": len(combined), "pdf": pdfMode,
		"est_text_tokens": estTextTok, "est_image_tokens": estImgTok,
		"est_saved_tokens": estTextTok - estImgTok})

	var buf strings.Builder
	carrier := "the attached image"
	if len(chunks) > 1 {
		carrier = fmt.Sprintf("the %d attached images (in order)",
			len(chunks))
	}
	if pdfMode {
		carrier = "the attached PDF"
	}
	fmt.Fprintf(&buf,
		"microfiche: %s (pages %d-%d of %d, %d chars, ~%d text-tokens "+
			"delivered as ~%d image-tokens). The full content is in %s.\n"+
			"Verbatim factsheet (exact strings detected):\n%s\n"+
			"Read it in a single pass — do NOT crop, zoom, magnify, "+
			"re-render, or inspect it with Bash/other tools; that costs "+
			"more than it saves. If any part is not legible enough to "+
			"answer confidently, or you need a byte-exact value not in "+
			"the factsheet, use the Read tool on the original file "+
			"instead.",
		p+slice, page, last, nPages, len(combined), estTextTok, estImgTok,
		carrier, formatFactsheet(exact))
	if last < nPages {
		fmt.Fprintf(&buf, "\nFile continues: call microfiche again with "+
			"page=%d for the next part.", last+1)
	}
	if opusProfile {
		buf.WriteString("\nIMPORTANT: never transcribe codes, " +
			"identifiers, or numbers from the image itself — take them " +
			"ONLY from the factsheet above, or use Read on the original " +
			"file. Values read from the image may be misread.")
	}

	content := []mcp.Content{&mcp.TextContent{Text: buf.String()}}
	if pdfMode {
		pdfBytes, err := buildPDF(imgs, pngs)
		if err != nil {
			return textResult(
				fmt.Sprintf("microfiche: pdf failed: %v", err)), nil, nil
		}
		content = append(content, &mcp.EmbeddedResource{
			Resource: &mcp.ResourceContents{
				URI:      "microfiche://" + p,
				MIMEType: "application/pdf",
				Blob:     pdfBytes,
			},
		})
	} else {
		for _, pg := range pngs {
			content = append(content,
				&mcp.ImageContent{Data: pg, MIMEType: "image/png"})
		}
	}

	result := &mcp.CallToolResult{Content: content}
	cacheMu.Lock()
	renderCache[key] = result
	cacheMu.Unlock()
	return result, nil, nil
}

// ----------------------------------------------------------------- main ---

const toolDescription = `Read a LARGE read-only file cheaply by returning ` +
	`it as a rendered image instead of text (~3-4x fewer input tokens). ` +
	`Use this INSTEAD of Read for big reference material you only need ` +
	`to understand — docs, logs, transcripts, source you are not going ` +
	`to edit. Do NOT use it when you must copy exact strings out of the ` +
	`file (identifiers to reuse in code, hashes, config values) — use ` +
	`the regular Read tool for those. A factsheet of exact-looking ` +
	`values is included as text; anything not in it that must be ` +
	`byte-exact should be re-read with Read. For very large files, ` +
	`locate the region with Grep first, then pass line_start/line_end ` +
	`to read just that slice.`

func main() {
	renderFlag := flag.String("render", "",
		"preview: render a file to PNG on stdout instead of serving MCP")
	benchFlag := flag.String("bench", "",
		"benchmark: A/B a file through headless Claude Code (needs `claude`)")
	model := flag.String("model", "claude-fable-5", "model for -bench")
	reps := flag.Int("n", 2, "repetitions for -bench")
	question := flag.String("q",
		"In 2-3 sentences, what is this file about?", "question for -bench")
	profile := flag.String("profile", "fable",
		"vision profile: fable (dense, ~2.8x) | opus (conservative, ~2x)")
	flag.IntVar(&maxPages, "pages", 4,
		"max pages returned per call (multi-image response)")
	flag.IntVar(&pageChars, "pagechars", 95_000, "characters per page")
	flag.BoolVar(&pdfMode, "pdf", false,
		"return pages as a single no-text-layer PDF instead of images")
	flag.IntVar(&tokenBudget, "budget", 25_000,
		"max estimated image tokens returned per call")
	flag.IntVar(&maxFileChars, "maxfile", 200_000,
		"refuse to ingest files above this many characters (grep+slice instead)")
	statsFlag := flag.Bool("stats", false,
		"print savings statistics from ~/.microfiche/log.jsonl and exit")
	flag.Parse()
	if *profile == "opus" {
		fontSize = 10
		opusProfile = true
	}

	loadFont()
	if fontError != nil {
		log.Fatal(fontError)
	}

	if *statsFlag {
		printStats()
		return
	}

	if *benchFlag != "" {
		runBench(*benchFlag, *model, *profile, *question, *reps)
		return
	}

	if *renderFlag != "" {
		data, err := os.ReadFile(*renderFlag)
		if err != nil {
			log.Fatal(err)
		}
		chunk := string(data)
		if len(chunk) > pageChars {
			chunk = chunk[:pageChars]
		}
		exact := extractExactTokens(chunk)
		emph := make([]string, len(exact))
		for i, t := range exact {
			emph[i] = t.tok
		}
		img := render(chunk, emph)
		b := img.Bounds()
		fmt.Fprintf(os.Stderr,
			"size=%dx%d est_text_tok=%d est_img_tok=%d\n",
			b.Dx(), b.Dy(), int(float64(len(chunk))/charsPerToken),
			b.Dx()*b.Dy()/pxPerToken)
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(os.Stdout, img); err != nil {
			log.Fatal(err)
		}
		return
	}

	server := mcp.NewServer(
		&mcp.Implementation{Name: "microfiche", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "microfiche",
		Description: toolDescription,
	}, microfiche)
	if err := server.Run(context.Background(),
		&mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// buildPDF assembles rendered page images into a single no-text-layer PDF
// (pages carry only the raster, so models bill them as images, not
// image+text like ordinary PDFs).
func buildPDF(imgs []*image.RGBA, pngs [][]byte) ([]byte, error) {
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt"})
	for i, img := range imgs {
		b := img.Bounds()
		w, h := float64(b.Dx()), float64(b.Dy())
		name := fmt.Sprintf("page%d", i)
		pdf.RegisterImageOptionsReader(name,
			fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(pngs[i]))
		pdf.AddPageFormat("P", fpdf.SizeType{Wd: w, Ht: h})
		pdf.ImageOptions(name, 0, 0, w, h, false,
			fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	}
	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
