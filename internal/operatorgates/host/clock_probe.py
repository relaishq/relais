"""Finite clock observer; deploy beside host assets, independently of products."""
import argparse
import http.server
import ipaddress
import json
import threading
import time
import http.client
import urllib.parse


class ClockHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        received = time.time_ns()
        if self.path != '/clock':
            self.send_error(404)
            return
        body = json.dumps(dict(receivedUnixNs=str(received), sentUnixNs=str(time.time_ns()))).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass

def private_ip(value):
    ip = ipaddress.ip_address(value)
    if not (ip.is_private or ip.is_loopback) or ip.version != 4:
        raise ValueError('private IPv4 required')
    return str(ip)


def peers(values):
    result = {}
    for value in values:
        name, origin = value.split('=', 1)
        u = urllib.parse.urlsplit(origin)
        if name not in ('control', 'w1', 'w2') or name in result or u.scheme != 'http' or u.username or u.password or u.path or u.query or u.fragment:
            raise ValueError('invalid clock peer')
        private_ip(u.hostname)
        if not u.port or not 1 <= u.port <= 65535:
            raise ValueError('invalid clock port')
        result[name] = origin
    if result and set(result) != {'control', 'w1', 'w2'}:
        raise ValueError('all three peers required')
    return result


def probe(origin):
    f, r, h, s, status = 0, 0, 0, 0, 1
    u = urllib.parse.urlsplit(origin)
    conn = None
    try:
        conn = http.client.HTTPConnection(u.hostname, u.port, timeout=0.75)
        conn.connect()
        f = time.time_ns()
        conn.request('GET', '/clock')
        response = conn.getresponse()
        raw = response.read(1025)
        if response.status != 200:
            raise ValueError('clock request failed')
        r, status = time.time_ns(), 2
        if len(raw) > 1024:
            raise ValueError('oversized clock response')
        data = json.loads(raw)
        h, s = int(data['receivedUnixNs']), int(data['sentUnixNs'])
        if not (0 < h <= s < 2**63 and f <= r < 2**63 and r-f >= s-h):
            raise ValueError('invalid clock response')
        status = 0
    except Exception:
        r = r or time.time_ns()
        if status == 2:
            h, s = 0, 0
    finally:
        if conn is not None:
            conn.close()
    return f, h, s, r, status


def observe(targets, output, stop):
    lock = threading.Lock()

    def worker(name, origin):
        target = ('control', 'w1', 'w2').index(name)
        tick = time.monotonic()
        while not stop.is_set():
            f, h, s, r, status = probe(origin)
            line = ('%d %019d %019d %019d %019d %d\n' % (target, f, h, s, r, status)).encode()
            with lock:
                if stop.is_set():
                    return
                if output.tell() + len(line) > 4*1024*1024 - 84:
                    output.write(('3 ' + ' '.join(['0'*19]*4) + ' 3\n').encode())
                    output.flush()
                    stop.set()
                    return
                output.write(line)
                output.flush()
            tick += 1
            stop.wait(max(0, tick-time.monotonic()))

    threads = [threading.Thread(target=worker, args=(name, origin)) for name, origin in sorted(targets.items())]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--listen', required=True)
    parser.add_argument('--seconds', type=int, default=150)
    parser.add_argument('--until-unix-ns', type=int, default=0)
    parser.add_argument('--peer', action='append', default=[])
    parser.add_argument('--output', default='')
    args = parser.parse_args()
    address, port = args.listen.rsplit(':', 1)
    targets = peers(args.peer)
    if not 1 <= args.seconds <= 6300 or bool(targets) != bool(args.output):
        raise ValueError('invalid clock helper configuration')
    server = http.server.HTTPServer((private_ip(address), int(port)), ClockHandler)
    stop = threading.Event()
    serving = threading.Thread(target=server.serve_forever)
    serving.start()
    duration = args.seconds
    if args.until_unix_ns:
        duration = min(duration, max(0, (args.until_unix_ns-time.time_ns()) / 1e9))
    timer = threading.Timer(duration, stop.set)
    timer.start()
    try:
        if targets:
            with open(args.output, 'xb') as output:
                observe(targets, output, stop)
        else:
            stop.wait()
    finally:
        stop.set()
        timer.cancel()
        server.shutdown()
        server.server_close()
        serving.join()


if __name__ == '__main__':
    main()
