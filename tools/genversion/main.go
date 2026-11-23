// genversion 用固定版本的 goversioninfo 为两个 Windows EXE 生成版本与图标资源。
// 在 GoReleaser before hook 和 Windows CI/本地打包中共用，不影响 Darwin/Linux 构建。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var semverPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func versionParts(version string) ([3]int, error) {
	var parts [3]int
	match := semverPattern.FindStringSubmatch(version)
	if match == nil {
		return parts, fmt.Errorf("无效发行版本: %s", version)
	}
	for i := range parts {
		n, err := strconv.Atoi(match[i+1])
		if err != nil || n > 65535 {
			return parts, fmt.Errorf("Windows 版本分量必须在 0..65535")
		}
		parts[i] = n
	}
	return parts, nil
}

func generate(root, version string) error {
	parts, err := versionParts(version)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	// FileVersion 的字符串保留 semver（含 prerelease），固定数值资源为 x.y.z.0。
	version = strings.TrimPrefix(version, "v")
	temp, err := os.MkdirTemp("", "linkgate-versioninfo-")
	if err != nil {
		return err
	}
	for _, name := range []string{"linkgate", "linkgate-tray"} {
		fixed := map[string]int{"Major": parts[0], "Minor": parts[1], "Patch": parts[2], "Build": 0}
		info := map[string]any{
			"FixedFileInfo":  map[string]any{"FileVersion": fixed, "ProductVersion": fixed, "FileFlagsMask": "3f", "FileFlags": "00", "FileOS": "040004", "FileType": "01", "FileSubType": "00"},
			"StringFileInfo": map[string]string{"CompanyName": "Linkgate", "FileDescription": name, "FileVersion": version, "ProductVersion": version, "InternalName": name, "OriginalFilename": name + ".exe", "ProductName": "Linkgate", "LegalCopyright": "MIT License"},
			"VarFileInfo":    map[string]any{"Translation": map[string]string{"LangID": "0409", "CharsetID": "04B0"}},
		}
		b, err := json.Marshal(info)
		if err != nil {
			return err
		}
		input := filepath.Join(temp, name+".json")
		if err := os.WriteFile(input, b, 0600); err != nil {
			return err
		}
		for _, arch := range []string{"amd64", "arm64"} {
			output := filepath.Join(root, "cmd", name, "resource_windows_"+arch+".syso")
			args := []string{"run", "github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.4.1", "-64", "-icon", filepath.Join(root, "cmd", "linkgate-tray", "icons", "tray-win.ico"), "-o", output}
			if arch == "arm64" {
				args = append(args, "-arm")
			}
			args = append(args, input)
			cmd := exec.Command("go", args...)
			cmd.Dir = root
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("生成 %s/%s 版本资源: %w", name, arch, err)
			}
		}
	}
	return nil
}

func main() {
	root := flag.String("root", ".", "仓库根目录")
	version := flag.String("version", "0.0.0", "发行版本")
	flag.Parse()
	if err := generate(*root, *version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
