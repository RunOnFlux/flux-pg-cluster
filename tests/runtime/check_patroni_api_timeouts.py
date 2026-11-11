"""Run with /opt/patroni/bin/python inside the built image; no database needed."""
import http.client
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock

from patroni.api import RestApiHandler, RestApiServer


class HealthHandler(RestApiHandler):
    def do_GET(self):
        self.write_response(200, '{"ok":true}', content_type='application/json')


class ApiTimeouts(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory()
        d = Path(cls.directory.name)
        cls.cert, cls.key = str(d / 'test.crt'), str(d / 'test.key')
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-keyout', cls.key, '-out', cls.cert, '-days', '1',
                        '-subj', '/CN=localhost'], check=True, capture_output=True)
        cls.context = ssl._create_unverified_context()

    @classmethod
    def tearDownClass(cls):
        cls.directory.cleanup()

    def setUp(self):
        self.server = RestApiServer(Mock(), {'listen': '127.0.0.1:0', 'certfile': self.cert, 'keyfile': self.key})
        self.server.RequestHandlerClass = HealthHandler
        self.port = self.server.server_address[1]
        self.clients = []
        self.entered = 0
        self.lock = threading.Condition()
        original = self.server.process_request_thread

        def track(request, address):
            with self.lock:
                self.entered += 1
                self.lock.notify_all()
            return original(request, address)

        self.server.process_request_thread = track
        self.server.start()

    def tearDown(self):
        for c in self.clients:
            c.close()
        self.server.shutdown()
        self.server.server_close()

    def wait_workers(self):
        with self.lock:
            self.assertTrue(self.lock.wait_for(lambda: self.entered >= 5, timeout=3), 'did not occupy all five workers')

    def healthy_request(self, timeout):
        connection = http.client.HTTPSConnection('127.0.0.1', self.port, context=self.context, timeout=timeout)
        try:
            connection.request('GET', '/probe')
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.read(), b'{"ok":true}')
        finally:
            connection.close()

    def test_silent_tls_clients_cannot_exhaust_api(self):
        for _ in range(5):
            self.clients.append(socket.create_connection(('127.0.0.1', self.port), timeout=2))
        self.wait_workers()
        start = time.monotonic()
        self.healthy_request(timeout=4)
        self.assertLess(time.monotonic() - start, 4)

    def test_tls_clients_without_http_request_cannot_exhaust_api(self):
        for _ in range(5):
            raw = socket.create_connection(('127.0.0.1', self.port), timeout=2)
            self.clients.append(self.context.wrap_socket(raw, server_hostname='localhost'))
        self.wait_workers()
        start = time.monotonic()
        # Five seconds for HTTP I/O plus one for TLS close-notify, with
        # scheduling margin. The old API never releases these workers.
        self.healthy_request(timeout=8)
        self.assertLess(time.monotonic() - start, 8)

    def test_clients_ignoring_tls_shutdown_cannot_exhaust_api(self):
        for _ in range(5):
            raw = socket.create_connection(('127.0.0.1', self.port), timeout=2)
            client = self.context.wrap_socket(raw, server_hostname='localhost')
            self.clients.append(client)
            client.sendall(b'GET /probe HTTP/1.0\r\n\r\n')
        self.wait_workers()
        self.healthy_request(timeout=4)

    def test_aborted_handshakes_do_not_break_next_request(self):
        for _ in range(20):
            with socket.create_connection(('127.0.0.1', self.port), timeout=2) as c:
                c.sendall(b'not a TLS handshake')
        self.healthy_request(timeout=4)


if __name__ == '__main__':
    unittest.main()
