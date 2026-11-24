#!/usr/bin/env python3
"""Probe a Codex stdio executor profile against a real VM; no model/API call."""

import argparse
import base64
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import tomllib
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("profile", type=Path)
    parser.add_argument("--workspace", type=Path, required=True)
    args = parser.parse_args()
    config = tomllib.loads((args.profile / "environments.toml").read_text())
    assert config["include_local"] is False, "local fallback must be disabled"
    entry = next(e for e in config["environments"] if e["id"] == config["default"])
    workspace = args.workspace.resolve()
    token = "vmjail-codex-" + uuid.uuid4().hex
    marker = workspace / ("." + token)
    messages = queue.Queue()

    with tempfile.TemporaryFile() as errors:
        proc = subprocess.Popen(
            [entry["program"], *entry["args"]],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=errors,
            cwd=entry.get("cwd", str(workspace)),
            env={**os.environ, **entry.get("env", {})},
        )

        def receive():
            try:
                for line in proc.stdout:
                    messages.put(json.loads(line))
            except Exception as error:
                messages.put(error)
            finally:
                messages.put(EOFError("executor closed stdout"))

        threading.Thread(target=receive, daemon=True).start()

        def send(message):
            proc.stdin.write((json.dumps(message) + "\n").encode())
            proc.stdin.flush()

        def read():
            message = messages.get(timeout=30)
            if isinstance(message, Exception):
                raise message
            if "error" in message:
                raise RuntimeError(message["error"])
            return message

        def command(number, script):
            process_id = str(number)
            send({"id": number, "method": "process/start", "params": {
                "processId": process_id,
                "argv": [guest_shell, "-c", script, "probe", str(marker), token],
                "cwd": workspace.as_uri(),
                "env": {"PATH": "/run/current-system/sw/bin:/bin"},
                "tty": False,
                "pipeStdin": False,
            }})
            output = bytearray()
            while True:
                message = read()
                params = message.get("params", {})
                if params.get("processId") != process_id:
                    continue
                if message.get("method") == "process/output":
                    output.extend(base64.b64decode(params["chunk"]))
                if message.get("method") == "process/exited":
                    return params["exitCode"], output.decode()

        try:
            send({"id": 1, "method": "initialize", "params": {"clientName": "vmjail-smoke"}})
            response = read()
            assert response["id"] == 1 and response["result"]["sessionId"]
            guest_shell = response["result"]["environmentInfo"]["shell"]["path"]
            send({"method": "initialized", "params": {}})

            code, output = command(2, 'cat /proc/sys/kernel/osrelease; '
                                   'printf "%s" "$2" > "$1"; '
                                   'printf "%s" "$2" > "/tmp/$2"; exit 7')
            assert code == 7, (code, output)
            guest_kernel = output.strip()
            assert guest_kernel and guest_kernel != os.uname().release, output
            assert marker.read_text() == token, "guest write did not reach host workspace"

            code, output = command(3, 'test "$(cat "/tmp/$2")" = "$2" && '
                                   'test "$(cat "$1")" = "$2" && echo persistent-vm-ok')
            assert code == 0 and "persistent-vm-ok" in output, (code, output)
            print(f"Command checks passed in guest {guest_kernel}; checking teardown...", flush=True)
        except Exception:
            errors.seek(0)
            print(errors.read().decode(errors="replace")[-4000:])
            raise
        finally:
            proc.stdin.close()
            try:
                proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                proc.terminate()
                proc.wait(timeout=10)
            proc.stdout.close()
            marker.unlink(missing_ok=True)
        if proc.returncode != 0:
            errors.seek(0)
            raise RuntimeError(f"executor teardown {proc.returncode}: " + errors.read().decode(errors="replace")[-4000:])
        print("Codex executor smoke passed, including teardown")


if __name__ == "__main__":
    main()
