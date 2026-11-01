#!/usr/bin/env python3
"""Checks docs/wiki/Invariants.md against the code, in both directions.

docs -> code
  * The table is parsed strictly: every line starting with "|" must be the
    header, the separator, or a row "| ID-n | invariant | guards | origin |".
    Anything else is an error, never silently skipped.
  * Only the "Guarded by" column counts. Each row needs at least one guard:
    `TestX`, `FuzzX`, `ExampleX` or `ci:NAME`.
  * Every named test exists in exactly one package (go test -list), and
    passes without being skipped (it is run here with -v).
  * Every `ci:NAME` is a step named NAME in .github/workflows/ci.yml.
code -> docs
  * Every TestLaw_*, TestInv_* and Fuzz* test is a guard in some row.
  * No test file uses a build constraint, which would hide tests from all
    of the above.
Wiki images referenced must exist, and SVGs must be well-formed XML.
"""
import glob, os, re, subprocess, sys
import xml.dom.minidom

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
INV = "docs/wiki/Invariants.md"
errors = []
err = errors.append

def run(*cmd):
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit(f"invariants: {' '.join(cmd)} failed:\n{p.stdout}{p.stderr}")
    return p.stdout

if not os.path.exists(INV):
    sys.exit(f"invariants: {INV} is missing")

# --- parse the table strictly ---------------------------------------------
ROW = re.compile(r"^\| ([A-Z]+-[0-9]+) \| (.+?) \| (.+?) \| (.+?) \|$")
GUARD = re.compile(r"`((?:Test|Fuzz|Example)[A-Za-z0-9_]*|ci:[A-Za-z0-9_-]+)`")
rows, ids = [], set()
for n, line in enumerate(open(INV, encoding="utf-8"), 1):
    line = line.rstrip("\n")
    if not line.startswith("|"):
        continue
    if line == "| ID | Invariant | Guarded by | Came from |" or re.fullmatch(r"\|(---\|){4}", line):
        continue
    m = ROW.match(line)
    if not m:
        err(f"{INV}:{n}: not a valid row (want | ID-n | invariant | guards | origin |): {line[:70]}")
        continue
    rid, _, guards, _ = m.groups()
    if rid in ids:
        err(f"{INV}:{n}: duplicate ID {rid}")
    ids.add(rid)
    g = GUARD.findall(guards)
    if not g:
        err(f"{INV}:{n}: {rid} has no guard in its 'Guarded by' column")
    rows.append((rid, g))
if not rows:
    err(f"{INV}: no invariant rows")

listed_tests = {g for _, gs in rows for g in gs if not g.startswith("ci:")}
listed_ci = {g[3:] for _, gs in rows for g in gs if g.startswith("ci:")}

# --- tests the toolchain knows, per package --------------------------------
where = {}  # test name -> set of packages
if os.path.exists("go.mod"):
    names = []
    for line in run("go", "test", "-list", ".*", "./...").splitlines():
        if re.match(r"^(Test|Fuzz|Example)\w*$", line):
            names.append(line)
        elif line.startswith(("ok ", "ok\t", "?")):
            pkg = line.split()[1]
            for t in names:
                where.setdefault(t, set()).add(pkg)
            names = []

for t in sorted(listed_tests):
    if t not in where:
        err(f"Invariants.md names {t}, but no such test exists")
    elif len(where[t]) > 1:
        err(f"{t} exists in {len(where[t])} packages ({', '.join(sorted(where[t]))}); rename one so the guard is unambiguous")
for t in sorted(where):
    if re.match(r"^(TestLaw_|TestInv_|Fuzz)", t) and t not in listed_tests:
        err(f"{t} guards an invariant but isn't a guard in any row of Invariants.md")

for f in glob.glob("**/*_test.go", recursive=True):
    if re.search(r"^//go:build\b", open(f, encoding="utf-8").read(), re.M):
        err(f"{f} has a build constraint; tests behind one are invisible to this check")

# --- run the listed guards: they must pass and must not skip ---------------
runnable = sorted(t for t in listed_tests if t in where and not t.startswith("Example"))
if runnable:
    p = subprocess.run(["go", "test", "-count=1", "-v", "-run", "^(" + "|".join(runnable) + ")$", "./..."],
                       capture_output=True, text=True)
    for t in re.findall(r"--- SKIP: (\S+)", p.stdout):
        if t.split("/")[0] in listed_tests:
            err(f"guard {t} was skipped; a skipped guard guards nothing")
    if p.returncode != 0:
        err("guard tests failed:\n" + "\n".join(l for l in p.stdout.splitlines() if "FAIL" in l or "_test.go" in l)[:2000])

# --- ci: guards must be real step names ------------------------------------
steps = set()
if os.path.exists(".github/workflows/ci.yml"):
    steps = set(re.findall(r"^\s*- name: ([A-Za-z0-9_-]+)\s*$", open(".github/workflows/ci.yml").read(), re.M))
for g in sorted(listed_ci):
    if g not in steps:
        err(f"Invariants.md names ci:{g}, but ci.yml has no step named {g}")

# --- wiki images ------------------------------------------------------------
for page in glob.glob("docs/wiki/*.md"):
    for img in re.findall(r"\[\[/(images/[^|\]]+)", open(page, encoding="utf-8").read()):
        if not os.path.exists(f"docs/wiki/{img}"):
            err(f"{page} references missing image {img}")
for svg in glob.glob("docs/wiki/images/*.svg"):
    try:
        xml.dom.minidom.parse(svg)
    except Exception as e:
        err(f"invalid SVG {svg}: {e}")

if errors:
    print("\n".join("invariants: " + e for e in errors), file=sys.stderr)
    sys.exit(1)
print(f"invariants: {len(rows)} rows, {len(listed_tests)} guard tests run, {len(listed_ci)} CI guards; docs and code agree")
