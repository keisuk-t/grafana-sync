#!/usr/bin/env python3
"""Test the CLI against schemas and ciphertext created by real Grafana servers.

All credentials and encryption keys here are for disposable local containers.
Never accepts an external database DSN.
"""

import base64
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import traceback
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parent.parent
VERSION = os.environ.get("GRAFANA_TEST_VERSION", "13.2.4")
PROBE_IMAGE = f"grafana-sync-header-probe:{os.getpid()}"
os.environ["GRAFANA_TEST_PROBE_IMAGE"] = PROBE_IMAGE
ENGINES = ("mysql", "postgres")
TABLES = ("secrets", "data_source", "data_keys", "org")
COMPOSE = [
    "docker", "compose", "-p", f"grafana-sync-real-{os.getpid()}",
    "-f", str(ROOT / "compose.grafana-test.yaml"),
]


def run(args, **kwargs):
    return subprocess.run(
        args, check=True, text=True, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, timeout=600, cwd=ROOT, **kwargs,
    ).stdout.strip()


def compose(*args):
    return run(COMPOSE + list(args))


def sql(engine, statement):
    if engine == "mysql":
        args = ["mysql", "-ugrafana", "-plocal-test-password",
                "-N", "-B", "grafana_sync_test", "-e", statement]
    else:
        args = ["psql", "-U", "grafana", "-d", "grafana_sync_test",
                "-v", "ON_ERROR_STOP=1", "-Atc", statement]
    return compose("exec", "-T", engine, *args)


def clear(engine):
    # Delete rows only in our isolated Compose databases, with Grafana stopped.
    sql(engine, "BEGIN; " + " ".join(f"DELETE FROM {table};" for table in TABLES)
        + " COMMIT;")


def api(engine, path, payload=None):
    address = compose("port", f"grafana-{engine}", "3000")
    request = urllib.request.Request(
        f"http://{address}{path}",
        data=None if payload is None else json.dumps(payload).encode(),
        headers={
            "Authorization": "Basic " + base64.b64encode(
                b"admin:local-test-admin").decode(),
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        print(f"{engine} {path}: HTTP {error.code}: {error.read().decode()}", flush=True)
        raise


def start(engine):
    compose("up", "-d", f"grafana-{engine}")
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        try:
            health = api(engine, "/api/health")
            if health["database"] == "ok":
                assert health["version"] == VERSION, health
                # /api/health alone does not establish authenticated API readiness.
                api(engine, "/api/org")
                return
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            time.sleep(1)
    raise RuntimeError(f"Grafana {engine} did not become ready")


def stop(engine):
    compose("stop", f"grafana-{engine}")


def cli(engine, command, path, *args):
    address = compose("port", engine, "3306" if engine == "mysql" else "5432")
    dsn = (f"grafana:local-test-password@tcp({address})/grafana_sync_test"
           if engine == "mysql" else
           f"postgres://grafana:local-test-password@{address}/grafana_sync_test?sslmode=disable")
    return run(
        [str(ROOT / "bin/grafana-sync"), command, "-db", engine,
         "-file", str(path), *args],
        env={**os.environ, "GRAFANA_SYNC_DSN": dsn},
    )


def canonical(snapshot):
    # Compare values by column name: physical column order can differ by engine.
    return {
        table: sorted(
            [dict(zip(snapshot[table]["columns"], row))
             for row in snapshot[table]["rows"]],
            key=lambda row: row["name" if table == "data_keys" else "id"],
        )
        for table in TABLES
    }


def check_proxy(engine, names):
    for name in names:
        result = api(engine, f"/api/datasources/proxy/uid/sync-{name}/api/v1/query")
        assert result == {"secret": f"secret-{name}"}, (engine, name, result)


def test(directory):
    print(f"Preparing disposable databases for Grafana {VERSION}", flush=True)
    compose("up", "-d", "--wait", "--wait-timeout", "180", *ENGINES, "header-probe")
    paths = {}
    for engine in ENGINES:
        print(f"Creating Grafana {VERSION} schema and encrypted fixtures: {engine}", flush=True)
        start(engine)
        for name in ("old", "current"):
            # Force a new envelope key after the first datasource was encrypted.
            if name == "current":
                stop(engine)
                sql(engine, "UPDATE data_keys SET active = " +
                    ("false" if engine == "postgres" else "0"))
                start(engine)
            api(engine, "/api/datasources", {
                "name": name, "uid": f"sync-{name}", "type": "prometheus",
                "access": "proxy",
                "url": "http://header-probe:8080",
                "jsonData": {
                    "httpHeaderName1": "X-Sync-Test",
                    # Grafana 13 widens MySQL json_data from TEXT to MEDIUMTEXT.
                    "padding": "日" * (30000 if VERSION.startswith("13.") else 100),
                },
                "secureJsonData": {"httpHeaderValue1": f"secret-{name}"},
            })
        check_proxy(engine, ("old", "current"))
        stop(engine)
        if engine == "mysql" and VERSION.startswith("13."):
            kind = sql(engine, "SELECT DATA_TYPE FROM information_schema.COLUMNS "
                       "WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='data_source' "
                       "AND COLUMN_NAME='json_data'")
            assert kind == "mediumtext", kind
        path = directory / f"{engine}.json"
        cli(engine, "export", path)
        snapshot = json.loads(path.read_text())
        keys = canonical(snapshot)["data_keys"]
        assert {key["active"] for key in keys} == {"0", "1"}, keys
        assert len(snapshot["secrets"]["rows"]) == 2
        assert len(snapshot["data_source"]["rows"]) == 2
        paths[engine] = path

    for source in ENGINES:
        snapshot = json.loads(paths[source].read_text())
        for target in ENGINES:
            for mode in ("require-empty", "insert-only", "sync"):
                print(f"Grafana {VERSION}: {source} -> {target} ({mode})", flush=True)
                clear(target)
                args = ["--mode", mode]
                if mode == "sync":
                    args += ["--allow-delete"]
                    # Assert dry-run does not create any of the four table rows.
                    cli(target, "import", paths[source], "--mode", "sync", "--dry-run")
                    for table in TABLES:
                        assert sql(target, f"SELECT COUNT(*) FROM {table}") == "0"
                cli(target, "import", paths[source], *args)
                if mode != "require-empty":
                    output = cli(target, "import", paths[source], *args)
                    assert "inserted=0" in output, output
                actual_path = directory / f"{source}-{target}-{mode}.json"
                cli(target, "export", actual_path)
                expected = copy.deepcopy(snapshot)
                if mode == "sync":
                    index = expected["data_keys"]["columns"].index("active")
                    for row in expected["data_keys"]["rows"]:
                        row[index] = "0"
                assert canonical(json.loads(actual_path.read_text())) == canonical(expected)
                start(target)
                check_proxy(target, ("old", "current"))
                # Verify the >64 KiB JSON survived and is readable by Grafana.
                ds = api(target, "/api/datasources/uid/sync-current")
                padding = "日" * (30000 if VERSION.startswith("13.") else 100)
                assert ds["jsonData"]["padding"] == padding
                stop(target)
    print(f"PASS: Grafana {VERSION}, 4 routes x 3 import modes, "
          "current/inactive key decryption and JSON preservation", flush=True)


def main():
    probe_built = False
    try:
        with tempfile.TemporaryDirectory(prefix="grafana-sync-real-") as directory:
            # Buildx otherwise tries to write its state into ~/.docker.
            run(["docker", "build", "-t", PROBE_IMAGE, "-f",
                 str(ROOT / "scripts/grafana-probe/Dockerfile"), str(ROOT / "bin")],
                env={**os.environ, "BUILDX_CONFIG": str(Path(directory) / "buildx")})
            probe_built = True
            test(Path(directory))
    except subprocess.CalledProcessError as error:
        # Output is from disposable test services and fixed public test credentials.
        print(error.stdout)
        print(error.stderr)
        raise
    except Exception:
        traceback.print_exc()
        print(compose("logs", "--tail", "40", "grafana-mysql", "grafana-postgres"))
        raise
    finally:
        try:
            compose("down", "-v", "--remove-orphans")
        finally:
            if probe_built:
                run(["docker", "image", "rm", PROBE_IMAGE])


if __name__ == "__main__":
    main()
