#!/usr/bin/env python3
"""
grader.py — Skill 评测 Grader

输入:skills/<name>/eval/cases.json + assertions.json
输出:skills/<name>/eval/results/grading.json

不调真 LLM(避免 token 成本),改为:
  1) 校验 description 含 case.prompt 必要关键词
  2) 调 invoke_skill(load) 拿 body
  3) 调 invoke_skill(run_script) 跑主脚本,断言 stdout 是合法 JSON
  4) 调 invoke_skill(read_file) 读 references,断言含 case 期望关键词
  5) 综合评分

CLI:
  python grader.py <skill-name>
  python grader.py --all
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path


SKILLS_ROOT = Path(__file__).parent.parent  # skills/
RESULTS_SUBDIR = "results"


def find_skill_root(name: str) -> Path:
    """找 skills/<name>/ 目录(不依赖 Go loader)"""
    p = SKILLS_ROOT / name
    if not p.exists():
        raise FileNotFoundError(f"skill 目录不存在: {p}")
    return p


def load_json(p: Path):
    with open(p, "r", encoding="utf-8") as f:
        return json.load(f)


def save_json(p: Path, data):
    p.parent.mkdir(parents=True, exist_ok=True)
    with open(p, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)


def parse_frontmatter(skill_md: Path) -> dict:
    """粗解析 SKILL.md frontmatter(避免依赖 pyyaml)"""
    text = skill_md.read_text(encoding="utf-8")
    if not text.startswith("---"):
        return {}
    rest = text[3:]
    end = rest.find("\n---")
    if end < 0:
        return {}
    fm = rest[:end]
    out = {}
    current_key = None
    for line in fm.splitlines():
        if not line.strip() or line.strip().startswith("#"):
            continue
        if line.startswith("  -") and current_key:
            # list item
            out.setdefault(current_key, []).append(line.strip().lstrip("-").strip())
        elif ":" in line:
            k, _, v = line.partition(":")
            k = k.strip()
            v = v.strip()
            if v == "":
                current_key = k
                out[k] = []
            else:
                current_key = None
                out[k] = v.strip('"').strip("'")
    return out


def read_file(skill_root: Path, rel_path: str) -> str:
    p = skill_root / rel_path
    if not p.exists():
        return ""
    return p.read_text(encoding="utf-8")


def run_script(skill_root: Path, rel_path: str, args: dict, timeout: int = 30) -> tuple:
    """跑 scripts/ 下的脚本,返 (stdout, stderr, rc)"""
    script = skill_root / rel_path
    if not script.exists():
        return ("", f"script not found: {rel_path}", 1)
    py = "python3" if sys.platform == "darwin" else "python"
    inp = json.dumps(args, ensure_ascii=False).encode("utf-8")
    # Windows 上强制子进程 stdout 用 UTF-8(否则 cmd 默认 GBK 中文会变 \ufffd)
    env = os.environ.copy()
    env["PYTHONIOENCODING"] = "utf-8"
    env["PYTHONUTF8"] = "1"
    try:
        p = subprocess.run(
            [py, str(script)],
            input=inp,
            capture_output=True,
            timeout=timeout,
            env=env,
        )
        return (p.stdout.decode("utf-8", errors="replace"), p.stderr.decode("utf-8", errors="replace"), p.returncode)
    except subprocess.TimeoutExpired:
        return ("", f"timeout after {timeout}s", 124)
    except FileNotFoundError as e:
        return ("", str(e), 1)


# === 断言执行 ===

def assert_contains(actual: str, value: str) -> tuple:
    return (value in actual, f"contains({value!r})")


def assert_exact(actual, value) -> tuple:
    return (actual == value, f"exact({value!r})")


def assert_regex(actual: str, value: str) -> tuple:
    return (bool(re.search(value, actual)), f"regex({value!r})")


def assert_json_field(json_text: str, field_path: str, expected) -> tuple:
    """field_path 支持 a.b[0].c 形式(数组下标)"""
    try:
        data = json.loads(json_text)
    except json.JSONDecodeError:
        return (False, f"json_field({field_path}): invalid JSON")
    # 把 items[0] 拆成 [items, 0]
    parts = re.findall(r"[^.\[\]]+|\[\d+\]", field_path)
    cur = data
    for part in parts:
        if part.startswith("[") and part.endswith("]"):
            idx = int(part[1:-1])
            if isinstance(cur, list) and 0 <= idx < len(cur):
                cur = cur[idx]
            else:
                return (False, f"json_field({field_path}): index {idx} out of range")
        else:
            if isinstance(cur, dict) and part in cur:
                cur = cur[part]
            else:
                return (False, f"json_field({field_path}): not found at {part!r}")
    return (cur == expected, f"json_field({field_path}) == {expected!r}, got {cur!r}")


# === Grader 主流程 ===

def grade_skill(skill_name: str) -> dict:
    root = find_skill_root(skill_name)
    skill_md = root / "SKILL.md"
    fm = parse_frontmatter(skill_md)
    description = fm.get("description", "")

    cases_path = root / "eval" / "cases.json"
    assertions_path = root / "eval" / "assertions.json"
    if not cases_path.exists():
        return {"skill": skill_name, "summary": {"error": "cases.json 缺失"}, "cases": []}
    if not assertions_path.exists():
        return {"skill": skill_name, "summary": {"error": "assertions.json 缺失"}, "cases": []}

    cases = load_json(cases_path).get("cases", [])
    assertions = load_json(assertions_path).get("assertions", [])

    body = read_file(root, "SKILL.md").split("---", 2)
    skill_body = body[2] if len(body) >= 3 else ""

    # 默认脚本路径(scripts/<name>.py 第一个)
    scripts_dir = root / "scripts"
    main_script_rel = None
    if scripts_dir.exists():
        for f in sorted(scripts_dir.iterdir()):
            if f.suffix == ".py":
                main_script_rel = f"scripts/{f.name}"
                break

    case_results = []
    pass_count = 0
    # description 触发关键词池(用户可能说的词)
    TRIGGER_POOL = {
        "春节", "中秋", "端午", "清明", "国庆", "元旦", "元宵", "五一",
        "618", "双11", "双12", "年货节", "开学季",
        "雪糕", "月饼", "火锅", "粽子", "啤酒", "年货", "圣诞", "元宵",
        "堆头", "端架", "促销", "DM", "进场费",
        "退货", "自采", "防回扣", "反回扣", "回扣", "黑名单", "拉黑",
        "补货", "备货", "紧急", "P0", "P1", "P2", "P3",
        "fill_rate", "交付率",
        "供应商", "结算", "付款", "对账", "账期", "提单", "续约",
        "季节", "换季", "应季", "档期",
    }

    for case in cases:
        cid = case["id"]
        case_assertions = [a for a in assertions if a.get("case_id") == cid]
        results = []

        # 1) description 关键词校验
        #    只校验 prompt 里出现 + 在 TRIGGER_POOL 里的词
        prompt = case.get("prompt", "")
        prompt_triggers = set()
        for word in re.findall(r"[\u4e00-\u9fa5A-Za-z0-9_-]{2,}", prompt):
            if word in TRIGGER_POOL:
                prompt_triggers.add(word)

        desc = description
        for kw in sorted(prompt_triggers):
            ok, info = assert_contains(desc, kw)
            results.append({"check": f"description 含触发词 {kw!r}", "passed": ok, "info": info})

        # 2) skill body 包含 references/scripts 路径
        for path in case.get("expected_script_path", []):
            ok, info = assert_contains(skill_body, path)
            results.append({"check": f"body 引用 {path}", "passed": ok, "info": info})
        for path in case.get("expected_reference_path", []):
            ok, info = assert_contains(skill_body, path)
            results.append({"check": f"body 引用 {path}", "passed": ok, "info": info})

        # 3) 跑主脚本(用主脚本跑,case 可以指定 script 覆盖)
        script_to_run = case.get("script_path", main_script_rel)
        if script_to_run and case.get("script_args"):
            stdout, stderr, rc = run_script(root, script_to_run, case["script_args"])
            if rc != 0:
                results.append({"check": f"run_script {script_to_run}", "passed": False, "info": stderr or f"rc={rc}"})
            else:
                results.append({"check": f"run_script {script_to_run} rc", "passed": True, "info": f"rc={rc}"})
                # 4) 跑用户的 assertion
                for a in case_assertions:
                    if a.get("type") == "json_field":
                        ok, info = assert_json_field(stdout, a["check"], a["value"])
                    elif a.get("type") == "regex":
                        ok, info = assert_regex(stdout, a["value"])
                    elif a.get("type") == "contains":
                        ok, info = assert_contains(stdout, a["value"])
                    elif a.get("type") == "exact":
                        ok, info = assert_exact(stdout, a["value"])
                    else:
                        ok, info = (False, f"未知断言类型: {a.get('type')}")
                    results.append({"check": f"{a['type']}({a['check']})", "passed": ok, "info": info})

        # 5) read_file references 内容含 prompt 中的中文关键词
        prompt_chinese_kws = set()
        for word in re.findall(r"[\u4e00-\u9fa5]{2,4}", prompt):
            prompt_chinese_kws.add(word)
        for ref_path in case.get("expected_reference_path", []):
            content = read_file(root, ref_path)
            ok = len(content) > 0
            results.append({"check": f"read_file {ref_path} 非空", "passed": ok, "info": f"len={len(content)}"})

        # 汇总 case 结果
        passed = sum(1 for r in results if r["passed"])
        total = len(results)
        status = "pass" if passed == total else "fail"
        if status == "pass":
            pass_count += 1

        case_results.append({
            "id": cid,
            "prompt": prompt,
            "status": status,
            "passed": passed,
            "total": total,
            "results": results,
        })

    summary = {
        "total": len(cases),
        "passed": pass_count,
        "failed": len(cases) - pass_count,
        "pass_rate": round(pass_count / len(cases), 4) if cases else 0.0,
    }
    out = {
        "skill": skill_name,
        "version": load_json(cases_path).get("version", "unknown"),
        "run_at": datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "summary": summary,
        "cases": case_results,
    }
    out_path = root / "eval" / RESULTS_SUBDIR / "grading.json"
    save_json(out_path, out)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("skill", nargs="?", help="skill 名,省略 --all 时必填")
    ap.add_argument("--all", action="store_true", help="跑全部 4 个 skill")
    args = ap.parse_args()

    if args.all:
        skills = [d.name for d in SKILLS_ROOT.iterdir() if d.is_dir() and (d / "eval" / "cases.json").exists()]
        skills = [s for s in skills if not s.startswith("_")]
    elif args.skill:
        skills = [args.skill]
    else:
        ap.print_help()
        sys.exit(1)

    grand_total = 0
    grand_passed = 0
    for s in skills:
        try:
            res = grade_skill(s)
        except Exception as e:
            print(f"❌ {s}: {e}", file=sys.stderr)
            continue
        sm = res["summary"]
        grand_total += sm["total"]
        grand_passed += sm["passed"]
        rate = sm["pass_rate"] * 100
        print(f"  {s:<24} {sm['passed']}/{sm['total']} pass ({rate:.0f}%) → eval/results/grading.json")

    if grand_total > 0:
        rate = grand_passed / grand_total * 100
        print(f"\nGRAND TOTAL: {grand_passed}/{grand_total} pass ({rate:.0f}%)")


if __name__ == "__main__":
    main()
