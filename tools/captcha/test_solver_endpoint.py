import os
import unittest
from unittest.mock import patch

import solver


class YesCaptchaEndpointTests(unittest.TestCase):
    def test_create_and_poll_use_the_same_configured_endpoint(self):
        for endpoint in ['https://api.yescaptcha.com', 'https://cn.yescaptcha.com']:
            with self.subTest(endpoint=endpoint), patch.dict(os.environ, {'YESCAPTCHA_API_BASE': endpoint}), \
                    patch.object(solver, 'post_json', side_effect=[{'taskId': 'fixture'},
                        {'status': 'ready', 'solution': {'text': 'Ab3D'}}]) as post, \
                    patch.object(solver.time, 'sleep'):
                self.assertEqual(solver.solve_yes('private-key', 'image', 60), 'Ab3D')
                self.assertEqual([call.args[0] for call in post.call_args_list],
                                 [endpoint + '/createTask', endpoint + '/getTaskResult'])

    def test_default_endpoint_is_domestic(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(solver, 'post_json', side_effect=[
                {'taskId': 'fixture'}, {'status': 'ready', 'solution': {'text': 'Ab3D'}}]) as post, \
                patch.object(solver.time, 'sleep'):
            self.assertEqual(solver.solve_yes('private-key', 'image', 60), 'Ab3D')
            self.assertTrue(all(c.args[0].startswith('https://cn.yescaptcha.com/') for c in post.call_args_list))

    def test_request_ignores_browser_and_system_proxies(self):
        import http.server
        import threading
        import json
        received = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                received.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"ok":true}')
            def log_message(self, *args):
                pass
        with http.server.HTTPServer(('127.0.0.1', 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with patch.dict(os.environ, {'HTTP_PROXY': 'socks5://127.0.0.1:1',
                        'HTTPS_PROXY': 'socks5://127.0.0.1:1', 'NO_PROXY': ''}), \
                        patch.object(solver.urllib.request, 'getproxies', return_value={'http': 'socks5://127.0.0.1:1'}):
                    self.assertEqual(solver.post_json(f'http://127.0.0.1:{server.server_port}/fixture', {'fixture': True}, timeout=2), {'ok': True})
                self.assertEqual(received, [{'fixture': True}])
            finally:
                server.shutdown()
                thread.join()

    def test_unknown_endpoint_never_receives_key(self):
        with patch.dict(os.environ, {'YESCAPTCHA_API_BASE': 'https://other.example'}), \
                patch.object(solver, 'post_json') as post:
            with self.assertRaises(ValueError):
                solver.solve_yes('private-key', 'image', 60)
            post.assert_not_called()


if __name__ == '__main__':
    unittest.main()
