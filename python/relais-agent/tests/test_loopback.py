import unittest

import av

from relais_agent.__main__ import authenticate, loopback
from websockets.asyncio.server import serve
from websockets.asyncio.client import connect
from websockets.exceptions import InvalidStatus
from relais_agent.codec import Tone, decoder


class LoopbackTests(unittest.TestCase):
    def test_remote_and_dns_refused(self):
        for host in ("0.0.0.0", "192.168.1.1", "localhost", "::"):
            with self.assertRaises(ValueError):
                loopback(host)
        self.assertEqual(loopback("127.0.0.1"), "127.0.0.1")
        self.assertEqual(loopback("::1"), "::1")

    def test_pinned_codec_round_trip(self):
        tone, decode = Tone(), decoder()
        packets = [tone.packet() for _ in range(50)]
        self.assertTrue(all(0 < len(p) <= 1200 for p in packets))
        self.assertEqual(sum(f.samples for p in packets for f in decode.decode(av.Packet(p))), 48000)


class AuthenticationTests(unittest.IsolatedAsyncioTestCase):
    async def test_actual_handshake_auth_and_browser_origin(self):
        async def handler(ws):
            await ws.send("authenticated")

        async with serve(handler, "127.0.0.1", 0,
                         process_request=authenticate("isolated-test-token")) as server:
            url = f"ws://127.0.0.1:{server.sockets[0].getsockname()[1]}/bridge"
            for headers, origin, status in (({}, None, 401),
                                           ({"Authorization": "Bearer wrong"}, None, 401),
                                           ({"Authorization": "Bearer isolated-test-token"},
                                            "http://localhost", 403)):
                with self.assertRaises(InvalidStatus) as caught:
                    async with connect(url, additional_headers=headers, origin=origin, proxy=None):
                        self.fail("unauthorized connection upgraded")
                self.assertEqual(caught.exception.response.status_code, status)
            async with connect(url, additional_headers={"Authorization": "Bearer isolated-test-token"},
                               proxy=None) as ws:
                self.assertEqual(await ws.recv(), "authenticated")
