package imageproc

import (
	"image"
	"image/color"
	"math"
	"runtime"
	"sync"

	"github.com/disintegration/imaging"
)

// ---------------------------------------------------------------------------
// Processing pipeline (single source of truth for the math).
// The JS preview in web/app.js implements the exact same formulas so that the
// on-screen preview matches the server-rendered result.
//
// Order of operations:
//   1. clarity   : unsharp mask (detail = src - blur; out = src + detail*k)
//   2. base      : contrast -> brightness -> saturation/vibrance -> warmth
//                  (done in YCbCr, then converted back to RGB)
//   3. glow      : gaussian blur + screen blend (dreamy soft focus)
//   4. fade      : lifted blacks (film look)
//   5. grain     : deterministic per-pixel hash noise
//   6. vignette  : radial darkening
// ---------------------------------------------------------------------------

const (
	clarityRadiusScale = 1000.0 // sigma = maxEdge / this
	glowRadiusScale    = 250.0  // glow blur radius scale (resized internally)
)

type nrgba struct {
	img *image.NRGBA
	w   int
	h   int
}

func cloneNRGBA(src image.Image) *nrgba {
	img := imaging.Clone(src)
	b := img.Bounds()
	return &nrgba{img: img, w: b.Dx(), h: b.Dy()}
}

// Apply runs the full pipeline on src. Returns a new NRGBA image.
func Apply(src image.Image, p Params) *image.NRGBA {
	n := cloneNRGBA(src)
	if n.w == 0 || n.h == 0 {
		return n.img
	}
	maxEdge := float64(n.w)
	if n.h > n.w {
		maxEdge = float64(n.h)
	}

	// 1. Clarity (unsharp mask)
	if p.Clarity != 0 {
		sigma := clampFloat(maxEdge/clarityRadiusScale, 1.0, 6.0)
		blur := imaging.Blur(n.img, sigma)
		k := p.Clarity / 100.0 * 0.9
		applyIndexed(n, func(x, y int, r, g, b uint8) (uint8, uint8, uint8) {
			br, bg, bb, _ := nrgbaAt(blur, x, y)
			return clampU8(float64(r) + (float64(r)-float64(br))*k),
				clampU8(float64(g) + (float64(g)-float64(bg))*k),
				clampU8(float64(b) + (float64(b)-float64(bb))*k)
		})
	}

	// 2. Base color: contrast -> brightness -> saturation/vibrance -> warmth
	bright := p.Brightness * 2.55
	cf := (100.0 + p.Contrast) / 100.0
	sf := (100.0 + p.Saturation) / 100.0
	vf0 := p.Vibrance / 100.0 * 0.6
	warm := p.Warmth * 0.0009
	if bright != 0 || cf != 1 || sf != 1 || vf0 != 0 || warm != 0 {
		applyIndexed(n, func(x, y int, r, g, b uint8) (uint8, uint8, uint8) {
			yy, cb, cr := rgb2ycbcr(r, g, b)
			// contrast around neutral, then additive brightness (LCD feel)
			yf := (float64(yy)-128.0)*cf + 128.0 + bright
			// saturation with vibrance: low-saturation pixels get a bigger boost
			sat := math.Sqrt((float64(cb)-128.0)*(float64(cb)-128.0)+(float64(cr)-128.0)*(float64(cr)-128.0)) / 127.0
			if sat > 1 {
				sat = 1
			}
			f := sf * (1 + vf0*(1-sat))
			cb2 := 128.0 + (float64(cb)-128.0)*f
			cr2 := 128.0 + (float64(cr)-128.0)*f
			nr, ng, nb := ycbcr2rgb(clampF(yf), clampF(cb2), clampF(cr2))
			// warmth: warm shifts R up, B down
			if warm != 0 {
				nr = clampU8(float64(nr) * (1 + warm))
				nb = clampU8(float64(nb) * (1 - warm))
			}
			return nr, ng, nb
		})
	}

	// 3. Dreamy glow (screen blend)
	if p.Glow > 0 {
		gg := p.Glow / 100.0 * 0.9
		blur := blurredLarge(n.img, maxEdge)
		applyIndexed(n, func(x, y int, r, g, b uint8) (uint8, uint8, uint8) {
			br, bg, bb, _ := nrgbaAt(blur, x, y)
			sr := 255 - (255-float64(r))*(255-float64(br))/255.0
			sg := 255 - (255-float64(g))*(255-float64(bg))/255.0
			sb := 255 - (255-float64(b))*(255-float64(bb))/255.0
			return clampU8(float64(r)*(1-gg) + sr*gg),
				clampU8(float64(g)*(1-gg) + sg*gg),
				clampU8(float64(b)*(1-gg) + sb*gg)
		})
	}

	// 4+5+6. Fade, grain, vignette (single pass, all per-pixel)
	if p.Fade > 0 || p.Grain > 0 || p.Vignette > 0 {
		f := p.Fade / 100.0
		grainAmp := p.Grain / 100.0 * 22.0
		v := p.Vignette / 100.0
		invMaxR2 := 1.0 / (float64(n.w*n.w+n.h*n.h) / 4.0)
		seed := uint32(0x9E3779B9)
		applyIndexed(n, func(x, y int, r, g, b uint8) (uint8, uint8, uint8) {
			fr, fg, fb := float64(r), float64(g), float64(b)
			if f > 0 {
				fr = fr*(1-f) + 26*f
				fg = fg*(1-f) + 26*f
				fb = fb*(1-f) + 26*f
			}
			if grainAmp > 0 {
				gv := hashNoise(float64(x), float64(y), seed) * grainAmp
				fr += gv
				fg += gv
				fb += gv
			}
			if v > 0 {
				dx := float64(x) - float64(n.w-1)/2
				dy := float64(y) - float64(n.h-1)/2
				d2 := (dx*dx + dy*dy) * invMaxR2
				vf := 1 - v*0.32*d2*d2
				fr *= vf
				fg *= vf
				fb *= vf
			}
			return clampU8(fr), clampU8(fg), clampU8(fb)
		})
	}

	return n.img
}

// blurredLarge produces a gaussian blur equivalent to a large sigma by
// downscaling, blurring and upscaling — much faster than a direct big-kernel blur.
func blurredLarge(img *image.NRGBA, maxEdge float64) *image.NRGBA {
	sigma := clampFloat(maxEdge/glowRadiusScale, 3.0, 60.0)
	scale := 0.25
	small := imaging.Resize(img, int(float64(img.Bounds().Dx())*scale), int(float64(img.Bounds().Dy())*scale), imaging.Lanczos)
	small = imaging.Blur(small, sigma)
	return imaging.Resize(small, img.Bounds().Dx(), img.Bounds().Dy(), imaging.Lanczos)
}

// ---------------------------------------------------------------------------
// helpers

func applyIndexed(n *nrgba, fn func(x, y int, r, g, b uint8) (uint8, uint8, uint8)) {
	pix := n.img.Pix
	stride := n.w * 4
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	rows := make(chan int, n.h)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := range rows {
				base := y * stride
				for x := 0; x < n.w; x++ {
					i := base + x*4
					r, g, b, a := pix[i], pix[i+1], pix[i+2], pix[i+3]
					nr, ng, nb := fn(x, y, r, g, b)
					pix[i], pix[i+1], pix[i+2], pix[i+3] = nr, ng, nb, a
				}
			}
		}()
	}
	for y := 0; y < n.h; y++ {
		rows <- y
	}
	close(rows)
	wg.Wait()
}

func nrgbaAt(img *image.NRGBA, x, y int) (uint8, uint8, uint8, uint8) {
	i := y*img.Stride + x*4
	return img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func clampU8(v float64) uint8 {
	return uint8(clampF(v) + 0.5)
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func rgb2ycbcr(r, g, b uint8) (uint8, uint8, uint8) {
	// stdlib conversion (ITU-R BT.601)
	return color.RGBToYCbCr(r, g, b)
}

func ycbcr2rgb(y, cb, cr float64) (uint8, uint8, uint8) {
	return color.YCbCrToRGB(clampU8(y), clampU8(cb), clampU8(cr))
}

// hashNoise returns deterministic pseudo-random noise in [-1, 1] for (x, y).
func hashNoise(x, y float64, seed uint32) float64 {
	n := math.Sin(x*127.1 + y*311.7 + float64(seed)*0.5) * 43758.5453123
	n = n - math.Floor(n)
	return n*2 - 1
}
