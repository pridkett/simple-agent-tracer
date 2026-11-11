# Tracer footprint and query benchmark

Run this from the repository root with Docker, Go, and Python 3. The driver starts a fresh container and named volume for each run. The PostgreSQL run also starts an isolated database container and measures it separately.

```sh
docker build -t simple-agent-tracer .
docker pull alpine:3.23
docker pull postgres:17-alpine
python3 scripts/benchmark/docker.py --backend sqlite
python3 scripts/benchmark/docker.py --backend postgres
```

The driver creates 100,000 spans in OTLP batches of 500. The fixture has five spans per trace, roughly five traces per session, two model calls per trace, and synthetic captured input and output averaging approximately 1 KiB per model span. It records startup from the start of `docker run` to the first successful readiness response. After one warm-up request per route it measures 30 HTTP requests per route. The memory samples use `VmRSS` for PID 1 and `docker stats` for container memory, at 10 seconds of fresh idle and 30 seconds of idle after the query run. For PostgreSQL, PID 1 is the postmaster and the container measurement includes the other database processes.

## Results

The runs used an Apple M5 Pro with 64 GiB RAM on macOS, Docker Desktop's Linux arm64 VM with 8 vCPUs and 7.65 GiB available RAM, and PostgreSQL 17 Alpine. The scratch image ran as UID/GID 65532. Its `docker save` archive compressed with gzip level 9 was 7,580,991 bytes (7.23 MiB). The image's CA bundle was exercised by HTTPS OIDC discovery in the container. A separate TLS fixture covered signed owner login and session authorization.

| Metric | SQLite | PostgreSQL |
| --- | ---: | ---: |
| Startup to ready | 128.3 ms | 129.9 ms |
| Ingest 100,000 spans | 25.742 s | 139.992 s |
| Fresh tracer process RSS | 21.78 MiB | 22.91 MiB |
| Warm tracer process RSS | 32.80 MiB | 28.81 MiB |
| Fresh tracer container memory | 8.02 MiB | 9.34 MiB |
| Warm tracer container memory | 46.51 MiB | 19.04 MiB |
| Fresh database postmaster RSS | — | 26.69 MiB |
| Warm database postmaster RSS | — | 26.69 MiB |
| Fresh database container memory | — | 25.93 MiB |
| Warm database container memory | — | 237 MiB |

| Query | SQLite p50 / p95 | PostgreSQL p50 / p95 |
| --- | ---: | ---: |
| Session list | 12.180 / 13.066 ms | 12.157 / 12.764 ms |
| Sessions with failed `search` child | 92.543 / 100.557 ms | 63.921 / 78.619 ms |
| Trace list | 2.345 / 3.102 ms | 5.226 / 5.861 ms |
| Session timeline | 0.582 / 0.732 ms | 0.729 / 1.191 ms |
| Span detail | 0.347 / 0.391 ms | 0.435 / 0.649 ms |

Container memory and RSS come from different accounting sources. Docker's Linux memory statistic subtracts inactive file cache. SQLite met the provisional 50 MiB warm process RSS target on this fixture.
