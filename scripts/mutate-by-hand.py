#!/usr/bin/env python3
"""Apply hand-written mutants one at a time and run go test against each.

gremlins only mutates operators, and reports mutants on lines Go's coverage
does not instrument (tagless-switch case expressions, constant declarations)
as NOT COVERED. This script checks those, and statement-level changes such
as a reverted fix or a deleted call, by hand. See docs/mutation/scope.md.

Usage: scripts/mutate-by-hand.py <file> <test-regex|-> <spec>...
  spec: LINE::OLD::NEW  (OLD must occur exactly once on LINE)
A test regex of '-' runs the whole package. The file is restored after
every mutant, and on any exit. Run from the repository root, and do not
edit source files while it runs.
"""
import subprocess, sys, pathlib, atexit
path = pathlib.Path(sys.argv[1]); regex = sys.argv[2]; specs = sys.argv[3:]
orig = path.read_text()
atexit.register(lambda: path.write_text(orig))
lines = orig.split("\n")
results = []
for spec in specs:
    ln, old, new = spec.split("::")
    ln = int(ln)
    line = lines[ln - 1]
    assert line.count(old) == 1, (spec, line)
    mutated = lines[:]
    mutated[ln - 1] = line.replace(old, new)
    path.write_text("\n".join(mutated))
    cmd = ["go", "test", "-count=1", "."]
    if regex != "-":
        cmd[3:3] = ["-run", regex]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=900)
    status = "KILLED" if r.returncode != 0 else "LIVED"
    if "build failed" in r.stdout + r.stderr or "[build failed]" in r.stdout:
        status = "NOT VIABLE"
    results.append((status, spec))
    print(f"{status:10} {path.name}:{spec}", flush=True)
    path.write_text(orig)
print("killed", sum(1 for s, _ in results if s == "KILLED"), "lived", sum(1 for s, _ in results if s == "LIVED"))
