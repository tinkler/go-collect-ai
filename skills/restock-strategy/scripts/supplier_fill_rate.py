#!/usr/bin/env python3
"""
supplier_fill_rate.py — 算供应商 fill_rate + 调整系数

入参(stdin JSON): {
  "delivered_qty": float,
  "ordered_qty": float,
  "window_days": int(可选,默认 30)
}
出参(stdout JSON): {
  "fill_rate": float(0~1),
  "tier": "excellent" | "reliable" | "fair" | "poor",
  "adjustment_factor": float(>= 1.0),
  "rationale": str
}

fill_rate = delivered / ordered
adjustment_factor = 1 / fill_rate(补货量应乘以这个数,fill_rate 越低,补得越多)

分级(可改):
  >= 0.98  excellent  调整 1.02
  >= 0.95  reliable   调整 1.05
  >= 0.90  fair       调整 1.11
  <  0.90  poor       调整 1.25
"""
import json
import sys


TIERS = [
    (0.98, "excellent", 1.02),
    (0.95, "reliable", 1.05),
    (0.90, "fair", 1.11),
    (0.0, "poor", 1.25),
]


def classify(fr: float) -> tuple:
    for thresh, tier, adj in TIERS:
        if fr >= thresh:
            return tier, adj
    return "poor", 1.25


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    try:
        delivered = float(p["delivered_qty"])
        ordered = float(p["ordered_qty"])
    except (KeyError, ValueError, TypeError) as e:
        print(json.dumps({"error": f"参数错误: {e}"}), file=sys.stderr)
        sys.exit(1)

    if ordered <= 0:
        print(json.dumps({"error": "ordered_qty 必须 > 0"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
    if delivered < 0:
        print(json.dumps({"error": "delivered_qty 不能为负"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)

    window_days = int(p.get("window_days", 30))

    fr = min(1.0, delivered / ordered)  # 截断到 1.0(超过 100% 视为满分)
    tier, adj = classify(fr)
    rationale = (
        f"近 {window_days} 天: 实收 {delivered} / 订购 {ordered} = {fr*100:.1f}%"
        f" → {tier}(补货量 ×{adj:.2f})"
    )

    out = {
        "fill_rate": round(fr, 4),
        "tier": tier,
        "adjustment_factor": adj,
        "rationale": rationale,
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
