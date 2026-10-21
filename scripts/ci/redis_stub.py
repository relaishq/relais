"""Minimal loopback PING server for the wrapper's process-lifetime tests."""
import os
import socket
import sys

parent = os.getppid()
port = int(sys.argv[sys.argv.index('--port') + 1])
if port == 6379:
    raise SystemExit('The test stub requires a dedicated port')
with socket.socket() as server:
    server.bind(('127.0.0.1', port))
    server.listen()
    server.settimeout(0.1)
    print('Ready to accept connections', flush=True)
    # A killed wrapper cannot reap us. Bound both accept and receive so parent
    # death is noticed even without another connection or with an idle client.
    while parent != 1 and os.getppid() == parent:
        try:
            client, _ = server.accept()
        except socket.timeout:
            continue
        with client:
            client.settimeout(0.1)
            try:
                client.recv(1024)
                client.sendall(b'+PONG\r\n')
            except (socket.timeout, ConnectionError):
                pass
