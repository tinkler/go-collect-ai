#!/usr/bin/env python3
"""
comparator.py — 盲 A/B 对比(不知道哪个 skill 来源)

输入:同一 skill 的两个版本的 grading.json
输出:comparison.json(对每个 case,哪个版本更好)

评分维度(每项 1-5):
  - content:     正确性 / 完整性 / 准确性
  - structure:   组织性 / 格式化 / 可读性
综合 1-10 总分;判定优先级:总分 > 断言通过率 > 平局(极少)

用法:
  python comparator.py <skill>                    # 对比最近 2 个 grading.json
  python comparator.py <skill> --v1 v1.0.0 --v2 v1.0.1
"""
import argparse
import json
import sys
from datetime import datetime
from pathlib import Path


SKILLS_ROOT = Path(__file__).parent.parent


def load_grading(skill_name: str, version: str) -> dict:
    """从 eval/results/grading.json 或从 eval/grading-<version>.json 加载"""
    root = SKILLS_ROOT / skill_name
    # 先找带 version 后缀的
    p = root / "eval" / f"grading-{version}.json"
    if p.exists():
        return json.loads(p.read_text(encoding="utf-8"))
    p = root / "eval" / "results" / "grading.json"
    if p.exists():
        return json.loads(p.read_text(encoding="utf-8"))
    raise FileNotFoundError(f"找不到 {skill_name} 的 grading data")


def score_case(case_result: dict) -> dict:
    """给单个 case 的结果打 content + structure 分(0-5)"""
    results = case_result.get("results", [])
    if not results:
        return {"content": 0, "structure": 0, "total": 0, "rationale": "no results"}

    n_pass = sum(1 for r in results if r["passed"])
    n_total = len(results)
    pass_rate = n_pass / n_total if n_total else 0

    # content: 跟通过率线性相关
    content = min(5, round(pass_rate * 5 + (1 if pass_rate == 1.0 else 0)))

    # structure: 看检查项是否有"info"信息(信息丰富度)
    rich = sum(1 for r in results if r.get("info") and len(r["info"]) > 5)
    structure = min(5, round(rich / n_total * 5))

    total = content * 1 + structure * 1  # 1-10
    return {
        "content": content,
        "structure": structure,
        "total": total,
        "rationale": f"pass_rate={pass_rate:.0%}, rich_info={rich}/{n_total}",
    }


def compare(skill_name: str, a_path: Path, b_path: Path) -> dict:
    a = json.loads(a_path.read_text(encoding="utf-8"))
    b = json.loads(b_path.read_text(encoding="utf-8"))

    a_cases = {c["id"]: c for c in a.get("cases", [])}
    b_cases = {c["id"]: c for c in b.get("cases", [])}
    common_ids = set(a_cases) & set(b_cases)

    blind_runs = []
    a_total_score = 0
    b_total_score = 0
    a_wins = 0
    b_wins = 0
    ties = 0

    for cid in common_ids:
        a_score = score_case(a_cases[cid])
        b_score = score_case(b_cases[cid])
        a_total_score += a_score["total"]
        b_total_score += b_score["total"]

        if a_score["total"] > b_score["total"]:
            winner = "a"
            a_wins += 1
        elif b_score["total"] > a_score["total"]:
            winner = "b"
            b_wins += 1
        else:
            winner = "tie"
            ties += 1

        blind_runs.append({
            "case_id": cid,
            "a": a_score,
            "b": b_score,
            "winner": winner,
            "rationale": f"A={a_score['total']} vs B={b_score['total']} → {winner}",
        })

    out = {
        "skill": skill_name,
        "a_path": str(a_path),
        "b_path": str(b_path),
        "generated_at": datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "summary": {
            "total_cases": len(common_ids),
            "a_total_score": a_total_score,
            "b_total_score": b_total_score,
            "a_wins": a_wins,
            "b_wins": b_wins,
            "ties": ties,
            "winner": "a" if a_total_score > b_total_score else ("b" if b_total_score > a_total_score else "tie"),
        },
        "blind_runs": blind_runs,
    }
    out_path = SKILLS_ROOT / skill_name / "eval" / "results" / "comparison.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("skill")
    ap.add_argument("--v1", default="v1")
    ap.add_argument("--v2", default="v2")
    args = ap.parse_args()

    root = SKILLS_ROOT / args.skill / "eval"
    a = root / f"grading-{args.v1}.json"
    b = root / f"grading-{args.v2}.json"
    if not a.exists() or not b.exists():
        # 退化:拿最近的 2 个 grading.json
        candidates = sorted(root.glob("grading*.json"))
        if len(candidates) < 2:
            print(f"❌ {args.skill} 找不到 2 个 grading 数据(需要至少 2 次评测)", file=sys.stderr)
            sys.exit(1)
        a, b = candidates[-2], candidates[-1]

    out = compare(args.skill, a, b)
    s = out["summary"]
    print(f"\n{args.skill} A/B:")
    print(f"  A: {a.name} score={s['a_total_score']} wins={s['a_wins']}")
    print(f"  B: {b.name} score={s['b_total_score']} wins={s['b_wins']}")
    print(f"  winner: {s['winner'].upper()}")
    print(f"  → {SKILLS_ROOT / args.skill / 'eval' / 'results' / 'comparison.json'}")


if __name__ == "__main__":
    main()
