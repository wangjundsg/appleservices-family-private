"""Offline CPU readability inference; contains no OCR or collector integration."""
import argparse
import io
import json
import math
from pathlib import Path

import numpy as np
from PIL import Image, ImageOps
import torch
from torch import nn
from torchvision.models import mobilenet_v3_small


PREPROCESS_VERSION = 'rgb-fit-224x112-imagenet-v1'


def build_model():
    model = mobilenet_v3_small(weights=None)
    model.classifier = nn.Sequential(nn.Dropout(.2), nn.Linear(576, 1))
    return model


def preprocess(image):
    """Preserve every character, aspect ratio and white padding; no center crop."""
    if isinstance(image, (bytes, bytearray)):
        image = io.BytesIO(image)
    if not isinstance(image, Image.Image):
        with Image.open(image) as opened:
            return preprocess(opened)
    if min(image.size) < 1 or max(image.size) > 4096:
        raise ValueError('unsupported_image_dimensions')
    rgb = Image.new('RGB', image.size, 'white')
    rgba = image.convert('RGBA')
    rgb.paste(rgba, mask=rgba.getchannel('A'))
    resized = ImageOps.contain(rgb, (224, 112), Image.Resampling.BILINEAR)
    canvas = Image.new('RGB', (224, 112), 'white')
    canvas.paste(resized, ((224 - resized.width) // 2, (112 - resized.height) // 2))
    values = np.asarray(canvas, dtype=np.float32) / 255
    values = (values - np.array([.485, .456, .406], dtype=np.float32)) / np.array([.229, .224, .225], dtype=np.float32)
    return torch.from_numpy(values.transpose(2, 0, 1).copy())


def probability(logit, slope, intercept):
    value = float(logit) * slope + intercept
    if not math.isfinite(value):
        raise ValueError('nonfinite_model_output')
    return 1 / (1 + math.exp(-value)) if value >= 0 else math.exp(value) / (1 + math.exp(value))


class ReadabilityModel:
    def __init__(self, model_path):
        bundle = torch.load(Path(model_path), map_location='cpu', weights_only=True)
        metadata = bundle['metadata']
        if (metadata.get('schema_version') != 1 or metadata.get('architecture') != 'mobilenet_v3_small'
                or metadata.get('preprocess_version') != PREPROCESS_VERSION
                or not isinstance(metadata.get('model_version'), str) or not metadata['model_version']):
            raise ValueError('unsupported_readability_model')
        self.threshold = float(metadata['threshold'])
        self.slope, self.intercept = map(float, metadata['calibration'])
        if (not all(math.isfinite(v) for v in (self.threshold, self.slope, self.intercept))
                or not 0 <= self.threshold <= 1 or self.slope <= 0):
            raise ValueError('invalid_decision_parameters')
        self.metadata = metadata
        self.model = build_model()
        self.model.load_state_dict(bundle['state_dict'], strict=True)
        self.model.eval()

    @torch.inference_mode()
    def predict(self, image):
        logit = self.model(preprocess(image).unsqueeze(0)).item()
        score = probability(logit, self.slope, self.intercept)
        return {'decision': 'readable' if score > self.threshold else 'refresh',
                'readable_probability': score, 'model_version': self.metadata['model_version']}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--model', required=True)
    parser.add_argument('--image', required=True)
    args = parser.parse_args()
    torch.set_num_threads(1)
    print(json.dumps(ReadabilityModel(args.model).predict(args.image), allow_nan=False))
