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

    def test_unknown_endpoint_never_receives_key(self):
        with patch.dict(os.environ, {'YESCAPTCHA_API_BASE': 'https://other.example'}), \
                patch.object(solver, 'post_json') as post:
            with self.assertRaises(ValueError):
                solver.solve_yes('private-key', 'image', 60)
            post.assert_not_called()


if __name__ == '__main__':
    unittest.main()
