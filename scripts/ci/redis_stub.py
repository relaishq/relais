"""Minimal loopback PING server for the wrapper's process-lifetime tests."""
import socket
import sys

port = int(sys.argv[sys.argv.index('--port') + 1])
if port == 6379:
    raise SystemExit('The test stub requires a dedicated port')
with socket.socket() as server:
    server.bind(('127.0.0.1', port))
    server.listen()
    print('Ready to accept connections', flush=True)
    while True:
        client, _ = server.accept()
        with client:
            client.recv(1024)
            client.sendall(b'+PONG\r\n')
