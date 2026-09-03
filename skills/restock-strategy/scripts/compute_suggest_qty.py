#!/usr/bin/env python3
"""
compute_suggest_qty.py — 根据 daily_avg / lead_days / safety_days / fill_rate 算补货量

入参(stdin JSON): {
  "daily_avg": float,
  "lead_days": int,
  "safety_days": float(default 1.5),
  "fill_rate": float(0~1, default 1.0=不调整),
  "ceil_unit": int(默认 1=不取整)
}
出参(stdout JSON): {
  "base_qty": float,
  "fill_rate_adjusted": float,
  "final_qty": int,
  "rationale": str
}

公式:
  base_qty = daily_avg * lead_days + daily_avg * safety_days
  fill_rate_adjusted = base_qty / fill_rate    # fill_rate 越低,补得越多
  final_qty = ceil(fill_rate_adjusted / ceil_unit) * ceil_unit
"""
import json
import math
import sys


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    try:
        daily_avg = float(p["daily_avg"])
        lead_days = int(p["lead_days"])
    except (KeyError, ValueError, TypeError) as e:
        print(json.dumps({"error": f"参数错误: {e}"}), file=sys.stderr)
        sys.exit(1)

    safety_days = float(p.get("safety_days", 1.5))
    fill_rate = float(p.get("fill_rate", 1.0))
    ceil_unit = int(p.get("ceil_unit", 1))

    if daily_avg < 0 or lead_days < 0 or safety_days < 0:
        print(json.dumps({"error": "参数不能为负"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
    if not (0 < fill_rate <= 1):
        print(json.dumps({"error": "fill_rate 必须在 (0, 1]"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
    if ceil_unit < 1:
        ceil_unit = 1

    base_qty = daily_avg * lead_days + daily_avg * safety_days
    fill_rate_adj = base_qty / fill_rate
    final_qty = math.ceil(fill_rate_adj / ceil_unit) * ceil_unit

    rationale = (
        f"{daily_avg} × {lead_days} + {daily_avg} × {safety_days} = {base_qty:.2f},"
        f"按 {fill_rate:.2f} 交付率上调到 {fill_rate_adj:.2f},"
        f"向上取 {ceil_unit} 的倍数到 {final_qty}"
    )

    out = {
        "base_qty": round(base_qty, 2),
        "fill_rate_adjusted": round(fill_rate_adj, 2),
        "final_qty": final_qty,
        "rationale": rationale,
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
