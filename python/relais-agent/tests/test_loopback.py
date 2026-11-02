import unittest
from http import HTTPStatus

from websockets.asyncio.server import serve
from websockets.exceptions import InvalidStatus

import av

from relais_agent.__main__ import endpoint, loopback, Runner, socket
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


class AssignmentTests(unittest.TestCase):
    def test_floor_compares_epoch_before_generation_and_keeps_identity(self):
        runner = Runner("isolated-test-token", 150)
        runner.bot_instance = "bot"
        assignment = dict(session_id="call", epoch=1, generation=10, bot_instance="bot", lineage_id="lineage")
        runner.accept(assignment)
        runner.accept(dict(assignment, epoch=2))
        for changed in (dict(assignment, generation=999), dict(assignment, epoch=2, generation=9),
                        dict(assignment, epoch=2, lineage_id="other")):
            with self.assertRaises(ValueError):
                runner.accept(changed)
        runner.accept(dict(assignment, epoch=2))

    def test_remote_and_credential_urls_refused(self):
        for url in ("ws://localhost:9/bridge", "ws://127.0.0.1:9/bridge?token=x",
                    "ws://u:p@127.0.0.1:9/bridge", "wss://127.0.0.1:9/bridge"):
            with self.assertRaises(ValueError):
                endpoint(url)


class SocketTests(unittest.IsolatedAsyncioTestCase):
    async def test_redirects_do_not_forward_authorization(self):
        forwarded = []

        async def unused_handler(ws):
            self.fail("redirect handshake unexpectedly upgraded")

        def reject_target(connection, request):
            forwarded.append(request.headers.get("Authorization"))
            return connection.respond(HTTPStatus.FORBIDDEN, "target must not receive credentials\n")

        async with serve(unused_handler, "127.0.0.1", 0, process_request=reject_target) as target:
            destination = f"ws://127.0.0.1:{target.sockets[0].getsockname()[1]}/bridge"

            def redirect(connection, request):
                response = connection.respond(HTTPStatus.FOUND, "redirect refused\n")
                response.headers["Location"] = destination
                return response

            async with serve(unused_handler, "127.0.0.1", 0, process_request=redirect) as source:
                url = f"ws://127.0.0.1:{source.sockets[0].getsockname()[1]}/bridge"
                for suffix in ("", "/registry"):
                    with self.subTest(path=suffix), self.assertRaises(InvalidStatus) as caught:
                        async with socket(url + suffix, "isolated-test-token"):
                            self.fail("redirect was accepted")
                    self.assertEqual(caught.exception.response.status_code, 302)
                    self.assertEqual(forwarded, [], "Authorization reached a redirected endpoint")
