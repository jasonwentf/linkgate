// genicons 用超采样光栅化生成托盘图标，几何与 assets/logo.svg 一致。
//
// 在仓库根目录运行：go run ./tools/genicons
//
// 输出（结果确定，可重复生成）：
//
//	assets/logo.png                     32x32 彩色 logo，Windows 运行时转成 ICO
//	assets/tray-mac.png                 44x44（22pt @2x）黑色 + 透明，macOS template image
//	assets/tray-win.ico                 16/32/48，强调色，32bpp BMP 条目
//	cmd/linkgate-tray/icons/*           同上三份的副本，供 go:embed 使用
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// 设计坐标系为 24x24，与 logo.svg 的 viewBox 相同。
const (
	design      = 24.0
	strokeWidth = 2.6
	supersample = 8 // 每像素 8x8 子采样
)

type pt struct{ x, y float64 }
type seg struct{ a, b pt }

// shape 返回描边中心线（折线化后的线段集合）。
func shape() []seg {
	var out []seg
	out = append(out, cubic(pt{3, 5.5}, pt{9, 5.5}, pt{10, 12}, pt{15, 12}, 48)...)
	out = append(out, cubic(pt{3, 18.5}, pt{9, 18.5}, pt{10, 12}, pt{15, 12}, 48)...)
	out = append(out, seg{pt{15, 12}, pt{21, 12}})
	return out
}

func cubic(p0, p1, p2, p3 pt, steps int) []seg {
	at := func(t float64) pt {
		u := 1 - t
		return pt{
			u*u*u*p0.x + 3*u*u*t*p1.x + 3*u*t*t*p2.x + t*t*t*p3.x,
			u*u*u*p0.y + 3*u*u*t*p1.y + 3*u*t*t*p2.y + t*t*t*p3.y,
		}
	}
	out := make([]seg, 0, steps)
	prev := p0
	for i := 1; i <= steps; i++ {
		cur := at(float64(i) / float64(steps))
		out = append(out, seg{prev, cur})
		prev = cur
	}
	return out
}

func distSeg(p pt, s seg) float64 {
	dx, dy := s.b.x-s.a.x, s.b.y-s.a.y
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = ((p.x-s.a.x)*dx + (p.y-s.a.y)*dy) / l2
		t = math.Max(0, math.Min(1, t))
	}
	ex, ey := p.x-(s.a.x+t*dx), p.y-(s.a.y+t*dy)
	return math.Sqrt(ex*ex + ey*ey)
}

// render 返回 size x size 的覆盖率图（0..1）。圆头描边由“到中心线距离 < 半线宽”自然得到。
func render(size int, segs []seg) [][]float64 {
	half := strokeWidth / 2
	scale := design / float64(size)
	cov := make([][]float64, size)
	for y := 0; y < size; y++ {
		cov[y] = make([]float64, size)
		for x := 0; x < size; x++ {
			hit := 0
			for sy := 0; sy < supersample; sy++ {
				for sx := 0; sx < supersample; sx++ {
					p := pt{
						(float64(x) + (float64(sx)+0.5)/supersample) * scale,
						(float64(y) + (float64(sy)+0.5)/supersample) * scale,
					}
					for _, s := range segs {
						if distSeg(p, s) <= half {
							hit++
							break
						}
					}
				}
			}
			cov[y][x] = float64(hit) / (supersample * supersample)
		}
	}
	return cov
}

func toNRGBA(cov [][]float64, c color.NRGBA) *image.NRGBA {
	n := len(cov)
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			a := uint8(math.Round(cov[y][x] * float64(c.A)))
			if a == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, a})
		}
	}
	return img
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// bmpEntry 生成 ICO 内的 32bpp DIB 条目（BITMAPINFOHEADER + 自底向上 BGRA + AND 掩码）。
// 用 BMP 而非 PNG 条目，兼容 LoadImage 的所有路径。
func bmpEntry(img *image.NRGBA) []byte {
	n := img.Bounds().Dx()
	var buf bytes.Buffer
	hdr := struct {
		Size          uint32
		Width, Height int32
		Planes, Bpp   uint16
		Compression   uint32
		ImageSize     uint32
		XPPM, YPPM    int32
		ClrUsed       uint32
		ClrImportant  uint32
	}{40, int32(n), int32(n * 2), 1, 32, 0, 0, 0, 0, 0, 0}
	_ = binary.Write(&buf, binary.LittleEndian, hdr)
	for y := n - 1; y >= 0; y-- {
		for x := 0; x < n; x++ {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	rowBytes := ((n + 31) / 32) * 4
	buf.Write(make([]byte, rowBytes*n)) // AND 掩码全 0，透明度由 alpha 通道决定
	return buf.Bytes()
}

func encodeICO(imgs []*image.NRGBA) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, []uint16{0, 1, uint16(len(imgs))})
	entries := make([][]byte, len(imgs))
	offset := 6 + 16*len(imgs)
	for i, img := range imgs {
		entries[i] = bmpEntry(img)
		n := img.Bounds().Dx()
		dim := byte(n)
		if n >= 256 {
			dim = 0
		}
		out.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&out, binary.LittleEndian, []uint16{1, 32})
		_ = binary.Write(&out, binary.LittleEndian, []uint32{uint32(len(entries[i])), uint32(offset)})
		offset += len(entries[i])
	}
	for _, e := range entries {
		out.Write(e)
	}
	return out.Bytes()
}

func writeAll(name string, data []byte, dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", p, len(data))
	}
	return nil
}

func main() {
	root := flag.String("root", ".", "仓库根目录")
	flag.Parse()

	assetsDir := filepath.Join(*root, "assets")
	embedDir := filepath.Join(*root, "cmd", "linkgate-tray", "icons")
	segs := shape()

	// macOS：template image 只看 alpha，颜色用纯黑。
	mac := toNRGBA(render(44, segs), color.NRGBA{0, 0, 0, 255})
	macPNG, err := encodePNG(mac)
	if err != nil {
		fail(err)
	}
	if err := writeAll("tray-mac.png", macPNG, assetsDir, embedDir); err != nil {
		fail(err)
	}

	// Windows：强调色在深浅任务栏上都可辨认。
	accent := color.NRGBA{0x0F, 0x9D, 0x8F, 255}
	logoPNG, err := encodePNG(toNRGBA(render(32, segs), accent))
	if err != nil {
		fail(err)
	}
	if err := writeAll("logo.png", logoPNG, assetsDir, embedDir); err != nil {
		fail(err)
	}
	var winImgs []*image.NRGBA
	for _, s := range []int{16, 32, 48} {
		winImgs = append(winImgs, toNRGBA(render(s, segs), accent))
	}
	if err := writeAll("tray-win.ico", encodeICO(winImgs), assetsDir, embedDir); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genicons:", err)
	os.Exit(1)
}
