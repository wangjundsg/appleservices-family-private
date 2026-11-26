import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from PIL import Image, ImageDraw
import ocr
import solver
import contextlib
import io
import json
import urllib.error

class OCRTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.image = Path(self.directory.name) / 'fixture.png'
        image = Image.new('RGB',(126,56),'white')
        ImageDraw.Draw(image).text((15,15),'Ab3D',fill='black')
        image.save(self.image)

    def test_strict_ascii_case_and_length(self):
        for text in ['Ab3D','aB8De','AbC123']: self.assertTrue(ocr.valid_text(text))
        for text in ['ABC','Ａb3D','答案Ab3D','AB CD','ABCDEFG','']: self.assertFalse(ocr.valid_text(text))

    def test_confidence_and_disagreement_fall_back(self):
        def model(text, confidence):
            return Mock(classification=Mock(return_value={'text':text,'confidence':confidence}))
        for second in [model('AB3D',.99),model('Ab3D',.5)]:
            with patch.object(ocr,'reader',side_effect=[model('Ab3D',.99),second]):
                self.assertEqual(ocr.read_local(self.image),'')
        with patch.object(ocr,'reader',side_effect=[model('Ab3D',.99),model('Ab3D',.98)]):
            self.assertEqual(ocr.read_local(self.image),'Ab3D')

    def test_empty_or_broken_images_do_not_reach_model(self):
        Image.new('RGB',(126,56),'white').save(self.image)
        with patch.object(ocr,'reader') as reader:
            self.assertEqual(ocr.read_local(self.image),'');reader.assert_not_called()

    def test_solver_uses_shared_local_policy(self):
        with patch.object(ocr,'read_local',return_value='Ab3D') as local:
            self.assertEqual(solver.local_ocr(str(self.image)),'Ab3D')
            local.assert_called_once_with(str(self.image))

    def test_local_first_never_calls_paid_solver_on_accepted_result(self):
        with patch('sys.argv',['solver.py',str(self.image)]),patch.object(solver,'local_ocr',return_value='Ab3D'), \
             patch.dict(solver.SOLVERS,{'yes':Mock(side_effect=AssertionError('must not pay'))}),contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(solver.main(),0)
        self.assertEqual(output.getvalue(),'Ab3D')

    def test_uncertain_local_uses_remote_once_and_preserves_case(self):
        remote=Mock(return_value='aB3D')
        with patch('sys.argv',['solver.py',str(self.image),'--key','fixture-key']),patch.object(solver,'local_ocr',return_value=''), \
             patch.dict(solver.SOLVERS,{'yes':remote}),contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(solver.main(),0)
        remote.assert_called_once()
        self.assertEqual(output.getvalue(),'aB3D')

    def test_transport_failure_returns_safe_diagnostic_without_answer(self):
        for failure, code in [
            (urllib.error.HTTPError('https://fixture.invalid/private',403,'private-response',{},None),'http_403'),
            (urllib.error.URLError('private-response'),'network_error'),
            (TimeoutError('private-response'),'network_error'),
        ]:
            with self.subTest(code=code), patch('sys.argv',['solver.py',str(self.image),'--skip-local','--key','fixture-key']), \
                 patch.dict(solver.SOLVERS,{'yes':Mock(side_effect=failure)}), \
                 contextlib.redirect_stdout(io.StringIO()) as output, contextlib.redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(solver.main(),1)
            self.assertEqual(output.getvalue(),'')
            self.assertEqual(json.loads(errors.getvalue()),{'solver_error':code})

if __name__ == '__main__': unittest.main()
