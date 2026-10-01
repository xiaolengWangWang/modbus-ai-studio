// icon 生成应用图标 PNG：深青色圆角方块上一条白色方波（Modbus 通信的示意）。
// 用法：go run ./scripts/icon <输出路径> [边长]
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		os.Stderr.WriteString("用法：go run ./scripts/icon <输出路径> [边长]\n")
		os.Exit(2)
	}
	size := 1024
	if len(os.Args) > 2 {
		size, _ = strconv.Atoi(os.Args[2])
	}
	s := float64(size) / 1024
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	top, bottom := color.NRGBA{0x0E, 0x7C, 0xA8, 255}, color.NRGBA{0x06, 0x4E, 0x6C, 255}
	// macOS 图标网格：主体 824 × 824，圆角半径约 185
	body, radius := 100*s, 185*s
	for y := 0; y < size; y++ {
		t := float64(y) / float64(size)
		c := color.NRGBA{lerp(top.R, bottom.R, t), lerp(top.G, bottom.G, t), lerp(top.B, bottom.B, t), 255}
		for x := 0; x < size; x++ {
			if a := roundRectAlpha(float64(x)+0.5, float64(y)+0.5, body, float64(size)-body, radius); a > 0 {
				c.A = uint8(255 * a)
				img.SetNRGBA(x, y, c)
			}
		}
	}
	// 方波：低 – 高 – 低 – 高 – 低
	pts := [][2]float64{{230, 610}, {330, 610}, {330, 410}, {470, 410}, {470, 610}, {600, 610}, {600, 410}, {740, 410}, {740, 610}, {800, 610}}
	for i := range pts {
		pts[i][0] *= s
		pts[i][1] *= s
	}
	white := color.NRGBA{255, 255, 255, 255}
	stroke(img, pts, 46*s, white)
	// 下方三个寄存器格
	for i := 0; i < 3; i++ {
		x0 := (330 + float64(i)*140) * s
		fillRect(img, x0, 690*s, x0+100*s, 730*s, color.NRGBA{255, 255, 255, 150})
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }

// roundRectAlpha 返回点在圆角矩形内的覆盖度（边缘 1 像素抗锯齿）。
func roundRectAlpha(x, y, lo, hi, r float64) float64 {
	cx := math.Max(lo+r, math.Min(x, hi-r))
	cy := math.Max(lo+r, math.Min(y, hi-r))
	d := math.Hypot(x-cx, y-cy) - r
	if x < lo || x > hi || y < lo || y > hi {
		return 0
	}
	return math.Max(0, math.Min(1, 0.5-d))
}

func stroke(img *image.NRGBA, pts [][2]float64, width float64, c color.NRGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			d := math.Inf(1)
			for i := 0; i+1 < len(pts); i++ {
				d = math.Min(d, segDist(px, py, pts[i], pts[i+1]))
			}
			if a := math.Max(0, math.Min(1, width/2-d+0.5)); a > 0 {
				blend(img, x, y, c, a)
			}
		}
	}
}

func segDist(px, py float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := ((px-a[0])*dx + (py-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(a[0]+t*dx), py-(a[1]+t*dy))
}

func fillRect(img *image.NRGBA, x0, y0, x1, y1 float64, c color.NRGBA) {
	for y := int(y0); y < int(y1); y++ {
		for x := int(x0); x < int(x1); x++ {
			blend(img, x, y, c, float64(c.A)/255)
		}
	}
}

func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	o := img.NRGBAAt(x, y)
	mix := func(p, q uint8) uint8 { return uint8(float64(p)*(1-a) + float64(q)*a) }
	img.SetNRGBA(x, y, color.NRGBA{mix(o.R, c.R), mix(o.G, c.G), mix(o.B, c.B), uint8(math.Max(float64(o.A), 255*a))})
}
