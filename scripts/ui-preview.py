#!/usr/bin/env python3
"""Run an isolated, local-only panel preview. No service manager or user config."""
import argparse
import http.server
import json
import os
from pathlib import Path
import signal
import subprocess
import threading
import time
import urllib.error
import urllib.request


class Panel(http.server.BaseHTTPRequestHandler):
    """Live HTML preview on a separate loopback port, including a dark CSS fixture."""
    def do_GET(self):
        if self.path in ("/", "/dark"):
            html = self.server.ui_file.read_text(encoding="utf-8")
            if self.path == "/dark":
                html = html.replace("@media (prefers-color-scheme: dark)", "@media screen")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(html.encode())
        else:
            self.proxy()

    def do_POST(self):
        self.proxy()

    def do_PUT(self):
        self.proxy()

    def do_DELETE(self):
        self.proxy()

    def proxy(self):
        if not self.path.startswith("/__linkgate/v1/"):
            self.send_error(404)
            return
        size = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(size) if size else None
        token = (self.server.data_dir / "admin.token").read_text().strip()
        request = urllib.request.Request("http://127.0.0.1:18766" + self.path,
            data=body, method=self.command, headers={"X-Linkgate-Token": token,
            "Content-Type": self.headers.get("Content-Type", "application/json")})
        try:
            response = urllib.request.urlopen(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            self.send_response(response.status)
            self.send_header("Content-Type", response.headers.get("Content-Type", "application/json"))
            self.end_headers()
            self.wfile.write(response.read())

    def log_message(self, *_):
        pass


class Backend(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        time.sleep(self.server.delay)
        self.send_response(self.server.code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true,"source":"ui-preview"}')

    def log_message(self, *_):
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--work-dir", required=True)
    parser.add_argument("--duration", type=int, default=1800)
    parser.add_argument("--ui-file", help="Serve live HTML on 18770; /dark previews dark tokens without changing system settings")
    parser.add_argument("--panel-only", action="store_true", help="Serve live HTML against an already running isolated preview")
    args = parser.parse_args()
    root = Path(args.work_dir).resolve()
    if args.panel_only:
        if not args.ui_file:
            parser.error("--panel-only requires --ui-file")
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 18770), Panel)
        server.ui_file, server.data_dir = Path(args.ui_file).resolve(), root / "data"
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            server.server_close()
        return
    root.mkdir(parents=True, exist_ok=False, mode=0o700)
    data = root / "data"
    data.mkdir(mode=0o700)
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    servers = []
    process = None
    try:
        if args.ui_file:
            panel = http.server.ThreadingHTTPServer(("127.0.0.1", 18770), Panel)
            panel.ui_file, panel.data_dir = Path(args.ui_file).resolve(), data
            servers.append(panel)
            threading.Thread(target=panel.serve_forever, daemon=True).start()
        for port, delay, code in ((18780, .008, 200), (18781, .032, 200), (18782, .015, 503)):
            server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Backend)
            server.delay, server.code = delay, code
            servers.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
        lines = ["version: 1", "data_dir: " + json.dumps(str(data)),
                 "admin:", "  listen: 127.0.0.1:18766", "log:", "  ring_size: 2000",
                 "health:", "  interval: 10s", "  timeout: 300ms", "  fail_threshold: 1",
                 "  recover_threshold: 1", "  cooldown: 10s", "  probe_path: /health", "groups:"]
        services = [("模型 API", 18765, "统一的模型入口，私网优先，边缘备用。"),
                    ("工作助手", 18767, "日常工具与自动化任务。"),
                    ("文件服务", 18768, "本地文件工具的固定入口。"),
                    ("实验环境", 18769, "隔离测试，不影响常用服务。")]
        for index, (name, port, note) in enumerate(services):
            lines += ["  - name: " + json.dumps(name, ensure_ascii=False),
                      f"    listen: 127.0.0.1:{port}", "    protocol: http",
                      "    comment: " + json.dumps(note, ensure_ascii=False), "    upstreams:"]
            for route, target, priority, kind, enabled, comment in [
                ("私网主线", 18780, 10, "zt", True, "优先使用的私网线路"),
                ("私网备用", 18781, 20, "ts", True, "主线不可用时接替"),
                ("边缘入口", 18782, 30, "cf" if index < 2 else "frp", index != 3, "演示故障或停用状态")]:
                lines += ["      - name: " + json.dumps(route, ensure_ascii=False),
                          f"        url: http://127.0.0.1:{target}", f"        priority: {priority}",
                          "        kind: " + kind, "        enabled: " + str(enabled).lower(),
                          "        comment: " + json.dumps(comment, ensure_ascii=False)]
        cfg = root / "config.yaml"
        cfg.write_text("\n".join(lines) + "\n", encoding="utf-8")
        cfg.chmod(0o600)
        with (root / "kernel.log").open("wb") as log:
            (root / "kernel.log").chmod(0o600)
            process = subprocess.Popen([str(Path(args.binary).resolve()), "-config", str(cfg)], stdout=log, stderr=log)
            base = "http://127.0.0.1:18766/__linkgate/v1"
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError("Preview kernel exited; inspect kernel.log")
                try:
                    urllib.request.urlopen(base + "/health", timeout=.3).close()
                    break
                except (urllib.error.URLError, TimeoutError):
                    time.sleep(.1)
            else:
                raise RuntimeError("Preview kernel did not start")
            token = (data / "admin.token").read_text().strip()

            def api(path, method="GET", body=None):
                request = urllib.request.Request(base + path, method=method,
                    data=None if body is None else json.dumps(body).encode(),
                    headers={"X-Linkgate-Token": token, "Content-Type": "application/json"})
                with urllib.request.urlopen(request, timeout=5) as response:
                    return json.load(response)

            from urllib.parse import quote
            # Exercise a real safe pre-connect failover, then remove only the test route.
            path = "/groups/" + quote("模型 API") + "/upstreams"
            api(path, "POST", {"name": "临时断路线", "url": "http://127.0.0.1:18783", "priority": 0, "kind": "direct"})
            for _ in range(8):
                urllib.request.urlopen("http://127.0.0.1:18765/health", timeout=5).close()
            api(path, "DELETE", {"name": "临时断路线"})
            for port in (18767, 18768, 18769):
                for _ in range(4):
                    urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=5).close()
            status = api("/status")
            assert len(status["groups"]) == 4
            assert all(len(g["upstreams"]) == 3 for g in status["groups"])
            assert all(g["protocol"] == "http" and g["comment"] for g in status["groups"])
            assert api("/config")["groups"][0]["upstreams"][0]["kind"] == "zt"
            assert any(e["kind"] == "failover" and e["timestamp"] for e in api("/events?limit=300")["events"])
            (root / "ready").write_text("Preview ready; stop by creating a stop file in this directory.\n")
            print("Preview ready: 4 services / 12 routes; API and failover checks passed", flush=True)
            deadline = time.monotonic() + args.duration
            while not stop.wait(.5) and time.monotonic() < deadline and not (root / "stop").exists():
                if process.poll() is not None:
                    raise RuntimeError("Preview kernel exited")
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for server in servers:
            server.shutdown()
            server.server_close()
        print("Isolated preview stopped", flush=True)


if __name__ == "__main__":
    main()
