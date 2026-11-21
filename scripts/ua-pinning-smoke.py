#!/usr/bin/env python3
"""Exercise UA pinning in an isolated kernel with synthetic loopback upstreams."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', required=True)
    parser.add_argument('--work-dir', required=True)
    args = parser.parse_args()
    binary = Path(args.binary).resolve(strict=True)
    work = Path(args.work_dir).resolve()
    work.mkdir(parents=True, exist_ok=False)
    os.chmod(work, 0o700)
    servers = []
    reservations = []
    proc = None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    class Mock(BaseHTTPRequestHandler):
        def do_GET(self):
            self.reply()

        def do_POST(self):
            self.reply()

        def reply(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            result = {
                'line': self.server.line,
                'ua': self.headers.get('User-Agent', ''),
                'body_sha256': hashlib.sha256(body).hexdigest(),
            }
            raw = json.dumps(result).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *args):
            pass

    try:
        urls = {}
        for line in ['direct', 'second', 'cf']:
            server = ThreadingHTTPServer(('127.0.0.1', 0), Mock)
            server.line = line
            servers.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            urls[line] = 'http://127.0.0.1:' + str(server.server_address[1])

        # Reserve distinct unused ports until all endpoints are chosen.
        for _ in range(2):
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            reservations.append(sock)
        group_port, admin_port = [sock.getsockname()[1] for sock in reservations]
        rules = [
            {'match': 'Chrome/', 'line': 'cf', 'comment': 'synthetic browser'},
            {'match': 'prefix:Mozilla/', 'line': 'second'},
            {'match': 'exact:Pinned-Client/1', 'line': 'second'},
            {'match': 'regex:(?i)^harness/[0-9]+$', 'line': 'cf'},
        ]
        config = work / 'config.yaml'
        cfg = {
            'version': 1,
            'data_dir': str(work / 'data'),
            'admin': {'listen': '127.0.0.1:' + str(admin_port)},
            'health': {'enabled': False, 'fail_threshold': 1},
            'retry': {'mode': 'connect', 'max_attempts': 3, 'backoff': '0s'},
            'http': {'connect_timeout': '200ms'},
            'groups': [{
                'name': 'ua-smoke', 'listen': '127.0.0.1:' + str(group_port),
                'ua_rules': rules,
                'upstreams': [
                    {'name': line, 'url': urls[line], 'priority': (i + 1) * 10}
                    for i, line in enumerate(['direct', 'second', 'cf'])
                ],
            }],
        }
        # JSON is valid YAML; avoid a third-party parser in this smoke harness.
        config.write_text(json.dumps(cfg, indent=2), encoding='utf-8')
        os.chmod(config, 0o600)
        log = (work / 'kernel.log').open('xb')
        os.chmod(work / 'kernel.log', 0o600)
        for sock in reservations:
            sock.close()
        proc = subprocess.Popen([str(binary), '-config', str(config)], stdout=log, stderr=subprocess.STDOUT)
        token = ''

        def request(path, method='GET', payload=None, ua=None, proxy=False):
            headers = {}
            if not proxy:
                headers['X-Linkgate-Token'] = token
            if ua is not None:
                headers['User-Agent'] = ua
            data = None
            if payload is not None:
                data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
                headers['Content-Type'] = 'application/json'
            port = group_port if proxy else admin_port
            req = urllib.request.Request('http://127.0.0.1:' + str(port) + path, data=data, headers=headers, method=method)
            try:
                with opener.open(req, timeout=5) as response:
                    return response.status, json.loads(response.read())
            except urllib.error.HTTPError as error:
                return error.code, json.loads(error.read())

        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError('isolated kernel exited before ready; see private kernel.log')
            token_file = work / 'data' / 'admin.token'
            if token_file.exists():
                token = token_file.read_text().strip()
                try:
                    code, status = request('/__linkgate/v1/status')
                    if code == 200:
                        break
                except OSError:
                    pass
            time.sleep(.05)
        else:
            raise RuntimeError('isolated startup timeout')

        results = []
        body = b'{"synthetic":"transparent"}'

        def route(ua, expected):
            code, result = request('/v1/messages', 'POST', body, ua=ua, proxy=True)
            assert code == 200 and result['line'] == expected, (code, result)
            assert result['ua'] == ua and result['body_sha256'] == hashlib.sha256(body).hexdigest()
            results.append({'ua': ua, 'line': expected})

        route('Mozilla/5.0 Chrome/131 Safari/537', 'cf')
        route('Mozilla/5.0 Firefox/132', 'second')
        route('curl/8', 'direct')
        route('PINNED-CLIENT/1', 'second')
        route('HARNESS/42', 'cf')
        for endpoint in ['status', 'stats']:
            code, response = request('/__linkgate/v1/' + endpoint)
            assert code == 200 and response['groups'][0]['ua_rules'] == rules
        _, stats = request('/__linkgate/v1/stats')
        assert stats['groups'][0]['failover'] == 0, 'successful pin is not a fallback'

        # Stop only our fake CF target. HTTP/1.0 responses do not leave stale pools.
        cf = servers[2]
        cf.shutdown()
        cf.server_close()
        servers.remove(cf)
        code, probe = request('/__linkgate/v1/probe?group=ua-smoke&upstream=cf', 'POST')
        assert code == 200 and not probe['reach']
        _, status = request('/__linkgate/v1/status')
        target = next(u for u in status['groups'][0]['upstreams'] if u['name'] == 'cf')
        assert target['status'] == 'unhealthy'
        route('Mozilla/5.0 Chrome/131 Safari/537', 'direct')
        _, events = request('/__linkgate/v1/events?group=ua-smoke')
        assert any(e['kind'] == 'failover' and e.get('from') == 'cf' and e.get('to') == 'direct' for e in events['events'])

        original = config.read_bytes()
        new_rules = [{'match': 'Chrome/', 'line': 'second', 'comment': 'updated via CRUD'}]
        code, _ = request('/__linkgate/v1/groups', 'PUT', {'name': 'ua-smoke', 'ua_rules': new_rules})
        assert code == 200
        backup = Path(str(config) + '.bak')
        assert backup.read_bytes() == original
        route('Mozilla/5.0 Chrome/131 Safari/537', 'second')
        accepted, accepted_bak = config.read_bytes(), backup.read_bytes()
        code, _ = request('/__linkgate/v1/groups', 'PUT', {'name': 'ua-smoke', 'ua_rules': [{'match': 'Chrome/', 'line': 'missing'}]})
        assert code == 400 and config.read_bytes() == accepted and backup.read_bytes() == accepted_bak
        _, stats = request('/__linkgate/v1/stats')
        assert stats['groups'][0]['ua_rules'] == new_rules
        assert stats['groups'][0]['total'] == 7 and stats['groups'][0]['failover'] == 1
        code, _ = request('/__linkgate/v1/groups', 'PUT', {'name': 'ua-smoke', 'ua_rules': []})
        assert code == 200
        route('Mozilla/5.0 Chrome/131 Safari/537', 'direct')
        _, status = request('/__linkgate/v1/status')
        assert status['groups'][0]['ua_rules'] == []
        output = {'requests': results, 'unhealthy_target_fallback': True, 'ua_body_transparent': True,
                  'crud_hot_update': True, 'backup_and_rejection_rollback': True,
                  'status_stats_rules': True, 'successful_pin_not_counted_as_fallback': True}
        result_file = work / 'results.json'
        result_file.write_text(json.dumps(output, indent=2), encoding='utf-8')
        os.chmod(result_file, 0o600)
        print('PASS isolated UA pinning smoke: matching, first rule, fallback, transparency, CRUD, .bak, status/stats')
    finally:
        for sock in reservations:
            sock.close()
        if proc is not None and proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            proc.wait(timeout=10)
        for server in servers:
            server.shutdown()
            server.server_close()
        if 'log' in locals():
            log.close()


if __name__ == '__main__':
    main()
