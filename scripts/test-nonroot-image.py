#!/usr/bin/env python3
"""Exercise the shell-free image using host-side Docker and HTTP clients."""

import argparse
import base64
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def container_file(container, path):
    data = subprocess.check_output(["docker", "cp", "-L", f"{container}:{path}", "-"])
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        files = [member for member in archive if member.isfile()]
        assert len(files) == 1, f"expected one file at {path}"
        member = files[0]
        return archive.extractfile(member).read(), member.uid, member.gid


def request(url, method="HEAD", authenticated=True):
    headers = {}
    if authenticated:
        credentials = base64.b64encode(b"acceptance:temporary-test-password").decode()
        headers["Authorization"] = "Basic " + credentials
    try:
        with urllib.request.urlopen(urllib.request.Request(url, method=method, headers=headers), timeout=2) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code


def verify_persisted_scan(container, directory):
    data = subprocess.check_output(["docker", "cp", f"{container}:/config/.", "-"])
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        for member in archive:
            name = Path(member.name).name
            if member.isfile() and name in {"autoscan.db", "autoscan.db-journal", "autoscan.db-wal", "autoscan.db-shm"}:
                (directory / name).write_bytes(archive.extractfile(member).read())
    with sqlite3.connect(directory / "autoscan.db") as database:
        count = database.execute("SELECT count(*) FROM scan WHERE folder = ?", ("/media/acceptance",)).fetchone()[0]
        assert count == 1, "accepted scan was not persisted"


def test_image(image, platform, short_sha, numeric_user):
    container = "autoscan-nonroot-" + uuid.uuid4().hex[:12]
    uid = (os.getuid() or 1000) if numeric_user else 65532
    gid = (os.getgid() or 1000) if numeric_user else 65532
    with tempfile.TemporaryDirectory(prefix="autoscan-nonroot-") as temporary:
        directory = Path(temporary)
        config = directory / "config.yml"
        config.write_text("host: [0.0.0.0]\nport: 3030\nminimum-age: 24h\nscan-stats: 0s\n"
                          "authentication:\n  username: acceptance\n  password: temporary-test-password\n")
        config.chmod(0o644)
        command = ["create", "--name", container, "--platform", platform, "--read-only",
                   "--tmpfs", "/tmp:rw,nosuid,noexec,size=64m", "--publish", "127.0.0.1::3030"]
        if numeric_user:
            mounted = directory / "config"
            mounted.mkdir(mode=0o755)
            # Simulate an existing log that needs rotation under the same UID/GID.
            log = mounted / "activity.log"
            log.write_bytes(b"x" * (6 * 1024 * 1024))
            if os.getuid() == 0:
                os.chown(mounted, uid, gid)
                os.chown(log, uid, gid)
            command.extend(["--user", f"{uid}:{gid}", "--mount", f"type=bind,src={mounted},dst=/config"])
        command.append(image)
        try:
            docker(*command)
            docker("cp", str(config), f"{container}:/config/config.yml")
            docker("start", container)
            address = docker("port", container, "3030/tcp").splitlines()[0]
            url = "http://" + address + "/triggers/manual"
            deadline = time.monotonic() + 60
            while True:
                try:
                    if request(url) == 200:
                        break
                except urllib.error.URLError:
                    pass
                if docker("inspect", "--format", "{{.State.Running}}", container) != "true":
                    raise AssertionError("Autoscan exited during startup")
                if time.monotonic() >= deadline:
                    raise AssertionError("Autoscan did not start its authenticated webhook server")
                time.sleep(0.5)

            version = docker("exec", container, "/app/autoscan/autoscan", "--version")
            assert short_sha + "@" in version, f"wrong binary: {version}"
            processes = docker("top", container, "-eo", "pid,uid,gid,args").splitlines()[1:]
            assert processes and all(line.split()[1:3] == [str(uid), str(gid)] for line in processes), processes
            assert request(url, authenticated=False) == 401
            assert request(url + "?dir=%2Fmedia%2Facceptance", method="POST") == 200
            for name in ("autoscan.db", "activity.log"):
                data, owner, group = container_file(container, "/config/" + name)
                assert data and (owner, group) == (uid, gid), f"incorrect {name} ownership"
            if numeric_user:
                assert any(mounted.glob("activity-*.log")), "existing log did not rotate"
            else:
                certificates, _, _ = container_file(container, "/etc/ssl/certs/ca-certificates.crt")
                assert b"BEGIN CERTIFICATE" in certificates, "CA trust store is missing"
                zone, _, _ = container_file(container, "/usr/share/zoneinfo/Europe/Copenhagen")
                assert zone.startswith(b"TZif"), "timezone data is missing"
                shell = subprocess.run(["docker", "exec", container, "/bin/sh", "-c", "true"], capture_output=True)
                assert shell.returncode != 0, "nonroot image unexpectedly contains a shell"
            docker("stop", "--time", "10", container)
            assert docker("inspect", "--format", "{{.State.ExitCode}}", container) == "143", "unexpected SIGTERM exit"
            verify_persisted_scan(container, directory)
            print(f"Passed {platform}, UID/GID {uid}:{gid}, read-only root, persisted scan: {version}")
        except Exception:
            subprocess.run(["docker", "logs", container], check=False)
            raise
        finally:
            subprocess.run(["docker", "rm", "--force", "--volumes", container], capture_output=True, check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("platform")
    parser.add_argument("arch")
    parser.add_argument("short_sha")
    args = parser.parse_args()
    metadata = json.loads(docker("image", "inspect", "--format", "{{json .}}", args.image))
    expected = {"x86_64": "amd64", "aarch64": "arm64", "armv7l": "arm"}[args.arch]
    assert metadata["Architecture"] == expected
    if expected == "arm":
        assert metadata["Variant"] == "v7"
    for numeric_user in (False, True):
        test_image(args.image, args.platform, args.short_sha, numeric_user)


if __name__ == "__main__":
    main()
