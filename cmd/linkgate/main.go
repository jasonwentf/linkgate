package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"linkgate/internal/app"
)

var (
	version = app.Version
	commit  = "dev"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "linkgate:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", app.DefaultPath(), "配置文件路径（默认 ~/.linkgate/config.yaml）")
	checkOnly := flag.Bool("check", false, "只校验配置，然后退出")
	doctor := flag.Bool("doctor", false, "逐条探测线路并打印结果，然后退出")
	initConfig := flag.Bool("init", false, "在配置文件路径写入一份示例配置")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	install := flag.Bool("install", false, "安装当前用户的后台内核与托盘启动项")
	uninstall := flag.Bool("uninstall", false, "卸载当前安装的启动项（保留配置与日志）")
	flag.BoolVar(&windowsInstall.autostart, "install-autostart", true, "安装时启用登录自启动")
	flag.BoolVar(&windowsInstall.tray, "install-tray", true, "安装时创建托盘启动项")
	flag.BoolVar(&windowsInstall.start, "install-start", true, "安装后立即启动内核与托盘")
	flag.BoolVar(&windowsInstall.dryrun, "dryrun", false, "只打印 Windows 安装计划，不修改系统")
	flag.BoolVar(&windowsInstall.openPanel, "install-open-panel", false, "安装启动后打开面板")
	flag.Parse()

	if *install && *uninstall {
		return fmt.Errorf("-install 与 -uninstall 不能同时使用")
	}
	if *install || *uninstall {
		return runInstall(*configPath, *uninstall)
	}

	if *showVersion {
		fmt.Printf("linkgate %s (%s)\n", version, commit)
		return nil
	}

	if *initConfig {
		return writeExample(*configPath)
	}

	cfg, err := app.Load(*configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("找不到配置文件 %s\n提示：先运行 linkgate -init 生成示例配置，再按你的线路改写", *configPath)
		}
		return err
	}

	if *checkOnly {
		fmt.Printf("配置有效：%d 个线路组，%d 条线路\n", len(cfg.Groups), countUpstreams(cfg))
		return nil
	}

	logger, closer, err := app.NewLogger(cfg)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}

	events := app.NewEventLog(cfg.Log.RingSize)
	th := thresholds(cfg)
	store := app.NewStore(cfg, th)
	monitor := app.NewHealthMonitor(cfg, store, events, logger)

	if *doctor {
		return runDoctor(cfg, monitor)
	}

	engine, err := app.NewEngine(cfg, store, events, logger)
	if err != nil {
		return err
	}
	defer engine.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager, err := app.NewConfigManager(*configPath, cfg, engine, store, monitor, events, logger)
	if err != nil {
		return err
	}
	admin := app.NewAdminServer(cfg, engine, store, events, *configPath, logger)
	admin.SetProber(monitor)
	admin.SetVersion(version)
	manager.SetAdmin(admin)
	if err := manager.Start(); err != nil {
		return err
	}
	defer manager.Close()
	go monitor.Start(ctx)
	defer monitor.Stop()

	events.Add(app.Event{Level: "info", Kind: "start", Message: fmt.Sprintf("Linkgate %s 已启动", version)})
	printBanner(cfg, *configPath)

	// 热重载：SIGHUP（Windows 下由 watchSignals 处理为空）
	hup := make(chan os.Signal, 1)
	watchHangup(hup)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-hup:
			if err := manager.Reload(); err != nil {
				logger.Warn("reload rejected; previous configuration retained", "err", err)
				events.Add(app.Event{Level: "warn", Kind: "config", Message: "重载失败，保留旧配置: " + err.Error()})
				continue
			}
			logger.Info("config reloaded")
		case sig := <-stop:
			logger.Info("shutting down", "signal", sig.String())
			return nil
		}
	}
}

// runDoctor 逐条探测线路，输出人可读的结果。排障时单独跑一次即可，不启动服务。
func runDoctor(cfg *app.Config, monitor *app.HealthMonitor) error {
	fmt.Printf("逐条探测 %d 条线路（超时 %s）…\n\n", countUpstreams(cfg), cfg.Health.Timeout.Duration)
	failures := 0
	for _, g := range cfg.Groups {
		fmt.Printf("[%s]  %s\n", g.Name, g.Listen)
		for _, u := range g.Upstreams {
			ok, latency, msg := monitor.ProbeNow(g.Name, u.Name)
			mark := "OK  "
			if !ok {
				mark = "FAIL"
				failures++
			}
			if msg == "" {
				msg = "探针成功"
			}
			fmt.Printf("  %-4s %-14s %7dms  %s\n", mark, u.Name, latency.Milliseconds(), msg)
		}
		fmt.Println()
	}
	if failures > 0 {
		return fmt.Errorf("%d 条线路探测失败", failures)
	}
	fmt.Println("全部线路可达。")
	return nil
}

func thresholds(cfg *app.Config) app.Thresholds {
	return app.Thresholds{
		FailThreshold:    cfg.Health.FailThreshold,
		RecoverThreshold: cfg.Health.RecoverThreshold,
	}
}

func countUpstreams(cfg *app.Config) int {
	n := 0
	for _, g := range cfg.Groups {
		n += len(g.Upstreams)
	}
	return n
}

func printBanner(cfg *app.Config, configPath string) {
	var lines []string
	for _, g := range cfg.Groups {
		var names []string
		for _, u := range g.Upstreams {
			names = append(names, u.Name)
		}
		sort.Strings(names)
		lines = append(lines, fmt.Sprintf("  %-10s %-18s %s", g.Name, g.Listen, strings.Join(names, " → ")))
	}
	fmt.Printf("Linkgate %s\n配置：%s\n线路：\n%s\n", version, configPath, strings.Join(lines, "\n"))
	if !cfg.Admin.Disabled {
		fmt.Printf("面板：http://%s/__linkgate/ui\n", cfg.Admin.Listen)
	}
	fmt.Println("按 Ctrl-C 停止")
}

func writeExample(path string) error {
	example := `# Linkgate 初始配置：先从面板添加你自己的服务与线路。
# 下方注释中的 TEST-NET 地址仅为文档占位，不会被探测或转发。
version: 1

admin:
  listen: 127.0.0.1:8766

log:
  level: info
  format: text
  file: ~/.linkgate/kernel.log

# 请求体常达数 MB：小于 8MiB 放内存，超过落临时文件
buffer:
  max_request_bytes: 134217728
  spill_threshold: 8388608
  max_concurrent: 64
  global_max_concurrent: 64

http:
  connect_timeout: 2s
  tls_handshake_timeout: 5s
  response_header_timeout: 900s   # 模型可能长时间思考，首字节要放宽
  stream_idle_timeout: 900s       # 流中途多久没数据算卡死
  upload_idle_timeout: 60s        # 上传请求体多久没有进展算卡死
  keep_alive: 30s

retry:
  max_attempts: 3
  mode: connect                   # 只在连接未建立时换线路，避免重复计费

health:
  enabled: true
  interval: 15s
  timeout: 3s
  fail_threshold: 2
  recover_threshold: 2
  probe_path: /health

groups: []
# 添加服务示例（取消注释并替换 URL 后再使用）：
# groups:
#   - name: demo
#     listen: 127.0.0.1:8765
#     upstreams:
#       - name: backend
#         url: http://192.0.2.1:8000  # TEST-NET-1 占位地址，不是真实上游
#         priority: 10
`
	path = app.ExpandPath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在，拒绝覆盖", path)
	}
	return os.WriteFile(path, []byte(example), 0o600)
}
