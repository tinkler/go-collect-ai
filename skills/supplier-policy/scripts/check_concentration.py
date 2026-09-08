#!/usr/bin/env python3
"""
check_concentration.py — 赫芬达尔-Hirschman 指数(HHI)算供应商集中度

入参(stdin JSON): {"supplier_share": {"supplier_name": share_0_1, ...}}
                  或 {"supplier_amounts": {"supplier_name": amount, ...}}(脚本自算 share)
出参(stdout JSON): {
  "hhi": float,         # 0~1(直接除以 10000 转 0~1)
  "tier": str,          # "low" | "moderate" | "high"
  "warning": str,       # 可选
  "ranking": [{"supplier": str, "share": float}, ...]
}

阈值(US DoJ 标准,转 0~1 范围):
  HHI < 0.15       low       (竞争充分)
  0.15 <= HHI< 0.25 moderate
  HHI >= 0.25      high      (集中度高,议价能力风险)
"""
import json
import sys


def hhi(shares: dict) -> tuple:
    """shares: {name: share_0_1}"""
    if not shares:
        return 0.0, [], "no data"
    total = sum(shares.values())
    if total <= 0:
        return 0.0, [], "all zero"
    norm = {k: v / total for k, v in shares.items()}
    h = sum(s * s for s in norm.values())
    ranking = sorted(
        [{"supplier": k, "share": round(s, 4)} for k, s in norm.items()],
        key=lambda x: x["share"],
        reverse=True,
    )
    return round(h, 4), ranking, None


def tier_of(h: float) -> tuple:
    if h < 0.15:
        return "low", None
    if h < 0.25:
        return "moderate", f"HHI {h:.4f} 在 0.15~0.25,集中度中等"
    return "high", f"HHI {h:.4f} >= 0.25,供应商集中度过高,议价能力风险"


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    if "supplier_share" in p:
        shares = p["supplier_share"]
    elif "supplier_amounts" in p:
        amts = p["supplier_amounts"]
        shares = amts  # 脚本会做归一化
    else:
        print(json.dumps({"error": "需 supplier_share 或 supplier_amounts"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)

    h, ranking, err = hhi(shares)
    if err and not ranking:
        print(json.dumps({"error": err}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)

    tier, warn = tier_of(h)
    out = {
        "hhi": h,
        "tier": tier,
        "warning": warn,
        "ranking": ranking,
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
