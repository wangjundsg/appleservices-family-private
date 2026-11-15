"""Protocol registration adapter boundary.

The pipeline owns purchasing, refunds, cleanup and cancellation. A protocol
registration is delegated to an adapter that only knows how to drive the
protocol executor; it never touches numbers or browser resources.

Production has no adapter by default: a job that asks for ``protocol`` stops with
``protocol_adapter_missing`` before a number is bought or a browser is started.
Tests inject a fake adapter through ``APPLESERVICES_PROTOCOL_ADAPTER`` as
``module:callable``.
"""

from __future__ import annotations

import importlib
import os


class AdapterMissing(Exception):
    def __init__(self, reason="protocol_adapter_missing"):
        super().__init__(reason)
        self.reason = reason


def _spec(config=None):
    spec = os.environ.get("APPLESERVICES_PROTOCOL_ADAPTER", "").strip()
    if not spec and config:
        spec = str(config.get("adapter") or "").strip()
    return spec


def resolve(config=None):
    """Return the configured adapter, or raise AdapterMissing.

    The environment variable is the only production-safe switch: it is unset in
    the shipped configuration, so a protocol job cannot silently run on a stub.
    """
    spec = _spec(config)
    if not spec:
        raise AdapterMissing()
    module_name, _, attribute = spec.partition(":")
    if not module_name or not attribute:
        raise AdapterMissing("protocol_adapter_spec_invalid")
    module = importlib.import_module(module_name)
    factory = getattr(module, attribute, None)
    if factory is None:
        raise AdapterMissing("protocol_adapter_spec_invalid")
    adapter = factory() if callable(factory) else factory
    for method in ("create", "resume", "cancel"):
        if not hasattr(adapter, method):
            raise AdapterMissing("protocol_adapter_incomplete")
    return adapter
