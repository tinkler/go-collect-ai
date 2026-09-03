#!/usr/bin/env python3
"""
calc_share.py — 算促销费在指定 month 的分摊

入参(stdin JSON): {"amount": float, "period_start": "YYYY-MM-DD",
                   "period_end": "YYYY-MM-DD", "month": "YYYY-MM"}
出参(stdout JSON): {"overlap_days": int, "total_days": int, "month_share": float}
"""
import json
import sys
from datetime import date, datetime, timedelta


def parse(s: str) -> date:
    return datetime.strptime(s, "%Y-%m-%d").date()


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    try:
        amount = float(p["amount"])
        ps = parse(p["period_start"])
        pe = parse(p["period_end"])
        month_str = p["month"]
        month_start = parse(month_str + "-01")
        if month_start.month == 12:
            month_end = date(month_start.year + 1, 1, 1)
        else:
            month_end = date(month_start.year, month_start.month + 1, 1)
    except (KeyError, ValueError) as e:
        print(json.dumps({"error": f"参数错误: {e}"}), file=sys.stderr)
        sys.exit(1)

    # period 包含 end 当天
    pe_inc = pe + timedelta(days=1)

    overlap_start = max(ps, month_start)
    overlap_end = min(pe_inc, month_end)
    overlap_days = (overlap_end - overlap_start).days
    if overlap_days < 0:
        overlap_days = 0

    total_days = (pe_inc - ps).days
    if total_days <= 0:
        total_days = 0
    month_share = amount * overlap_days / total_days if total_days > 0 else 0.0

    out = {
        "overlap_days": overlap_days,
        "total_days": total_days,
        "month_share": round(month_share, 2),
        "month": month_str,
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
