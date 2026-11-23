package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestVersionParts(t *testing.T) {
	for _, v := range []string{"1.2.3", "v1.2.3", "1.2.3-rc.1+build"} {
		p, err := versionParts(v)
		if err != nil || p != [3]int{1, 2, 3} {
			t.Fatalf("%s: %v %v", v, p, err)
		}
	}
	for _, v := range []string{"dev", "1.2", "01.2.3", "65536.0.0"} {
		if _, err := versionParts(v); err == nil {
			t.Fatalf("accepted %s", v)
		}
	}
}

// 可在 macOS 上核验交叉编译产物；不会尝试运行 Windows EXE。
func TestWindowsExecutables(t *testing.T) {
	dir := os.Getenv("LINKGATE_WINDOWS_BINARY_DIR")
	if dir == "" {
		t.Skip("设置 LINKGATE_WINDOWS_BINARY_DIR 后检查 Windows PE 与资源")
	}
	version := os.Getenv("LINKGATE_RELEASE_VERSION")
	parts, err := versionParts(version)
	if err != nil {
		t.Fatal(err)
	}
	utf16Bytes := func(s string) []byte {
		chars := utf16.Encode([]rune(s))
		b := make([]byte, len(chars)*2)
		for i, c := range chars {
			binary.LittleEndian.PutUint16(b[i*2:], c)
		}
		return b
	}
	for _, name := range []string{"linkgate", "linkgate-tray"} {
		t.Run(name, func(t *testing.T) {
			file, err := pe.Open(filepath.Join(dir, name+".exe"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			header, ok := file.OptionalHeader.(*pe.OptionalHeader64)
			if !ok || file.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
				t.Fatal("not Windows x64")
			}
			wantSubsystem := uint16(pe.IMAGE_SUBSYSTEM_WINDOWS_CUI)
			if name == "linkgate-tray" {
				wantSubsystem = pe.IMAGE_SUBSYSTEM_WINDOWS_GUI
			}
			if header.Subsystem != wantSubsystem {
				t.Fatalf("subsystem=%d want=%d", header.Subsystem, wantSubsystem)
			}
			if header.DataDirectory[2].Size == 0 {
				t.Fatal("missing Windows resources")
			}
			section := file.Section(".rsrc")
			if section == nil {
				t.Fatal("missing .rsrc section")
			}
			data, err := section.Data()
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"FileVersion", "ProductVersion", strings.TrimPrefix(version, "v"), name + ".exe"} {
				if !bytes.Contains(data, utf16Bytes(value+"\x00")) {
					t.Fatalf("resource missing %s", value)
				}
			}
			// VS_FIXEDFILEINFO：签名后依次为 struct version、file MS/LS、product MS/LS。
			i := bytes.Index(data, []byte{0xbd, 0x04, 0xef, 0xfe})
			if i < 0 || i+24 > len(data) {
				t.Fatal("missing fixed version")
			}
			ms := uint32(parts[0])<<16 | uint32(parts[1])
			ls := uint32(parts[2]) << 16
			for _, offset := range []int{8, 16} {
				if binary.LittleEndian.Uint32(data[i+offset:]) != ms || binary.LittleEndian.Uint32(data[i+offset+4:]) != ls {
					t.Fatal("wrong numeric FileVersion/ProductVersion")
				}
			}
		})
	}
}
