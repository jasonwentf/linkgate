package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedIconsAreCurrent(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("go", "run", ".", "-root", root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("genicons: %v %s", err, output)
	}
	for _, name := range []string{"logo.png", "tray-mac.png", "tray-win.ico"} {
		for _, dir := range []string{"assets", "cmd/linkgate-tray/icons"} {
			want, err := os.ReadFile(filepath.Join("../..", dir, name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(root, dir, name))
			if err != nil || !bytes.Equal(want, got) {
				t.Fatalf("stale generated icon %s/%s; run make icons: %v", dir, name, err)
			}
		}
	}
	// The small rasterizer deliberately implements only this SVG's geometry.
	// A logo redesign must update both sources, not silently ship the old tray.
	svg, err := os.ReadFile("../../assets/logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	for _, attr := range []string{`viewBox="0 0 24 24"`, `stroke="#0F9D8F"`, `stroke-width="2.6"`, `d="M3 5.5C9 5.5 10 12 15 12"`, `d="M3 18.5C9 18.5 10 12 15 12"`, `d="M15 12H21"`} {
		if !strings.Contains(string(svg), attr) {
			t.Fatalf("logo.svg changed (%s); update rasterizer geometry too", attr)
		}
	}
}
