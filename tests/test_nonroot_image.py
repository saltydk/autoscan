import importlib.util
import io
import socket
import socketserver
import struct
import threading
import unittest
import urllib.error
from email.message import Message
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "nonroot_runtime",
    Path(__file__).resolve().parents[1] / "scripts/test-nonroot-image.py",
)
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class ReadinessTests(unittest.TestCase):
    def test_retry_connection_reset_before_server_is_ready(self):
        class StartingServer(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.recv(4096)
                self.server.requests += 1
                if self.server.requests == 1:
                    self.request.setsockopt(
                        socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0)
                    )
                    self.request.close()
                    return
                self.request.sendall(b"HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n")

        with socketserver.TCPServer(("127.0.0.1", 0), StartingServer) as server:
            server.requests = 0
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with patch.object(runtime, "docker", return_value="true"):
                    runtime.wait_for_server(
                        "starting-container",
                        f"http://127.0.0.1:{server.server_address[1]}",
                    )
                self.assertEqual(server.requests, 2)
            finally:
                server.shutdown()
                thread.join()

    def test_http_error_status_is_returned_and_closed(self):
        error = urllib.error.HTTPError(
            "http://127.0.0.1/triggers/manual",
            401,
            "Unauthorized",
            Message(),
            io.BytesIO(b"unauthorized"),
        )
        with patch.object(runtime.urllib.request, "urlopen", side_effect=error):
            status = runtime.request("http://127.0.0.1/triggers/manual")

        self.assertEqual(status, 401)
        self.assertTrue(error.fp.closed)

    def test_fail_when_container_exits_during_startup(self):
        with (
            patch.object(
                runtime,
                "request",
                side_effect=urllib.error.URLError("connection refused"),
            ),
            patch.object(runtime, "docker", return_value="false"),
            self.assertRaisesRegex(AssertionError, "exited during startup"),
        ):
            runtime.wait_for_server("exited-container", "http://127.0.0.1")

    def test_timeout_still_bounds_startup_retries(self):
        with (
            patch.object(runtime, "request", side_effect=TimeoutError),
            patch.object(runtime, "docker", return_value="true"),
            patch.object(runtime.time, "monotonic", side_effect=[0, 60]),
            self.assertRaisesRegex(AssertionError, "did not start"),
        ):
            runtime.wait_for_server("unready-container", "http://127.0.0.1")
