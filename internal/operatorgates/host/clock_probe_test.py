import io
import json
import threading
import unittest
from unittest.mock import patch
import clock_probe as helper

class HelperTests(unittest.TestCase):
    def test_targets(self):
        values = [name+'=http://127.0.0.1:24891' for name in ('control', 'w1', 'w2')]
        self.assertEqual(len(helper.peers(values)), 3)
        for bad in (values[:2], values+[values[0]], ['control=http://8.8.8.8:80']+values[1:]):
            with self.assertRaises(ValueError, msg='MUTATION: helper-validation'):
                helper.peers(bad)
        output, stop, calls = io.BytesIO(), threading.Event(), []
        class Thread:
            def __init__(self, target, args): calls.append(args[0])
            def start(self): pass
            def join(self): pass
        with patch.object(helper.threading, 'Thread', Thread):
            helper.observe(helper.peers(values), output, stop)
        self.assertEqual(calls, ['control', 'w1', 'w2'], 'MUTATION: helper-targets')
    def test_record_and_capacity(self):
        class Output(io.BytesIO):
            def flush(self): stop.set()
        output, stop = Output(), threading.Event()
        stamps = (1792886400000000000,1792886400000001000,1792886400000001000,1792886400000002000,0)
        with patch.object(helper, 'probe', return_value=stamps):
            helper.observe({'control':'unused'}, output, stop)
        self.assertEqual(len(output.getvalue()), 84)
        output, stop = Output(), threading.Event()
        output.seek(4*1024*1024-84)
        with patch.object(helper, 'probe', return_value=stamps):
            helper.observe({'control':'unused'}, output, stop)
        self.assertTrue(output.getvalue().endswith(('3 '+' '.join(['0'*19]*4)+' 3\n').encode()), 'MUTATION: helper-capacity')
    def test_probe_response_and_failure(self):
        response = io.BytesIO(json.dumps(dict(receivedUnixNs='1792886400000001000',sentUnixNs='1792886400000002000')).encode())
        from unittest.mock import Mock
        connection = Mock()
        response.status = 200
        connection.getresponse.return_value = response
        stamps = iter([1792886400000000000,1792886400000003000])
        def now():
            self.assertTrue(connection.connect.called, 'MUTATION: helper-connect-order')
            return next(stamps)
        with patch('http.client.HTTPConnection',return_value=connection), patch.object(helper.time,'time_ns',side_effect=now):
            self.assertEqual(helper.probe('http://127.0.0.1:24891')[-1],0)
        with patch('http.client.HTTPConnection',side_effect=OSError('unavailable')):
            self.assertEqual(helper.probe('http://127.0.0.1:24891')[1:3],(0,0))
    def test_endpoint_and_finite_lifetime(self):
        from unittest.mock import Mock
        handler = helper.ClockHandler.__new__(helper.ClockHandler)
        handler.path, handler.wfile = '/clock', io.BytesIO()
        handler.send_response, handler.send_header, handler.end_headers = Mock(), Mock(), Mock()
        with patch.object(helper.time,'time_ns',side_effect=[1792886400000001000,1792886400000002000]):
            handler.do_GET()
        self.assertEqual(json.loads(handler.wfile.getvalue())['sentUnixNs'],'1792886400000002000')
        server = Mock()
        class Timer:
            def __init__(self, seconds, done): self.done=done; self.seconds=seconds
            def start(self): self.done()
            def cancel(self): pass
        with patch('sys.argv',['clock_probe.py','--listen','127.0.0.1:24891']), patch.object(helper.http.server,'HTTPServer',return_value=server), patch.object(helper.threading,'Timer',Timer):
            helper.main()
        server.shutdown.assert_called_once()

    def test_cooperative_stop_file(self):
        import pathlib
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'stop'
            path.touch()
            stop = threading.Event()
            helper.watch_stop(path, stop)
            self.assertTrue(stop.is_set(), 'MUTATION: helper-stop-file')
