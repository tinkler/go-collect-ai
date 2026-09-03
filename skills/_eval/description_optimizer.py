#!/usr/bin/env python3
"""
description_optimizer.py — 基于失败用例的 description 微调

输入:skills/<name>/eval/results/analysis.json
输出:建议改的 description(打印到 stdout)

策略:
  - 收集所有 case.prompt 里的"季节/节日/品类/动作"关键词
  - 比对当前 description
  - 输出"未覆盖的关键词"列表 + 建议加进 description 的文本片段
  - 不会自动改 SKILL.md,仅给建议(让人审)
"""
import argparse
import json
import re
import sys
from pathlib import Path


SKILLS_ROOT = Path(__file__).parent.parent

# 高频触发关键词(中文 + 英文),LLM 看到这些词更可能激活
COMMON_TRIGGERS = [
    "季节", "换季", "应季", "节假日", "节前", "节后", "中秋", "春节", "端午", "清明",
    "国庆", "元旦", "618", "双11", "双12", "雪糕", "月饼", "火锅", "粽子", "啤酒",
    "年货", "开学", "供应商", "结算", "付款", "对账", "堆头", "端架", "促销",
    "退货", "自采", "防回扣", "反回扣", "补货", "备货", "紧急", "P0", "P1", "P2", "P3",
    "fill_rate", "交付率", "回扣", "黑名单", "分摊", "账期", "提单", "建议",
]


def extract_chinese_keywords(text: str) -> set:
    """从 text 提取可能作为触发词的 2-4 字中文词"""
    out = set()
    for w in re.findall(r"[\u4e00-\u9fa5]{2,4}", text):
        out.add(w)
    return out


def suggest(skill_name: str) -> dict:
    root = SKILLS_ROOT / skill_name
    analysis_path = root / "eval" / "results" / "analysis.json"
    cases_path = root / "eval" / "cases.json"
    if not analysis_path.exists() or not cases_path.exists():
        return {"error": f"{skill_name}: 缺 analysis.json 或 cases.json"}

    analysis = json.loads(analysis_path.read_text(encoding="utf-8"))
    cases = json.loads(cases_path.read_text(encoding="utf-8"))

    # 1) 当前 description
    skill_md = (root / "SKILL.md").read_text(encoding="utf-8")
    fm_match = re.match(r"---\n(.*?)\n---", skill_md, re.DOTALL)
    current_desc = ""
    if fm_match:
        m = re.search(r"^description:\s*(.+)$", fm_match.group(1), re.MULTILINE)
        if m:
            current_desc = m.group(1).strip()

    # 2) 收集所有 case.prompt 关键词
    all_keywords = set()
    failed_case_ids = {i["case_id"] for i in analysis.get("issues", [])}
    for case in cases.get("cases", []):
        cid = case["id"]
        prompt = case.get("prompt", "")
        kws = extract_chinese_keywords(prompt)
        all_keywords.update(kws)
        if cid in failed_case_ids:
            # 失败的 case 关键词优先级高
            for kw in kws:
                all_keywords.add(f"❌{kw}")

    # 3) 过滤出"高频但 description 缺的"
    missing = []
    for kw in COMMON_TRIGGERS:
        if kw not in current_desc and kw in "".join(c.get("prompt", "") for c in cases.get("cases", [])):
            missing.append(kw)

    # 4) 输出建议
    additions = []
    if missing:
        additions.append(f"建议在 description 末尾追加关键词:{', '.join(missing[:10])}")
    if len(failed_case_ids) > 0:
        additions.append(f"有 {len(failed_case_ids)} 个失败 case,优先修它们")
    if not missing and not failed_case_ids:
        additions.append("[OK] 全部通过,description 无需调整")

    out = {
        "skill": skill_name,
        "current_description_length": len(current_desc),
        "current_description_keywords": [kw for kw in COMMON_TRIGGERS if kw in current_desc],
        "missing_in_description": missing,
        "failed_case_count": len(failed_case_ids),
        "suggestions": additions,
        "proposed_description_addition": "Use this skill when the user mentions: " + ", ".join(missing) if missing else "",
    }
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("skill", nargs="?", help="skill 名; --all 跑全部")
    ap.add_argument("--all", action="store_true")
    args = ap.parse_args()

    if args.all:
        skills = sorted([
            d.name for d in SKILLS_ROOT.iterdir()
            if d.is_dir() and (d / "eval" / "results" / "analysis.json").exists() and not d.name.startswith("_")
        ])
    elif args.skill:
        skills = [args.skill]
    else:
        ap.print_help()
        sys.exit(1)

    for s in skills:
        out = suggest(s)
        if "error" in out:
            print(f"[ERROR] {out['error']}", file=sys.stderr)
            continue
        print(f"\n{s} description 优化建议:")
        print(f"  当前 description 长度: {out['current_description_length']} 字符")
        print(f"  当前含触发词: {out['current_description_keywords']}")
        print(f"  缺触发词: {out['missing_in_description']}")
        print(f"  失败 case 数: {out['failed_case_count']}")
        for sugg in out["suggestions"]:
            print(f"  -> {sugg}")
        if out["proposed_description_addition"]:
            print(f"\n建议追加到 description 末尾:")
            print(f"  \"{out['proposed_description_addition']}\"")


if __name__ == "__main__":
    main()
