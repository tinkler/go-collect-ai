#!/usr/bin/env python3
"""
analyzer.py — 揭盲后给改进建议(WHY 赢家赢 / 输家输)

输入:grading.json + comparison.json(可选)
输出:analysis.json(issues + metrics + 改进建议)

分类建议(按 high/medium/low 优先级):
  - description_too_vague:description 缺关键词
  - missing_reference:body 没引用 references
  - script_logic_bug:脚本断言失败
  - assertion_weak:断言太弱(可能假阳性)
  - trigger_bug:LLM 调错 skill(我们这里由 description 关键词覆盖检查模拟)
"""
import argparse
import json
import re
import sys
from datetime import datetime
from pathlib import Path


SKILLS_ROOT = Path(__file__).parent.parent


def analyze(skill_name: str) -> dict:
    root = SKILLS_ROOT / skill_name
    grading_path = root / "eval" / "results" / "grading.json"
    if not grading_path.exists():
        print(f"❌ {skill_name}: 找不到 grading.json,先跑 grader.py", file=sys.stderr)
        sys.exit(1)
    grading = json.loads(grading_path.read_text(encoding="utf-8"))

    issues = []
    for case in grading.get("cases", []):
        cid = case["id"]
        prompt = case.get("prompt", "")
        if case["status"] == "pass":
            continue
        for r in case.get("results", []):
            if r["passed"]:
                continue
            check = r.get("check", "")
            info = r.get("info", "")
            # 分类
            category = "unknown"
            suggestion = ""
            priority = "medium"

            if "description 含" in check:
                category = "description_too_vague"
                # 提取缺失的关键词
                m = re.search(r"含 '(.+?)'", check)
                kw = m.group(1) if m else "?"
                suggestion = f"在 description 里加关键词 {kw!r}(用户可能在 prompt 里这么说)"
                priority = "high"
            elif "body 引用" in check:
                category = "missing_reference"
                m = re.search(r"引用 (.+)", check)
                p = m.group(1) if m else "?"
                suggestion = f"在 SKILL.md body 提到 {p} 的位置 + 用法,让 LLM 知道何时调它"
                priority = "medium"
            elif "run_script" in check:
                category = "script_logic_bug"
                suggestion = f"修脚本:{info}"
                priority = "high"
            elif check.startswith("json_field") or check.startswith("regex") or check.startswith("contains"):
                category = "script_output_mismatch"
                suggestion = f"脚本输出不符合 assertion,检查 {check} = {info}"
                priority = "high"
            elif "read_file" in check:
                category = "empty_reference"
                suggestion = f"reference 文件为空或缺失:{info}"
                priority = "medium"

            issues.append({
                "case_id": cid,
                "category": category,
                "priority": priority,
                "check": check,
                "info": info,
                "suggestion": suggestion,
            })

    # 优先级排序:high > medium > low
    pri_rank = {"high": 0, "medium": 1, "low": 2}
    issues.sort(key=lambda x: pri_rank.get(x["priority"], 9))

    # 汇总 metrics
    total = grading["summary"]["total"]
    passed = grading["summary"]["passed"]
    fail_rate = 1 - (passed / total) if total else 0

    # 按 category 聚合
    cat_count = {}
    for iss in issues:
        cat_count[iss["category"]] = cat_count.get(iss["category"], 0) + 1

    out = {
        "skill": skill_name,
        "version": grading.get("version", "unknown"),
        "generated_at": datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "metrics": {
            "total_cases": total,
            "passed": passed,
            "failed": total - passed,
            "pass_rate": grading["summary"]["pass_rate"],
            "fail_rate": round(fail_rate, 4),
            "issue_count": len(issues),
        },
        "category_breakdown": cat_count,
        "issues": issues,
        "next_action": next_action(issues),
    }
    out_path = root / "eval" / "results" / "analysis.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
    return out


def next_action(issues: list) -> str:
    if not issues:
        return "[OK] 全部 pass,继续扩大 eval 覆盖(加新 case)"
    high = [i for i in issues if i["priority"] == "high"]
    if high:
        top = high[0]
        return f"[HIGH] 优先修 {top['category']}:{top['suggestion']}"
    return f"[MED] 修 {issues[0]['category']}:{issues[0]['suggestion']}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("skill", nargs="?", help="skill 名; --all 跑全部")
    ap.add_argument("--all", action="store_true")
    args = ap.parse_args()

    if args.all:
        skills = [d.name for d in SKILLS_ROOT.iterdir() if d.is_dir() and (d / "eval" / "results" / "grading.json").exists() and not d.name.startswith("_")]
    elif args.skill:
        skills = [args.skill]
    else:
        ap.print_help()
        sys.exit(1)

    for s in skills:
        try:
            out = analyze(s)
        except Exception as e:
            print(f"❌ {s}: {e}", file=sys.stderr)
            continue
        m = out["metrics"]
        print(f"\n{s}:")
        print(f"  pass_rate={m['pass_rate']*100:.0f}%  issues={m['issue_count']}")
        print(f"  {out['next_action']}")
        # top 3 issues
        for iss in out["issues"][:3]:
            print(f"  - [{iss['priority']}] {iss['category']} ({iss['case_id']}): {iss['suggestion']}")
        print(f"  → {SKILLS_ROOT / s / 'eval' / 'results' / 'analysis.json'}")


if __name__ == "__main__":
    main()
