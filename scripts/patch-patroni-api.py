#!/usr/bin/env python3
"""Backport bounded API socket I/O to the pinned Patroni 4.1.5 wheel.

The upstream timeout implementation is in patroni/patroni master, api.py.
Keep this narrowly scoped backport until a tested release includes those fixes.
A source checksum deliberately makes dependency upgrades require review.
"""
import hashlib
import importlib.util
from pathlib import Path

SOURCE_SHA256 = 'f55951a2ab4957bccf22bdeb8bdf5253f1edb93f2008cb81b03e9ff6ce37b2a9'


def patch(source):
    if hashlib.sha256(source.encode()).hexdigest() != SOURCE_SHA256:
        raise RuntimeError('Unexpected Patroni API source; review timeout backport before upgrading')
    anchor = '    def handle_one_request(self) -> None:\n'
    setup = '''    def setup(self) -> None:
        # Bound HTTP reads/writes as well as the TLS handshake below. Silent
        # clients must not occupy the finite REST API worker pool indefinitely.
        super(RestApiHandler, self).setup()
        self.connection.settimeout(5)

'''
    handshake = '                    request.do_handshake()\n'
    shutdown = '                    request.unwrap()\n'
    if any(source.count(text) != 1 for text in [anchor, handshake, shutdown]):
        raise RuntimeError('Unexpected Patroni API timeout patch locations')
    return source.replace(anchor, setup + anchor).replace(
        handshake,
        '                    request.settimeout(2)\n' + handshake,
    ).replace(
        shutdown,
        # A timed-out HTTP reader must not spend another request timeout
        # waiting for a silent peer to finish the TLS close-notify exchange.
        '                    request.settimeout(1)\n' + shutdown,
    )


if __name__ == '__main__':
    spec = importlib.util.find_spec('patroni.api')
    if spec is None or spec.origin is None:
        raise SystemExit('Patroni API module not installed')
    path = Path(spec.origin)
    path.write_text(patch(path.read_text()))
    print('Applied Patroni API handshake=2s / request-I/O=5s / TLS-shutdown=1s timeouts')
