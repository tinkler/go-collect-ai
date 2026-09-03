#!/usr/bin/env python3
"""
compute_window.py — 给定 today,返回"下一个重要事件 + 当前活跃季节"

入参(stdin JSON): {"today": "YYYY-MM-DD"}
出参(stdout JSON): 详见 SKILL.md
"""
import json
import re
import sys
from datetime import date, datetime, timedelta
from pathlib import Path

# === 事件倍数表(可由老板改这里,即生效)===
EVENT_MULTIPLIER = {
    "春节": 8.0,
    "元宵": 2.0,
    "端午": 3.0,
    "中秋": 5.0,
    "国庆": 4.0,
    "元旦": 3.0,
    "清明": 1.5,
    "五一": 2.0,
    "618": 6.0,
    "双11": 8.0,
    "双12": 4.0,
    "年货节": 5.0,
    "开学季": 3.0,
}

DEFAULT_LEAD_DAYS = 7

# === 活跃季节判定(简化版,可扩展)===
SEASON_TAXONOMY = [
    {"name": "雪糕季", "start": (5, 1), "end": (9, 30), "rationale": "气温 25°C 以上,雪糕/冰棒/冷饮旺销"},
    {"name": "月饼季", "start": (8, 15), "end": (9, 25), "rationale": "中秋前 45 天到节前一天,月饼/礼盒核心档期"},
    {"name": "火锅季", "start": (10, 15), "end": (3, 15), "rationale": "气温下降,火锅底料/牛羊肉丸/蘸酱走量"},
    {"name": "年货季", "start": (12, 15), "end": (2, 10), "rationale": "春节前 20 天起,坚果/糖果/酒水旺销"},
    {"name": "夏季饮料", "start": (6, 1), "end": (9, 15), "rationale": "瓶装水/茶饮/啤酒走量"},
    {"name": "冬季进补", "start": (11, 1), "end": (2, 28), "rationale": "枸杞/红枣/阿胶/暖手宝"},
    {"name": "开学季", "start": (8, 20), "end": (9, 10), "rationale": "文具/书包/零食/牛奶"},
]

SKILL_ROOT = Path(__file__).parent.parent
REFS_DIR = SKILL_ROOT / "references"


def parse_today(payload: dict) -> date:
    if "today" not in payload:
        raise ValueError("missing 'today' (YYYY-MM-DD)")
    return datetime.strptime(payload["today"], "%Y-%m-%d").date()


def in_window(today: date, start: tuple, end: tuple) -> bool:
    """判断 today 是否落在 (start_md, end_md) 窗口内(支持跨年,比如 10/15-3/15)"""
    sm, sd = start
    em, ed = end
    md = (today.month, today.day)
    start_md = (sm, sd)
    end_md = (em, ed)
    if start_md <= end_md:
        return start_md <= md <= end_md
    # 跨年
    return md >= start_md or md <= end_md


def load_holidays(year: int) -> list:
    """解析 references/chinese_holidays_<year>.md,返回事件列表"""
    md_path = REFS_DIR / f"chinese_holidays_{year}.md"
    if not md_path.exists():
        # 退化:用内置表
        return _builtin_holidays(year)
    out = []
    text = md_path.read_text(encoding="utf-8")
    # 形如: - **2026-09-25** — 中秋节 (lead_days=7, multiplier=5.0)
    pattern = re.compile(r"-\s*\*\*(\d{4}-\d{2}-\d{2})\*\*\s*[—–-]\s*(\S+).*?lead_days=(\d+).*?multiplier=([\d.]+)")
    for m in pattern.finditer(text):
        d = datetime.strptime(m.group(1), "%Y-%m-%d").date()
        name = m.group(2)
        lead = int(m.group(3))
        mult = float(m.group(4))
        out.append({"date": d, "name": name, "lead_days": lead, "multiplier": mult})
    if not out:
        return _builtin_holidays(year)
    return sorted(out, key=lambda x: x["date"])


def _builtin_holidays(year: int) -> list:
    """内置节假日兜底(2026 视角的常用日期)"""
    base = [
        ("01-01", "元旦", 3, 3.0),
        ("02-17", "春节", 14, 8.0),  # 2026 春节
        ("06-01", "618", 3, 6.0),
        ("09-25", "中秋节", 7, 5.0),  # 2026 中秋
        ("10-01", "国庆", 7, 4.0),
        ("11-11", "双11", 3, 8.0),
        ("12-12", "双12", 3, 4.0),
    ]
    out = []
    for md, name, lead, mult in base:
        d = datetime.strptime(f"{year}-{md}", "%Y-%m-%d").date()
        out.append({"date": d, "name": name, "lead_days": lead, "multiplier": mult})
    return sorted(out, key=lambda x: x["date"])


def next_event(today: date, holidays: list) -> dict | None:
    """下一个 >= today+1 的事件"""
    future = [h for h in holidays if h["date"] > today]
    if not future:
        return None
    e = future[0]
    days_until = (e["date"] - today).days
    return {
        "name": e["name"],
        "date": e["date"].isoformat(),
        "days_until": days_until,
        "recommended_lead_days": e["lead_days"],
        "recommended_multiplier": e["multiplier"],
        "reason": f"{e['name']} 距今 {days_until} 天,按事实表默认 lead_days={e['lead_days']}、备货倍数={e['multiplier']}",
    }


def active_seasons(today: date) -> list:
    return [
        {"name": s["name"], "end_date": _season_end_iso(today, s), "rationale": s["rationale"]}
        for s in SEASON_TAXONOMY
        if in_window(today, s["start"], s["end"])
    ]


def _season_end_iso(today: date, s: dict) -> str:
    sm, sd = s["start"]
    em, ed = s["end"]
    end_year = today.year
    if (em, ed) < (sm, sd):  # 跨年
        end_year += 1
    return f"{end_year}-{em:02d}-{ed:02d}"


def main():
    # 关键:用 sys.stdin.buffer 读 raw bytes,避免 Windows 把 stdin 按 ANSI(GBK)解码产生 surrogates
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)
    try:
        today = parse_today(payload)
    except ValueError as e:
        print(json.dumps({"error": str(e)}), file=sys.stderr)
        sys.exit(1)

    holidays = load_holidays(today.year)
    nxt = next_event(today, holidays)
    result = {
        "today": today.isoformat(),
        "next_event": nxt,
        "active_seasons": active_seasons(today),
        "source": "compute_window.py v1.0",
    }
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
