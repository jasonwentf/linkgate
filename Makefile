SHELL := /bin/bash
VERSION ?= 0.2.2
LDFLAGS := -s -w -X linkgate/internal/app.Version=$(VERSION) -X main.version=$(VERSION)

.PHONY: build build-tray icons test vet fmt dist clean

icons:
	go run ./tools/genicons

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linkgate ./cmd/linkgate

build-tray:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linkgate-tray ./cmd/linkgate-tray

test:
	go test ./... -race -count=1

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './bin/*')

# 内核是纯 Go；macOS 托盘用 cgo，Windows 托盘可纯 Go 交叉编译。
dist:
	mkdir -p bin/dist
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-darwin-arm64 ./cmd/linkgate
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-darwin-amd64 ./cmd/linkgate
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-windows-amd64.exe ./cmd/linkgate
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-linux-amd64 ./cmd/linkgate
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-linux-arm64 ./cmd/linkgate
	# 托盘：macOS 用 cgo；Windows 的 systray 实现是纯 Go，可从这里交叉编译
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-tray-darwin-arm64 ./cmd/linkgate-tray
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=1 CC="clang -arch x86_64" CXX="clang++ -arch x86_64" go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-tray-darwin-amd64 ./cmd/linkgate-tray
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dist/linkgate-tray-windows-amd64.exe ./cmd/linkgate-tray

clean:
	rm -rf bin dist
