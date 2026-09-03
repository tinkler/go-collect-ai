#!/usr/bin/env python3
"""
run_eval.py — 一键:Grader → Comparator → Analyzer

用法:
  python run_eval.py <skill>
  python run_eval.py --all
"""
import argparse
import importlib.util
import sys
from pathlib import Path


THIS_DIR = Path(__file__).parent
SKILLS_ROOT = THIS_DIR.parent


def run_module(mod_name: str, fn: str, *args):
    spec = importlib.util.spec_from_file_location(mod_name, THIS_DIR / f"{mod_name}.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return getattr(mod, fn)(*args)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("skill", nargs="?", default="--all")
    ap.add_argument("--all", action="store_true")
    args = ap.parse_args()

    if args.all:
        skills = sorted([
            d.name for d in SKILLS_ROOT.iterdir()
            if d.is_dir() and (d / "eval" / "cases.json").exists() and not d.name.startswith("_")
        ])
    else:
        skills = [args.skill]

    print(f"=== run_eval: {skills} ===\n")

    for s in skills:
        print(f"--- {s} ---")
        print(f"[1/3] Grader ...")
        run_module("grader", "grade_skill", s)
        try:
            print(f"[2/3] Analyzer ...")
            run_module("analyzer", "analyze", s)
        except SystemExit:
            pass
        print()

    print("=== Done ===")
    print(f"结果在每个 skill 的 eval/results/ 下:")
    print(f"  - grading.json     # Grader 输出")
    print(f"  - analysis.json    # Analyzer 输出(改进建议)")


if __name__ == "__main__":
    main()
