// Package imageproc implements the photo processing pipeline that mimics
// the "camera LCD look" — slightly brighter, punchier contrast, more vivid
// colors — plus optional dreamy / film / landscape filters.
package imageproc

// Params holds all adjustable processing parameters.
// Ranges are roughly -100..100 (or 0..100 for one-sided effects).
type Params struct {
	Brightness float64 `json:"brightness"` // 亮度：加法提亮（模拟LCD背光，黑场同步抬起）
	Contrast   float64 `json:"contrast"`   // 对比度：围绕中灰的缩放
	Saturation float64 `json:"saturation"` // 饱和度：YCbCr色度缩放
	Vibrance   float64 `json:"vibrance"`   // 鲜艳度：优先增强低饱和区域（保护肤色）
	Warmth     float64 `json:"warmth"`     // 色温：>0偏暖 <0偏冷
	Clarity    float64 `json:"clarity"`    // 清晰度：局部对比度（unsharp mask）
	Glow       float64 `json:"glow"`       // 梦幻柔焦：高斯模糊+Screen混合 (0..100)
	Fade       float64 `json:"fade"`       // 胶片褪色：黑场提升 (0..100)
	Grain      float64 `json:"grain"`      // 颗粒感 (0..100)
	Vignette   float64 `json:"vignette"`   // 暗角 (0..100)
}

// Preset is a named combination of parameters shown in the UI.
type Preset struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Params Params `json:"params"`
}

// DefaultPreset returns the built-in presets. The first one is the flagship
// "camera screen restore" look.
func DefaultPresets() []Preset {
	return []Preset{
		{
			Key:  "lcd",
			Name: "相机屏显还原",
			Desc: "模拟相机LCD观感：整体提亮、对比鲜明、色彩鲜活，还原拍下那一刻屏幕上的惊艳",
			Params: Params{
				Brightness: 16,
				Contrast:   24,
				Saturation: 30,
				Vibrance:   14,
				Warmth:     0,
				Clarity:    14,
				Glow:       0,
				Fade:       0,
				Grain:      0,
				Vignette:   0,
			},
		},
		{
			Key:  "dreamy",
			Name: "梦幻柔焦",
			Desc: "柔光弥散、光晕梦幻，适合人像、花、夜景",
			Params: Params{
				Brightness: 10,
				Contrast:   6,
				Saturation: 10,
				Vibrance:   8,
				Warmth:     3,
				Clarity:    -2,
				Glow:       40,
				Fade:       6,
				Grain:      0,
				Vignette:   8,
			},
		},
		{
			Key:  "japan",
			Name: "日系清新",
			Desc: "明亮通透、低饱和微冷，干净的日系空气感",
			Params: Params{
				Brightness: 16,
				Contrast:   4,
				Saturation: 6,
				Vibrance:   10,
				Warmth:     -6,
				Clarity:    2,
				Glow:       10,
				Fade:       8,
				Grain:      2,
				Vignette:   0,
			},
		},
		{
			Key:  "film",
			Name: "复古胶片",
			Desc: "提升黑场、暖调、颗粒感，柯达风格的老照片味道",
			Params: Params{
				Brightness: 4,
				Contrast:   -6,
				Saturation: -12,
				Vibrance:   4,
				Warmth:     14,
				Clarity:    -4,
				Glow:       4,
				Fade:       18,
				Grain:      16,
				Vignette:   22,
			},
		},
		{
			Key:  "landscape",
			Name: "风光鲜亮",
			Desc: "高对比、高饱和、清晰通透，风光大片的机内风景模式加强版",
			Params: Params{
				Brightness: 6,
				Contrast:   22,
				Saturation: 30,
				Vibrance:   18,
				Warmth:     4,
				Clarity:    20,
				Glow:       0,
				Fade:       0,
				Grain:      0,
				Vignette:   10,
			},
		},
		{
			Key:  "portrait",
			Name: "人像柔肤",
			Desc: "肤色柔和偏暖、轻微柔光，人像出片更讨喜",
			Params: Params{
				Brightness: 8,
				Contrast:   5,
				Saturation: -4,
				Vibrance:   10,
				Warmth:     8,
				Clarity:    -3,
				Glow:       18,
				Fade:       4,
				Grain:      0,
				Vignette:   4,
			},
		},
		{
			Key:  "golden",
			Name: "黄昏暖阳",
			Desc: "日落金辉：暖调浸染、暗角收拢，黄昏时刻的氛围感",
			Params: Params{
				Brightness: 4,
				Contrast:   10,
				Saturation: 16,
				Vibrance:   14,
				Warmth:     18,
				Clarity:    0,
				Glow:       6,
				Fade:       8,
				Grain:      4,
				Vignette:   20,
			},
		},
		{
			Key:  "cinema",
			Name: "电影感",
			Desc: "冷调青灰、强对比、颗粒与暗角，宽银幕电影画面质感",
			Params: Params{
				Brightness: -2,
				Contrast:   20,
				Saturation: -10,
				Vibrance:   6,
				Warmth:     -8,
				Clarity:    8,
				Glow:       0,
				Fade:       8,
				Grain:      10,
				Vignette:   26,
			},
		},
		{
			Key:  "bw",
			Name: "经典黑白",
			Desc: "银盐质感：去色、强对比、颗粒与暗角，经典黑白胶片味道",
			Params: Params{
				Brightness: 2,
				Contrast:   18,
				Saturation: -100,
				Vibrance:   0,
				Warmth:     0,
				Clarity:    10,
				Glow:       0,
				Fade:       6,
				Grain:      18,
				Vignette:   24,
			},
		},
		{
			Key:  "morning",
			Name: "清晨薄雾",
			Desc: "清透柔和：提亮、低对比、微冷与淡淡柔光，清晨的空气感",
			Params: Params{
				Brightness: 14,
				Contrast:   2,
				Saturation: 8,
				Vibrance:   6,
				Warmth:     -10,
				Clarity:    -6,
				Glow:       14,
				Fade:       10,
				Grain:      0,
				Vignette:   0,
			},
		},
	}
}
