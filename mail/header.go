package mail

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
	_ "golang.org/x/image/webp"
)

// The header is the top of the email — logo, headline and the Discord ↔
// university pair — drawn as one PNG and sent inline. Mail clients load no web
// fonts, drop rounded corners and squeeze the three-column pair on phones; an
// image looks the same everywhere. The fonts are Fira (SIL OFL, fonts/OFL.txt).

var (
	//go:embed fonts/FiraSansExtraCondensed-ExtraBold.ttf
	headlineTTF []byte
	//go:embed fonts/FiraSans-Bold.ttf
	boldTTF []byte
	//go:embed fonts/FiraSans-Regular.ttf
	regularTTF []byte

	headlineFont = mustFont(headlineTTF)
	boldFont     = mustFont(boldTTF)
	regularFont  = mustFont(regularTTF)
)

func mustFont(b []byte) *sfnt.Font {
	f, err := opentype.Parse(b)
	if err != nil {
		panic(err)
	}
	return f
}

// The palette and sizes of the pages, in CSS pixels; the PNG is drawn at
// scale times that for sharp text on high-density screens.
const (
	headerWidth = 384 // the sheet's inner width
	scale       = 2
	ink         = 0x1B1A17
	paper       = 0xFEFBF1
	cardFill    = 0xF8F2DC
	yellow      = 0xFFC900
	muted       = 0x5A5648
	white       = 0xFFFFFF
	blurple     = 0x5865F2
)

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xFF}
}

// canvas draws in CSS pixels onto a scaled RGBA image.
type canvas struct{ img *image.RGBA }

func (c canvas) px(v float32) float32 { return v * scale }

// roundRect fills a rounded rectangle given in CSS pixels.
func (c canvas) roundRect(x0, y0, x1, y1, r float32, col uint32) {
	c.roundRectMask(x0, y0, x1, y1, r).Draw(c.img, c.img.Bounds(), image.NewUniform(rgb(col)), image.Point{})
}

// box fills a rounded rectangle with a 2px ink border.
func (c canvas) box(x0, y0, x1, y1, r float32, fill uint32) {
	c.roundRect(x0, y0, x1, y1, r, ink)
	c.roundRect(x0+2, y0+2, x1-2, y1-2, r-2, fill)
}

func (c canvas) roundRectMask(x0, y0, x1, y1, r float32) *vector.Rasterizer {
	b := c.img.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	x0, y0, x1, y1, r = c.px(x0), c.px(y0), c.px(x1), c.px(y1), c.px(r)
	const k = 0.5523 // cubic approximation of a quarter circle
	z.MoveTo(x0+r, y0)
	z.LineTo(x1-r, y0)
	z.CubeTo(x1-r+r*k, y0, x1, y0+r-r*k, x1, y0+r)
	z.LineTo(x1, y1-r)
	z.CubeTo(x1, y1-r+r*k, x1-r+r*k, y1, x1-r, y1)
	z.LineTo(x0+r, y1)
	z.CubeTo(x0+r-r*k, y1, x0, y1-r+r*k, x0, y1-r)
	z.LineTo(x0, y0+r)
	z.CubeTo(x0, y0+r-r*k, x0+r-r*k, y0, x0+r, y0)
	z.ClosePath()
	return z
}

// picture draws src scaled into a rounded square, clipped to its corners.
func (c canvas) picture(src image.Image, x, y, size, r float32, bg uint32) {
	c.roundRect(x, y, x+size, y+size, r, bg)
	d := int(c.px(size))
	scaled := image.NewRGBA(image.Rect(0, 0, d, d))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	// The rasterizer reads src at the same point it writes dst, so the
	// scaled picture is moved to where it lands.
	scaled.Rect = scaled.Rect.Add(image.Pt(int(c.px(x)), int(c.px(y))))
	c.roundRectMask(x, y, x+size, y+size, r).Draw(c.img, c.img.Bounds(), scaled, image.Point{})
}

// arrow draws a ↔ centred on cx, cy. It is a shape, not text: Fira has no
// arrow glyphs.
func (c canvas) arrow(cx, cy float32) {
	b := c.img.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	p := func(x, y float32) (float32, float32) { return c.px(cx + x), c.px(cy + y) }
	pts := [][2]float32{{-9, 0}, {-4, -5}, {-4, -1.4}, {4, -1.4}, {4, -5}, {9, 0}, {4, 5}, {4, 1.4}, {-4, 1.4}, {-4, 5}}
	z.MoveTo(p(pts[0][0], pts[0][1]))
	for _, pt := range pts[1:] {
		z.LineTo(p(pt[0], pt[1]))
	}
	z.ClosePath()
	z.Draw(c.img, b, image.NewUniform(rgb(ink)), image.Point{})
}

// framed draws a picture with a 2px ink ring around it.
func (c canvas) framed(src image.Image, x, y, size, r float32, bg uint32) {
	c.roundRect(x, y, x+size, y+size, r, ink)
	c.picture(src, x+2, y+2, size-4, r-2, bg)
}

// face is a font at a CSS pixel size.
type face struct {
	f    *sfnt.Font
	face font.Face
	size float32
}

func newFace(f *sfnt.Font, size float32) face {
	ff, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size * scale), DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	return face{f, ff, size}
}

// width is s's advance in CSS pixels.
func (f face) width(s string) float32 {
	return float32(font.MeasureString(f.face, s)) / 64 / scale
}

// ascent is the height above the baseline in CSS pixels.
func (f face) ascent() float32 {
	return float32(f.face.Metrics().Ascent) / 64 / scale
}

// clean drops characters the font cannot draw, such as emoji in a username,
// rather than showing empty boxes.
func (f face) clean(s string) string {
	var buf sfnt.Buffer
	var b strings.Builder
	for _, r := range s {
		if i, err := f.f.GlyphIndex(&buf, r); err == nil && i != 0 {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// text draws s with its baseline at y, starting at x.
func (c canvas) text(f face, s string, x, baseline float32, col uint32) {
	d := font.Drawer{Dst: c.img, Src: image.NewUniform(rgb(col)), Face: f.face,
		Dot: fixed.Point26_6{X: fixed.Int26_6(c.px(x) * 64), Y: fixed.Int26_6(c.px(baseline) * 64)}}
	d.DrawString(s)
}

// centred draws lines centred on cx from top, lineHeight apart, and returns
// the y below them.
func (c canvas) centred(f face, lines []string, cx, top, lineHeight float32, col uint32) float32 {
	for _, l := range lines {
		c.text(f, l, cx-f.width(l)/2, top+(lineHeight-f.size)/2+f.size*0.8, col)
		top += lineHeight
	}
	return top
}

// wrap breaks s into at most max lines no wider than w: at spaces, before an
// @ so an address splits into user and domain, and mid-word when nothing
// else fits. The last line ends in an ellipsis if text was cut.
func wrap(f face, s string, w float32, max int) []string {
	var words []string
	for _, word := range strings.Fields(s) {
		if user, domain, ok := strings.Cut(word, "@"); ok && f.width(word) > w {
			words = append(words, user, "@"+domain)
			continue
		}
		words = append(words, word)
	}
	var lines []string
	line := ""
	for _, word := range words {
		join := line + " " + word
		if line == "" {
			join = word
		}
		if line == "" || (!strings.HasPrefix(word, "@") && f.width(join) <= w) {
			line = join
			continue
		}
		lines = append(lines, line)
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	// Words wider than the line are cut by characters.
	var out []string
	for _, l := range lines {
		for f.width(l) > w {
			n := len(l)
			for n > 0 && f.width(l[:n]) > w {
				_, size := utf8.DecodeLastRuneInString(l[:n])
				n -= size
			}
			if n == 0 {
				break
			}
			out = append(out, l[:n])
			l = l[n:]
		}
		out = append(out, l)
	}
	if len(out) > max {
		last := out[max-1]
		for last != "" && f.width(last+"…") > w {
			_, size := utf8.DecodeLastRuneInString(last)
			last = last[:len(last)-size]
		}
		out = append(out[:max-1], strings.TrimSpace(last)+"…")
	}
	return out
}

// headline is the title, with one word on a yellow marker.
var headline = []struct {
	word string
	mark bool
}{{"Σύνδεσε", false}, {"τον", false}, {"λογαριασμό", true}, {"σου", false}}

// renderHeader draws the header PNG for l.
func renderHeader(ctx context.Context, l Link) ([]byte, error) {
	logo, err := decode(l.Logo)
	if err != nil {
		return nil, fmt.Errorf("steki logo: %w", err)
	}
	uniLogo, err := decode(l.UniLogo)
	if err != nil {
		return nil, fmt.Errorf("university logo: %w", err)
	}
	avatar := fetchAvatar(ctx, l.AvatarURL)

	big := newFace(headlineFont, 36)
	name := newFace(boldFont, 14)
	addr := newFace(boldFont, 13)
	small := newFace(regularFont, 12)

	// Lay the headline out first: its height decides where the pair goes.
	const lineH, markPad = 38, 5
	type placed struct {
		word    string
		mark    bool
		x, line float32
	}
	var words []placed
	var x, line float32
	space := big.width(" ")
	for _, w := range headline {
		ww := big.width(w.word)
		if w.mark {
			ww += 2 * markPad
		}
		if x > 0 && x+space+ww > headerWidth {
			x, line = 0, line+1
		} else if x > 0 {
			x += space
		}
		words = append(words, placed{w.word, w.mark, x, line})
		x += ww
	}
	const titleTop = 52 + 22
	pairTop := titleTop + (line+1)*lineH + 24

	// Each card: picture, the main line(s) and the small line(s) under it.
	const gap, pic = 48, 64
	cardW := float32(headerWidth-gap-4) / 2 // 4px left for the offset shadow
	textW := cardW - 16
	user := wrap(name, name.clean(l.Username), textW, 2)
	if len(user) == 0 {
		user = []string{"Discord"}
	}
	email := wrap(addr, addr.clean(l.Email), textW, 2)
	uni := wrap(small, small.clean(l.UniName), textW, 3)
	textH := func(main, sub int) float32 { return float32(main)*20 + float32(sub)*17 }
	cardH := 16 + pic + 10 + max(textH(len(user), 1), textH(len(email), len(uni))) + 12
	height := pairTop + cardH + 4

	c := canvas{image.NewRGBA(image.Rect(0, 0, headerWidth*scale, int(height*scale)))}
	draw.Draw(c.img, c.img.Bounds(), image.NewUniform(rgb(paper)), image.Point{}, draw.Src)

	c.framed(logo, 0, 0, 52, 12, white)

	for _, w := range words {
		top := titleTop + w.line*lineH
		tx := w.x
		if w.mark {
			c.roundRect(w.x, top+4, w.x+big.width(w.word)+2*markPad, top+lineH, 2, yellow)
			tx += markPad
		}
		c.text(big, w.word, tx, top+big.ascent()*0.88, ink)
	}

	card := func(x float32, picture image.Image, picBG uint32, main face, mainLines, sub []string) {
		c.roundRect(x+4, pairTop+4, x+cardW+4, pairTop+cardH+4, 8, ink)
		c.box(x, pairTop, x+cardW, pairTop+cardH, 8, cardFill)
		cx := x + cardW/2
		c.framed(picture, cx-pic/2, pairTop+16, pic, pic/2, picBG)
		y := c.centred(main, mainLines, cx, pairTop+16+pic+10, 20, ink)
		c.centred(small, sub, cx, y, 17, muted)
	}
	card(0, avatar, blurple, name, user, []string{"Discord"})
	card(cardW+gap, uniLogo, white, addr, email, uni)

	// The link between the two cards.
	cx, cy := cardW+gap/2, pairTop+16+pic/2
	c.roundRect(cx-17, cy-17, cx+17, cy+17, 17, ink)
	c.roundRect(cx-15, cy-15, cx+15, cy+15, 15, yellow)
	c.arrow(cx, cy)

	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, c.img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decode(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// fetchAvatar downloads the member's Discord avatar. A missing avatar is not
// worth failing the email over: it falls back to a plain Discord-blue disc.
func fetchAvatar(ctx context.Context, url string) image.Image {
	fallback := image.NewUniform(rgb(blurple))
	blank := image.NewRGBA(image.Rect(0, 0, 1, 1))
	draw.Draw(blank, blank.Bounds(), fallback, image.Point{}, draw.Src)
	if url == "" {
		return blank
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return blank
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return blank
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return blank
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return blank
	}
	img, err := decode(body)
	if err != nil {
		return blank
	}
	return img
}
