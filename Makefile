SHELL := /bin/bash
VERSION ?= 0.3.0
LDFLAGS := -s -w -X linkgate/internal/app.Version=$(VERSION) -X main.version=$(VERSION)
TRAY_LDFLAGS := $(LDFLAGS)
ifeq ($(shell go env GOOS),windows)
TRAY_LDFLAGS += -H=windowsgui
build build-tray: windows-resources
endif

.PHONY: build build-tray icons windows-resources test vet fmt dist clean

icons:
	go run ./tools/genicons

windows-resources:
	go run ./tools/genversion -version "$(VERSION)"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linkgate ./cmd/linkgate

build-tray:
	go build -trimpath -ldflags "$(TRAY_LDFLAGS)" -o bin/linkgate-tray ./cmd/linkgate-tray

test:
	go test ./... -race -count=1

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './bin/*')

# 内核是纯 Go；macOS 托盘用 cgo，Windows 托盘可纯 Go 交叉编译。
dist: windows-resources
	mkdir -p bin/dist
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-darwin-arm64 ./cmd/linkgate
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-darwin-amd64 ./cmd/linkgate
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-windows-amd64.exe ./cmd/linkgate
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-linux-amd64 ./cmd/linkgate
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-linux-arm64 ./cmd/linkgate
	# 托盘：macOS 用 cgo；Windows 的 systray 实现是纯 Go，可从这里交叉编译
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-tray-darwin-arm64 ./cmd/linkgate-tray
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=1 CC="clang -arch x86_64" CXX="clang++ -arch x86_64" go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-tray-darwin-amd64 ./cmd/linkgate-tray
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o bin/dist/linkgate-tray-windows-amd64.exe ./cmd/linkgate-tray

clean:
	rm -rf bin dist
