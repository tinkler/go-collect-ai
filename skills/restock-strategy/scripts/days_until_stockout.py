#!/usr/bin/env python3
"""
days_until_stockout.py — 算"按当前 daily_avg,库存还能撑几天" + 给出优先级

入参(stdin JSON): {"inv_snapshot": float, "daily_avg": float}
出参(stdout JSON): {
  "days_until_stockout": float,
  "priority": "P0" | "P1" | "P2" | "P3" | "N/A",
  "rationale": str
}

阈值(可改,见 priority_semantics.md):
  < 0.5 → P0
  < 1.5 → P1
  < 3   → P2
  ≥ 3   → P3
  inv_snapshot <= 0 或 daily_avg <= 0 → N/A(异常)
"""
import json
import sys


THRESHOLDS = [
    (0.5, "P0", "撑不到半天,立即处理"),
    (1.5, "P1", "2 小时内处理"),
    (3.0, "P2", "今日处理"),
    (float("inf"), "P3", "预防性,可在看板看"),
]


def classify(days: float) -> tuple:
    for thresh, prio, desc in THRESHOLDS:
        if days < thresh:
            return prio, desc
    return "P3", "预防性"


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    try:
        inv = float(p["inv_snapshot"])
        daily = float(p["daily_avg"])
    except (KeyError, ValueError, TypeError) as e:
        print(json.dumps({"error": f"参数错误: {e}"}), file=sys.stderr)
        sys.exit(1)

    if inv <= 0 or daily <= 0:
        out = {
            "days_until_stockout": 0.0,
            "priority": "N/A",
            "rationale": "库存或日均为 0,无法判定(可能是数据缺失或临期关单)",
        }
    else:
        days = inv / daily
        prio, desc = classify(days)
        out = {
            "days_until_stockout": round(days, 2),
            "priority": prio,
            "rationale": f"库存 {inv} / 日均 {daily} = {days:.2f} 天 → {prio}({desc})",
        }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
