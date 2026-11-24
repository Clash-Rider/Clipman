package ui

import (
	"image"
	"image/color"

	// "gioui.org/font/gofont"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// Tokyo Night color palette (self-contained, independent of GTK/GNOME themes).
var (
	ColorBg          = rgb(0x1a, 0x1b, 0x26)        // Primary window background
	ColorBgSecondary = rgb(0x24, 0x28, 0x3b)        // Secondary panels, inputs, cards
	ColorSurface     = rgb(0x29, 0x2e, 0x42)        // Selected / hover surface
	ColorBorder      = rgb(0x3b, 0x42, 0x61)        // Subtle borders and separators
	ColorTextPrimary = rgb(0xc0, 0xca, 0xf5)        // Primary text
	ColorTextMuted   = rgba(0xa9, 0xb1, 0xd6, 0x3c) // Secondary / muted text
	ColorBlue        = rgb(0x7a, 0xa2, 0xf7)        // Primary accent
	ColorCyan        = rgb(0x7d, 0xcf, 0xff)        // Info / binary indicator
	ColorPurple      = rgb(0xbb, 0x9a, 0xf7)        // Pinned accent
	ColorGreen       = rgba(0x9e, 0xce, 0x6a, 0x9c) // Success status
	ColorRed         = rgb(0xf7, 0x76, 0x8e)        // Error / delete accent
)

func rgb(r, g, b uint8) color.NRGBA {
	return color.NRGBA{R: r, G: g, B: b, A: 0xff}
}

func rgba(r, g, b, a uint8) color.NRGBA {
	return color.NRGBA{R: r, G: g, B: b, A: a}
}

// NewTokyoNightTheme returns a Gio material.Theme configured with Tokyo Night colors.
func NewTokyoNightTheme() *material.Theme {
	th := material.NewTheme()
	// th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))

	th.Shaper = text.NewShaper()
	th.Face = font.Typeface("Maple Mono NF")

	th.Palette = material.Palette{
		Bg:         ColorBg,
		Fg:         ColorTextPrimary,
		ContrastBg: ColorBlue,
		ContrastFg: ColorBg,
	}
	th.TextSize = unit.Sp(15)
	return th
}

// fillRect paints a solid rectangle filling gtx.Constraints.Max.
func fillRect(gtx layout.Context, col color.NRGBA) {
	paint.FillShape(gtx.Ops, col, clip.Rect{Max: gtx.Constraints.Max}.Op())
}

// fillRoundedBox draws a rounded background behind widget w.
func fillRoundedBox(gtx layout.Context, bg color.NRGBA, radius unit.Dp, w layout.Widget) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			r := gtx.Dp(radius)
			rect := image.Rectangle{Max: gtx.Constraints.Min}
			paint.FillShape(gtx.Ops, bg, clip.UniformRRect(rect, r).Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(w),
	)
}

// horizontalDivider draws a 1dp subtle horizontal separator.
func horizontalDivider(gtx layout.Context) layout.Dimensions {
	h := gtx.Dp(unit.Dp(1))
	if h < 1 {
		h = 1
	}
	size := image.Pt(gtx.Constraints.Max.X, h)
	paint.FillShape(gtx.Ops, ColorBorder, clip.Rect{Max: size}.Op())
	return layout.Dimensions{Size: size}
}
