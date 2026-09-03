#!/usr/bin/env python3
"""
assess_investment.py — 把 (month_share / month_forecast) 映射到 investment_weight

入参(stdin JSON): {"month_share": float, "month_forecast": float, "supplier_tier": "strategic|normal|tactical"(可选)}
出参(stdout JSON): {"ratio": float, "investment_weight": float, "rationale": str}

系数表(可改,与 references/coefficient_defaults.md 同步):
  ratio < 0.5%  -> 0.8
  ratio < 2%    -> 0.95
  ratio < 5%    -> 1.0
  ratio < 10%   -> 1.2
  ratio >= 10%  -> 1.5
"""
import json
import sys


THRESHOLDS = [
    (0.005, 0.8, "基本不投入"),
    (0.02, 0.95, "正常合作"),
    (0.05, 1.0, "平均水平"),
    (0.10, 1.2, "重投入,建议金额上调"),
    (float("inf"), 1.5, "严重依赖,建议老板重新谈判"),
]

# 战略供应商额外 +0.2
TIER_BONUS = {
    "strategic": 0.2,
    "normal": 0.0,
    "tactical": -0.1,
}


def assess(ratio: float, tier: str) -> tuple:
    for thresh, weight, desc in THRESHOLDS:
        if ratio < thresh:
            base = weight
            break
    bonus = TIER_BONUS.get(tier, 0.0)
    final = max(0.7, min(1.7, base + bonus))
    rationale = f"ratio {ratio*100:.2f}% → base {base}, tier={tier} bonus {bonus:+.2f} → final {final:.3f} ({desc})"
    return round(final, 3), rationale


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    try:
        month_share = float(p["month_share"])
        month_forecast = float(p["month_forecast"])
    except (KeyError, ValueError, TypeError) as e:
        print(json.dumps({"error": f"参数错误: {e}"}), file=sys.stderr)
        sys.exit(1)

    tier = p.get("supplier_tier", "normal")
    if month_forecast <= 0:
        out = {
            "ratio": 0.0,
            "investment_weight": 1.0,
            "rationale": "month_forecast <= 0,无法算比例,降级到 1.0",
        }
    else:
        ratio = month_share / month_forecast
        w, rationale = assess(ratio, tier)
        out = {
            "ratio": round(ratio, 4),
            "investment_weight": w,
            "rationale": rationale,
        }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
