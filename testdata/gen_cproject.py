#!/usr/bin/env python3
"""Generate a C project with a configurable number of translation units.

The benchmark needs a workload wide enough for parallelism to show and slow
enough per action that scheduler overhead is not the thing being measured.
These are real compiles, not `sleep` calls: PROJECT_SPEC.md section 8 rules out
synthetic actions, because a sleep does not contend for CPU, memory bandwidth,
or the filesystem the way a compiler does, and those are what actually bound a
real build.

Generation is deterministic: the same --count always produces byte-identical
sources, so two benchmark runs compile the same work.
"""

import argparse
import json
import pathlib
import shutil

UNIT = """\
#include "gen.h"
#include <math.h>

/* Enough arithmetic that -O2 has something to chew on. */
double unit_{i}(double x) {{
    double acc = 0.0;
    for (int k = 0; k < {iters}; k++) {{
        acc += sin(x + k) * cos(x - k) + sqrt(fabs(x) + k);
        acc = fmod(acc, 1.0e6);
    }}
    return acc + {i};
}}
"""


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--count", type=int, default=24, help="translation units")
    ap.add_argument("--iters", type=int, default=64, help="loop iterations per unit")
    ap.add_argument("--out", default="/tmp/buildforge-bench", help="output directory")
    args = ap.parse_args()

    out = pathlib.Path(args.out)
    if out.exists():
        shutil.rmtree(out)
    (out / "src").mkdir(parents=True)

    header = ["#ifndef GEN_H", "#define GEN_H"]
    for i in range(args.count):
        header.append(f"double unit_{i}(double x);")
    header += ["#endif", ""]
    (out / "src" / "gen.h").write_text("\n".join(header))

    for i in range(args.count):
        (out / "src" / f"unit_{i}.c").write_text(UNIT.format(i=i, iters=args.iters))

    main_c = ['#include <stdio.h>', '#include "gen.h"', "", "int main(void) {", "    double t = 0.0;"]
    for i in range(args.count):
        main_c.append(f"    t += unit_{i}(0.5);")
    main_c += ['    printf("%f\\n", t);', "    return 0;", "}", ""]
    (out / "src" / "main.c").write_text("\n".join(main_c))

    actions = []
    objects = []
    for name in ["main"] + [f"unit_{i}" for i in range(args.count)]:
        obj = f"out/{name}.o"
        objects.append(obj)
        actions.append({
            "name": f"compile_{name}",
            "inputs": [f"src/{name}.c", "src/gen.h"],
            "outputs": [obj],
            "command": ["cc", "-O2", "-Isrc", "-c", "-o", obj, f"src/{name}.c"],
        })

    actions.append({
        "name": "link",
        "deps": [a["name"] for a in actions],
        "inputs": objects,
        "outputs": ["out/app"],
        "command": ["cc", "-o", "out/app"] + objects + ["-lm"],
    })

    (out / "build.json").write_text(json.dumps({"actions": actions}, indent=2) + "\n")
    print(f"{out}/build.json: {len(actions)} actions ({args.count + 1} compiles + 1 link)")


if __name__ == "__main__":
    main()
