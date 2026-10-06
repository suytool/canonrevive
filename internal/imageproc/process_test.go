package imageproc

import (
	"image"
	"image/color"
	"testing"
)

func grayImg(v uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		if i%4 == 3 {
			img.Pix[i] = 255
		} else {
			img.Pix[i] = v
		}
	}
	return img
}

func px(img image.Image, x, y int) (uint8, uint8, uint8) {
	r, g, b, _ := img.At(x, y).RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
}

func TestBrightness(t *testing.T) {
	out := Apply(grayImg(100), Params{Brightness: 20})
	r, g, b := px(out, 0, 0)
	if r <= 100 || g <= 100 || b <= 100 {
		t.Fatalf("brightness+20 should raise 100; got r=%d g=%d b=%d", r, g, b)
	}
}

func TestContrast(t *testing.T) {
	// pivot stays, darker gets darker, brighter gets brighter
	outMid := Apply(grayImg(128), Params{Contrast: 30})
	rm, _, _ := px(outMid, 0, 0)
	if rm < 125 || rm > 131 {
		t.Fatalf("mid gray should stay ~128; got %d", rm)
	}
	outDark := Apply(grayImg(90), Params{Contrast: 30})
	rd, _, _ := px(outDark, 0, 0)
	if rd >= 90 {
		t.Fatalf("dark gray should get darker; got %d", rd)
	}
	outLight := Apply(grayImg(180), Params{Contrast: 30})
	rl, _, _ := px(outLight, 0, 0)
	if rl <= 180 {
		t.Fatalf("light gray should get brighter; got %d", rl)
	}
}

func TestSaturation(t *testing.T) {
	// pure red: boosting saturation keeps red dominance
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 220, G: 60, B: 40, A: 255})
		}
	}
	out := Apply(img, Params{Saturation: 40})
	r, g, b := px(out, 0, 0)
	if r-g < 220-60 || r-b < 220-40 {
		t.Fatalf("saturation+40 should widen red dominance; got r=%d g=%d b=%d", r, g, b)
	}
}

func TestFadeLiftsBlacks(t *testing.T) {
	out := Apply(grayImg(0), Params{Fade: 50})
	r, _, _ := px(out, 0, 0)
	if r < 10 || r > 15 {
		t.Fatalf("fade 50 should lift black to ~13; got %d", r)
	}
}

func TestPresetLCDRaisesOverall(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			v := uint8(60 + x*20 + y*8)
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v - 5, B: v + 10, A: 255})
		}
	}
	before := avgLuma(img)
	out := Apply(img, DefaultPresets()[0].Params) // 相机屏显还原
	after := avgLuma(out)
	if after <= before {
		t.Fatalf("LCD preset should raise overall luma; before=%.1f after=%.1f", before, after)
	}
}

func avgLuma(img image.Image) float64 {
	b := img.Bounds()
	var sum float64
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, _ := img.At(x, y).RGBA()
			sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(bb>>8)
			n++
		}
	}
	return sum / float64(n)
}
