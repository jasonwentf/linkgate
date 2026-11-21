#!/usr/bin/env python3
"""Local binary-only smoke: never uses the default configuration or production ports."""
import argparse
import json
import pathlib
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def listening(port):
    with socket.socket() as sock:
        sock.settimeout(0.2)
        return sock.connect_ex(("127.0.0.1", port)) == 0


class Upstream(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ready")

    def log_message(self, *_):
        pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--work-dir", required=True)
    args = parser.parse_args()
    binary = pathlib.Path(args.binary).resolve()
    work = pathlib.Path(args.work_dir).resolve()
    work.mkdir(parents=True, exist_ok=True, mode=0o700)
    task = pathlib.Path(tempfile.mkdtemp(prefix="listener-smoke-", dir=work))
    task.chmod(0o700)
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    ports = []
    while len(ports) < 4:
        port = free_port()
        if port not in ports and port not in (8765, 8766):
            ports.append(port)
    proxy_port, admin_port, added_port, moved_port = ports
    config = task / "config.yaml"
    config.write_text(f"""version: 1
data_dir: {task / 'data'}
admin:
  listen: 127.0.0.1:{admin_port}
health:
  enabled: false
groups:
  - name: base
    listen: 127.0.0.1:{proxy_port}
    upstreams:
      - name: local
        url: http://127.0.0.1:{upstream.server_port}
""")
    config.chmod(0o600)
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    token = ""

    def request(method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"http://127.0.0.1:{admin_port}{path}", data=data, method=method)
        req.add_header("X-Linkgate-Token", token)
        req.add_header("Content-Type", "application/json")
        with client.open(req, timeout=5) as resp:
            return resp.status, json.load(resp)

    with (task / "kernel.log").open("wb") as log:
        process = subprocess.Popen([str(binary), "-config", str(config)], stdout=log, stderr=log)
        try:
            token_path = task / "data" / "admin.token"
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError("smoke kernel exited at startup")
                if token_path.exists() and listening(admin_port):
                    break
                time.sleep(0.05)
            else:
                raise RuntimeError("smoke admin did not start")
            token = token_path.read_text().strip()
            path = "/__linkgate/v1/groups"
            status, _ = request("POST", path, {"name": "added", "listen": f"127.0.0.1:{added_port}"})
            assert status == 201 and listening(added_port), "new listener missing"
            request("POST", path + "/added/upstreams", {"name": "local", "url": f"http://127.0.0.1:{upstream.server_port}", "priority": 0})
            with client.open(f"http://127.0.0.1:{added_port}/health", timeout=5) as resp:
                assert resp.read() == b"ready", "added group does not forward"
            request("PUT", path, {"name": "added", "listen": f"127.0.0.1:{moved_port}"})
            assert not listening(added_port) and listening(moved_port), "listen update failed"
            with socket.socket() as sock:
                # Match Go's listener reuse semantics: a closed HTTP connection can leave TIME_WAIT.
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                sock.bind(("127.0.0.1", added_port))
            with client.open(f"http://127.0.0.1:{moved_port}/health", timeout=5) as resp:
                assert resp.read() == b"ready", "moved listener does not forward"
            request("DELETE", path, {"name": "added"})
            assert not listening(moved_port), "deleted group still listens"
            assert (task / "config.yaml.bak").exists(), "backup missing"
            _, state = request("GET", "/__linkgate/v1/status")
            assert [g["name"] for g in state["groups"]] == ["base"], "admin has stale config"
            # SIGHUP on the isolated process, followed by a rejected unknown-field reload.
            if hasattr(signal, "SIGHUP"):
                config.write_text(config.read_text().replace("name: base", "name: renamed"))
                process.send_signal(signal.SIGHUP)
                for _ in range(100):
                    _, state = request("GET", "/__linkgate/v1/status")
                    if state["groups"][0]["name"] == "renamed":
                        break
                    time.sleep(0.05)
                else:
                    raise RuntimeError("SIGHUP left admin on old config")
            accepted = config.read_bytes()
            config.write_text("misspelled_field: true\n")
            try:
                request("POST", "/__linkgate/v1/reload")
                raise RuntimeError("invalid reload accepted")
            except urllib.error.HTTPError as err:
                assert err.code == 400
            assert config.read_bytes() == accepted, "invalid reload did not restore disk"
            print("PASS: binary CRUD add/move/delete, forwarding, .bak, status, isolated SIGHUP and rollback")
            print(f"Evidence retained at {task}")
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    main()
