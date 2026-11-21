//go:build darwin || windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"image/png"
)

// windowsIcon wraps the embedded PNG in a 32bpp ICO. systray v1.12.2 uses
// LoadImageW(IMAGE_ICON) on Windows and cannot load standalone PNG bytes.
func windowsIcon(data []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode tray logo: %w", err)
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w < 1 || h < 1 || w > 256 || h > 256 {
		return nil, fmt.Errorf("invalid tray logo size: %dx%d", w, h)
	}
	maskRow := ((w + 31) / 32) * 4
	entrySize := 40 + w*h*4 + maskRow*h
	ico := make([]byte, 22+entrySize)
	binary.LittleEndian.PutUint16(ico[2:4], 1)
	binary.LittleEndian.PutUint16(ico[4:6], 1)
	ico[6], ico[7] = byte(w), byte(h) // 0 encodes 256 in an ICO directory.
	binary.LittleEndian.PutUint16(ico[10:12], 1)
	binary.LittleEndian.PutUint16(ico[12:14], 32)
	binary.LittleEndian.PutUint32(ico[14:18], uint32(entrySize))
	binary.LittleEndian.PutUint32(ico[18:22], 22)
	dib := ico[22:]
	binary.LittleEndian.PutUint32(dib[0:4], 40)
	binary.LittleEndian.PutUint32(dib[4:8], uint32(w))
	binary.LittleEndian.PutUint32(dib[8:12], uint32(h*2))
	binary.LittleEndian.PutUint16(dib[12:14], 1)
	binary.LittleEndian.PutUint16(dib[14:16], 32)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			i := 40 + ((h-1-y)*w+x)*4
			copy(dib[i:i+4], []byte{c.B, c.G, c.R, c.A})
			if c.A == 0 {
				// Keep transparent pixels transparent for legacy mask readers too.
				mask := 40 + w*h*4 + (h-1-y)*maskRow + x/8
				dib[mask] |= 0x80 >> uint(x%8)
			}
		}
	}
	return ico, nil
}
