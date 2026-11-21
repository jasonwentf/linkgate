//go:build darwin || windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"linkgate/internal/app"
)

func TestServiceSummaryIncludesActiveRoute(t *testing.T) {
	g := group{Name: "API", Active: "private", Upstreams: []upstream{
		{Name: "private", Enabled: true, Status: "healthy", LastProbeLatency: 12},
		{Name: "backup", Enabled: true, Status: "unknown"},
	}}
	if got := groupSummary(g); got != "API · private 正常 12ms" {
		t.Fatal(got)
	}
	g.Active = ""
	if !strings.Contains(groupSummary(g), "无可用线路") {
		t.Fatal(groupSummary(g))
	}
}

func TestServicesSummaryHasNoEightServiceLimit(t *testing.T) {
	var groups []group
	for i := 0; i < 20; i++ {
		groups = append(groups, group{Name: fmt.Sprintf("service-%d", i), Active: "route", Upstreams: []upstream{{Name: "route", Enabled: true, Status: "healthy"}}})
	}
	groups[19].Upstreams[0].Status = "unknown"
	groups[18].Active = ""
	groups[17].Upstreams[0].Enabled = false
	if got := servicesSummary(groups); got != "20 个服务 · 17 正常" {
		t.Fatal(got)
	}
	if got := servicesSummary(nil); got != "0 个服务 · 0 正常" {
		t.Fatal(got)
	}
}

func TestEmbeddedIconsAndWindowsConversion(t *testing.T) {
	logo, err := png.Decode(bytes.NewReader(iconLogo))
	if err != nil || logo.Bounds() != image.Rect(0, 0, 32, 32) {
		t.Fatalf("logo: %v", err)
	}
	mac, err := png.Decode(bytes.NewReader(iconMac))
	if err != nil || mac.Bounds() != image.Rect(0, 0, 44, 44) {
		t.Fatalf("mac template: %v", err)
	}
	ico, err := windowsIcon(iconLogo)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(ico[2:4]) != 1 || binary.LittleEndian.Uint16(ico[4:6]) != 1 ||
		ico[6] != 32 || ico[7] != 32 || binary.LittleEndian.Uint32(ico[18:22]) != 22 ||
		int(binary.LittleEndian.Uint32(ico[14:18])) != len(ico)-22 {
		t.Fatal("invalid ICO directory")
	}
	visible, transparent := false, false
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			c := color.NRGBAModel.Convert(logo.At(x, y)).(color.NRGBA)
			i := 22 + 40 + ((31-y)*32+x)*4
			if !bytes.Equal(ico[i:i+4], []byte{c.B, c.G, c.R, c.A}) {
				t.Fatalf("ICO pixel %d,%d differs from PNG", x, y)
			}
			mask := ico[22+40+32*32*4+(31-y)*4+x/8] & (0x80 >> uint(x%8))
			if (mask != 0) != (c.A == 0) {
				t.Fatal("incorrect ICO transparency mask")
			}
			if c.A > 0 {
				visible = true
				if c.R != 0x0f || c.G != 0x9d || c.B != 0x8f {
					t.Fatal("logo lost its accent color")
				}
			} else {
				transparent = true
			}
		}
	}
	if !visible || !transparent {
		t.Fatal("logo needs visible strokes and transparent background")
	}
	for y := 0; y < 44; y++ {
		for x := 0; x < 44; x++ {
			c := color.NRGBAModel.Convert(mac.At(x, y)).(color.NRGBA)
			if c.R != 0 || c.G != 0 || c.B != 0 {
				t.Fatal("Mac template is not monochrome")
			}
		}
	}
}

func TestWindowsIconRejectsInvalidPNG(t *testing.T) {
	if _, err := windowsIcon([]byte("not PNG")); err == nil {
		t.Fatal("accepted invalid PNG")
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 257, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := windowsIcon(buf.Bytes()); err == nil {
		t.Fatal("accepted oversized ICO")
	}
}

func TestTrayVersionDefaultAndLdflags(t *testing.T) {
	if version != "0.2.2" || version != app.Version {
		t.Fatalf("tray=%s app=%s", version, app.Version)
	}
	bin := filepath.Join(t.TempDir(), "linkgate-tray")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version=9.8.7", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	cmd = exec.Command(bin, "-version")
	// -version must exit before any config/token reads or native tray startup.
	cmd.Env = append(os.Environ(), "LINKGATE_CONFIG="+filepath.Join(t.TempDir(), "missing.yaml"))
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != "linkgate-tray 9.8.7\n" {
		t.Fatalf("version: %v %s", err, output)
	}
}
