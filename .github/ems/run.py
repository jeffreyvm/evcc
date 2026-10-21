#!/usr/bin/env python3
"""Run a command and turn its failure into GitHub annotations.

Job logs of the fork cannot always be downloaded, annotations can be read through
the API (check-runs/<job>/annotations), so a failing step reports its output there.

    run.py <title> -- <command...>       last lines of the output
    run.py <title> --go -- go test -json  failing Go tests with their output
"""

import json
import subprocess
import sys

MAX_LINES = 60
MAX_ANNOTATIONS = 9  # GitHub keeps 10 error annotations per step


def escape(s: str) -> str:
    return s.replace("%", "%25").replace("\r", "").replace("\n", "%0A")


def annotate(title: str, lines: list[str]) -> None:
    body = "\n".join(lines[-MAX_LINES:])
    print(f"::error title={escape(title)}::{escape(body)}", flush=True)


def go_failures(out: str) -> list[tuple[str, list[str]]]:
    output: dict[tuple[str, str], list[str]] = {}
    failed: list[tuple[str, str]] = []
    other: list[str] = []
    for line in out.splitlines():
        try:
            ev = json.loads(line)
        except ValueError:
            other.append(line)  # build errors are not json
            continue
        key = (ev.get("Package", ""), ev.get("Test", ""))
        if ev.get("Action") == "output":
            output.setdefault(key, []).append(ev.get("Output", "").rstrip("\n"))
        elif ev.get("Action") == "fail":
            failed.append(key)

    res = []
    # leaf tests first: a failing parent repeats its subtests
    tests = [k for k in failed if k[1]]
    leaves = [k for k in tests if not any(o[0] == k[0] and o[1].startswith(k[1] + "/") for o in tests)]
    for pkg, test in leaves:
        res.append((f"FAIL {pkg} {test}", output.get((pkg, test), [])))
    for pkg, test in failed:
        if not test and not any(k[0] == pkg for k in leaves):
            res.append((f"FAIL {pkg}", output.get((pkg, ""), [])))
    if other:
        res.append(("go output", other))
    return res


def main() -> int:
    args = sys.argv[1:]
    title = args.pop(0)
    go = False
    if args and args[0] == "--go":
        go = True
        args.pop(0)
    if args and args[0] == "--":
        args.pop(0)

    proc = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    out = proc.stdout

    if go:
        for line in out.splitlines():
            try:
                ev = json.loads(line)
                if ev.get("Action") == "output":
                    sys.stdout.write(ev.get("Output", ""))
            except ValueError:
                print(line)
    else:
        sys.stdout.write(out)
    sys.stdout.flush()

    if proc.returncode != 0:
        if go:
            for t, lines in go_failures(out)[:MAX_ANNOTATIONS]:
                annotate(t, lines)
        else:
            annotate(title, out.splitlines())
    return proc.returncode


if __name__ == "__main__":
    sys.exit(main())
