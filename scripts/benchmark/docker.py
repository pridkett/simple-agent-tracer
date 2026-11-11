"""Measure a fresh scratch image with a named volume and a generated OTLP dataset."""

import argparse
import gzip
import json
import os
import secrets
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def memory(container):
    usage = docker("stats", "--no-stream", "--format", "{{.MemUsage}}", container)
    status = docker("run", "--rm", f"--pid=container:{container}",
                    "alpine:3.23", "cat", "/proc/1/status")
    for line in status.splitlines():
        if line.startswith(("VmRSS:", "Uid:")):
            print(f"{container} {line}", flush=True)
    print(f"{container} container memory {usage}", flush=True)


def wait_for(check):
    for _ in range(150):
        try:
            if check():
                return
        except OSError:
            pass
        time.sleep(0.05)
    raise RuntimeError("service did not become ready")


def run(args):
    with tempfile.TemporaryFile() as compressed:
        with subprocess.Popen(["docker", "save", args.image], stdout=subprocess.PIPE) as saved:
            with gzip.GzipFile(fileobj=compressed, mode="wb", mtime=0) as output:
                for chunk in iter(lambda: saved.stdout.read(1 << 20), b""):
                    output.write(chunk)
            if saved.wait() != 0:
                raise RuntimeError("unable to export image")
        print(f"Compressed docker save: {compressed.tell():,} bytes", flush=True)

    tag = "tracer-bench-" + secrets.token_hex(4)
    container, database, network, volume = (f"{tag}-{kind}" for kind in ("app", "db", "net", "data"))
    read_token, ingest_token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    credentials = json.dumps([
        {"access": "read", "project": "default", "token": read_token},
        {"access": "ingest", "project": "default", "source": "benchmark", "token": ingest_token},
    ])
    pg = args.backend == "postgres"
    try:
        if pg:
            docker("network", "create", network)
            password = secrets.token_hex(16)
            docker("run", "-d", "--name", database, "--network", network,
                   "-v", f"{volume}:/var/lib/postgresql/data",
                   "-e", f"POSTGRES_PASSWORD={password}", args.postgres_image)
            wait_for(lambda: subprocess.run(
                ["docker", "exec", database, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            ).returncode == 0)
            dsn = f"postgres://postgres:{password}@{database}:5432/postgres?sslmode=disable"
            storage = ["--network", network, "-e", f"TRACER_DATABASE_URL={dsn}"]
        else:
            storage = ["-v", f"{volume}:/data"]
        start = time.monotonic()
        docker("run", "-d", "--name", container, "-p", "127.0.0.1::4318",
               "-e", "TRACER_BIND=0.0.0.0:4318", "-e", f"TRACER_CREDENTIALS={credentials}",
               *storage, args.image)
        address = docker("port", container, "4318/tcp")
        endpoint = "http://" + address

        def ready():
            with urllib.request.urlopen(endpoint + "/ready", timeout=1) as response:
                return json.load(response)["status"] == "ready"

        wait_for(ready)
        print(f"Startup to ready: {(time.monotonic() - start) * 1000:.1f} ms", flush=True)
        for state, idle in [("Fresh", 10), ("Warm", args.idle_seconds)]:
            if state == "Warm":
                env = {
                    **os.environ,
                    "TRACER_BENCH_READ_TOKEN": read_token,
                    "TRACER_BENCH_INGEST_TOKEN": ingest_token,
                }
                subprocess.run(
                    ["go", "run", "./scripts/benchmark", "-url", endpoint,
                     "-spans", str(args.spans), "-samples", str(args.samples)],
                    cwd=ROOT, env=env, check=True,
                )
            print(f"{state} idle after {idle} seconds", flush=True)
            time.sleep(idle)
            memory(container)
            if pg:
                memory(database)
    finally:
        for name in (container, database) if pg else (container,):
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if pg:
            subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="simple-agent-tracer")
    parser.add_argument("--backend", choices=["sqlite", "postgres"], default="sqlite")
    parser.add_argument("--postgres-image", default="postgres:17-alpine")
    parser.add_argument("--spans", type=int, default=100_000)
    parser.add_argument("--samples", type=int, default=30)
    parser.add_argument("--idle-seconds", type=int, default=30)
    run(parser.parse_args())
