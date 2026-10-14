package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"  // registers GIF for DecodeConfig
	_ "image/jpeg" // registers JPEG for DecodeConfig
	_ "image/png"  // registers PNG for DecodeConfig
	"math"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// Limits on branding. The logo is shown at 28 pixels; anything larger than these is a
// mistake, or an attempt to use the logo slot to store something else.
const (
	MaxDisplayName   = 80
	MaxLoginBanner   = 2000
	MaxLogoBytes     = 64 << 10
	MaxLogoDimension = 1024
	// MinAccentContrast is the WCAG 2.1 non-text contrast minimum (1.4.11), measured
	// against the light theme's white surface, where the accent marks the current page,
	// links and focus rings.
	MinAccentContrast = 3.0
)

// LogoTypes are the image formats accepted for a logo. SVG is deliberately absent: an SVG
// is a document that can carry script, and served from this origin it would run with the
// dashboard's authority.
var LogoTypes = []string{"image/png", "image/jpeg", "image/gif"}

var hexColor = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// expandHex turns "#abc" into "#aabbcc".
func expandHex(c string) (string, bool) {
	if len(c) == 4 && c[0] == '#' {
		out := "#"
		for _, r := range c[1:] {
			out += string(r) + string(r)
		}
		return out, hexColor.MatchString(out)
	}
	return c, hexColor.MatchString(c)
}

// Validate checks a normalized branding.
func (b Branding) Validate() error {
	var problems []string
	if utf8.RuneCountInString(b.DisplayName) > MaxDisplayName {
		problems = append(problems, fmt.Sprintf("branding.display_name is longer than %d characters", MaxDisplayName))
	}
	if !printableText(b.DisplayName) || bytes.ContainsAny([]byte(b.DisplayName), "\n\t") {
		problems = append(problems, "branding.display_name must be one line of plain text")
	}
	if b.AccentColor != "" {
		if !hexColor.MatchString(b.AccentColor) {
			problems = append(problems, fmt.Sprintf("branding.accent_color must be a hex color such as #2f6f4e, not %q", b.AccentColor))
		} else if ratio := ContrastRatio(b.AccentColor, "#ffffff"); ratio < MinAccentContrast {
			problems = append(problems, fmt.Sprintf(
				"branding.accent_color %s has a contrast ratio of %.2f:1 against the white page; at least %.1f:1 is needed to be visible (choose a darker shade)",
				b.AccentColor, ratio, MinAccentContrast))
		}
	}
	if utf8.RuneCountInString(b.LoginBanner) > MaxLoginBanner {
		problems = append(problems, fmt.Sprintf("branding.login_banner is longer than %d characters", MaxLoginBanner))
	}
	if !printableText(b.LoginBanner) {
		problems = append(problems, "branding.login_banner must be plain text without control characters")
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// OnAccent is the text color that reads best on the accent: white or near-black.
func OnAccent(accent string) string {
	if ContrastRatio(accent, "#ffffff") >= ContrastRatio(accent, "#111111") {
		return "#ffffff"
	}
	return "#111111"
}

// ContrastRatio is the WCAG 2.1 contrast ratio between two "#rrggbb" colors, from 1 to 21.
func ContrastRatio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	if len(hex) != 7 {
		return 0
	}
	channel := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(hex[1:3]) + 0.7152*channel(hex[3:5]) + 0.0722*channel(hex[5:7])
}

// Logo is a validated logo image.
type Logo struct {
	ContentType string `json:"content_type"`
	Data        []byte `json:"-"`
	SHA256      string `json:"sha256"`
}

// CheckLogo validates an uploaded logo: at most MaxLogoBytes, a PNG, JPEG or GIF by its own
// bytes (whatever the upload claimed), that decodes to at most MaxLogoDimension pixels a
// side. declared, when not empty, must agree with what the bytes are.
func CheckLogo(data []byte, declared string) (Logo, error) {
	switch {
	case len(data) == 0:
		return Logo{}, &ValidationError{Problems: []string{"the logo is empty"}}
	case len(data) > MaxLogoBytes:
		return Logo{}, &ValidationError{Problems: []string{fmt.Sprintf("the logo is %d bytes; at most %d KiB is accepted", len(data), MaxLogoBytes>>10)}}
	}
	sniffed := http.DetectContentType(data)
	allowed := false
	for _, t := range LogoTypes {
		allowed = allowed || sniffed == t
	}
	if !allowed {
		return Logo{}, &ValidationError{Problems: []string{fmt.Sprintf("the logo must be a PNG, JPEG or GIF image; this is %s", sniffed)}}
	}
	if declared != "" && declared != sniffed {
		return Logo{}, &ValidationError{Problems: []string{fmt.Sprintf("the logo was sent as %s but is %s", declared, sniffed)}}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Logo{}, &ValidationError{Problems: []string{"the logo could not be read as an image: " + err.Error()}}
	}
	if cfg.Width > MaxLogoDimension || cfg.Height > MaxLogoDimension || cfg.Width == 0 || cfg.Height == 0 {
		return Logo{}, &ValidationError{Problems: []string{fmt.Sprintf("the logo is %dx%d; at most %dx%d is accepted",
			cfg.Width, cfg.Height, MaxLogoDimension, MaxLogoDimension)}}
	}
	sum := sha256.Sum256(data)
	return Logo{ContentType: sniffed, Data: data, SHA256: hex.EncodeToString(sum[:])}, nil
}

// PlainLine reports whether s is one line of plain text: no control or format characters.
func PlainLine(s string) bool { return printableText(s) && !bytes.ContainsAny([]byte(s), "\n\t") }
