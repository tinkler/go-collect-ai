#!/usr/bin/env python3
"""
build_hints.py — 通用 OCR 解析 hints 生成器 (Phase A, 2026-09-02)

输入 (stdin JSON):
  {
    "supplier": "汇一商贸",
    "skus": [
      {"barcode": "6923644254230", "product_name": "蒙牛纯牛奶全脂200ml*12", "unit": "件"},
      ...
    ]
  }

输出 (stdout JSON):
  {
    "barcodes": ["6923644254230", "6923644258900", ...],
    "names": ["蒙牛纯牛奶", "蒙牛酸乳", ...],
    "units": ["件", "排", "箱", ...],  // 该 supplier 常用单位
    "spec_patterns": ["200ml*1*12", "250ml*1*24", ...],  // 常见规格
    "common_chars": ["蒙牛", "汇一", ...]  // 高频字符, 帮 LLM 校验
  }

被 invoke_skill action="run_script" 调:
  python build_hints.py
  stdin: 见上方

失败: 退出码 1 + stderr 错误信息
"""

import json
import re
import sys
from collections import Counter
from typing import Any, Dict, List


# 规格正则
SPEC_RE = re.compile(r"\d+\s*[*x×]\s*\d+(?:\s*[*x×]\s*\d+)*|\d+\s*(?:ml|L|g|kg)")
# 13 位 barcode
BARCODE_RE = re.compile(r"\b\d{13}\b")
# 中文 2-15 字字符串(可能是商品名)
NAME_RE = re.compile(r"[\u4e00-\u9fff]{2,15}")


def extract_spec(name: str) -> str:
    """从商品名中提取规格部分"""
    m = SPEC_RE.search(name)
    return m.group(0) if m else ""


def extract_name_core(name: str) -> str:
    """从商品名中提取核心名字(去掉规格 + 数量)"""
    s = name
    # 去规格
    s = SPEC_RE.sub("", s)
    # 去数字
    s = re.sub(r"\d+", "", s)
    # 去多余空白
    s = re.sub(r"\s+", "", s)
    return s.strip()


def build_hints(supplier: str, skus: List[Dict[str, Any]]) -> Dict[str, Any]:
    barcodes: List[str] = []
    names: List[str] = []
    units: List[str] = []
    spec_patterns: List[str] = []
    common_chars: Counter = Counter()

    for sku in skus:
        bc = sku.get("barcode", "").strip()
        nm = sku.get("product_name", "").strip()
        un = sku.get("unit", "").strip()

        if bc and BARCODE_RE.search(bc):
            barcodes.append(bc)
        if nm:
            # 完整名
            names.append(nm)
            # 核心名(去规格)
            core = extract_name_core(nm)
            if core:
                names.append(core)
            # 规格
            spec = extract_spec(nm)
            if spec:
                spec_patterns.append(spec)
        if un:
            units.append(un)
        # 高频字符(2-4 字的连续中文,可能是品牌)
        for m in NAME_RE.findall(nm):
            if 2 <= len(m) <= 6:
                common_chars[m] += 1

    return {
        "barcodes": sorted(set(barcodes))[:500],  # 上限 500
        "names": sorted(set(names))[:200],
        "units": sorted(set(units)),
        "spec_patterns": sorted(set(spec_patterns))[:100],
        "common_chars": [c for c, _ in common_chars.most_common(50)],
    }


def main():
    try:
        payload = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"invalid JSON: {e}", file=sys.stderr)
        sys.exit(1)

    supplier = payload.get("supplier", "")
    skus = payload.get("skus", [])

    hints = build_hints(supplier, skus)
    # 加上 supplier 名作为 L1 hint, LLM 看到后能匹配表头
    if supplier:
        hints["supplier"] = supplier

    print(json.dumps(hints, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
