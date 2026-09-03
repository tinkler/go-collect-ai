#!/usr/bin/env python3
"""
parse_policy_utterance.py — 拆老板一段话成 (supplier, key, value) 三元组

入参(stdin JSON): {"utterance": str, "known_suppliers": [str](可选)}
出参(stdout JSON): {
  "items": [{"supplier": str, "key": str, "value": any, "confidence": float, "matched_text": str}],
  "warnings": [str]
}

策略:先按 known_suppliers 切分文本成多段(没传就整段作为单段),
    每段内做 keyword → key 映射。LLM 拿到结果后**还要语义校验**,
    脚本只做"字面拆解",不替 LLM 决策。
"""
import json
import re
import sys


# (key_pattern_in_text, key, value, confidence)
KEY_RULES = [
    # 自采类
    (r"自采|自营|直采|厂方直发|自己进货", "is_self_procure", True, 0.90),
    # 退货 false 类
    (r"不让退|不能退|无理由不退|概不退换", "allow_return", False, 0.95),
    # 退货 true 类
    (r"可以退|能退|7\s*天无理由|临期可退|支持退", "allow_return", True, 0.85),
    # 堆头 true 类
    (r"堆头(?:他们|供应商|对方|厂方)\s*出|供应商承担堆头", "has_duitou", True, 0.95),
    # 堆头 false 类
    (r"(?:我们|店方|我)\s*(?:出|承担)\s*堆头", "has_duitou", False, 0.95),
    # 端架 true 类
    (r"端架(?:他们|供应商|对方|厂方)\s*出|供应商承担端架", "has_duanjia", True, 0.95),
    # 端架 false 类
    (r"(?:我们|店方|我)\s*(?:出|承担)\s*端架", "has_duanjia", False, 0.95),
    # 黑名单 true 类
    (r"以后[不别]进|拉黑|黑名单|以后[不别]要|不进货了", "block_entry", True, 0.85),
    (r"临时[不别]?[让进]?进了?|暂时[不别]?进|先[不别]进", "block_entry", True, 0.80),
]


def split_by_supplier(text: str, known: list) -> list:
    """把 text 按 known supplier 名切分(从长到短避免子串误匹配)"""
    if not known:
        return [(text, None)]
    # 按长度倒序,优先匹配长的
    sorted_known = sorted(known, key=lambda x: -len(x))
    parts = [(text, None)]
    for sup in sorted_known:
        new_parts = []
        for seg, cur_sup in parts:
            if cur_sup is not None:
                new_parts.append((seg, cur_sup))
                continue
            # 按 supplier 切分
            pieces = re.split(f"({re.escape(sup)})", seg)
            for p in pieces:
                if p == sup:
                    new_parts.append((p, sup))
                elif p:
                    new_parts.append((p, None))
        parts = new_parts
    # 合并:supplier name 后面紧跟的段(到下一个 supplier 或结尾)归该 supplier
    merged = []
    buf_text = ""
    cur_sup = None
    for seg, sup in parts:
        if sup is not None:
            # 提交上一个
            if cur_sup is not None and buf_text:
                merged.append((cur_sup, buf_text))
            elif buf_text:
                # 没识别到 supplier 的孤儿段,跳过(或挂到 None)
                pass
            cur_sup = sup
            buf_text = ""
        else:
            buf_text += seg
    if cur_sup is not None and buf_text:
        merged.append((cur_sup, buf_text))
    return merged


def extract(utterance: str, known_suppliers: list) -> tuple:
    items = []
    warnings = []
    seen = set()

    # 先尝试 known_supplier 切分
    segments = split_by_supplier(utterance, known_suppliers)

    for supplier, seg in segments:
        if not seg.strip():
            continue
        if supplier is None:
            # 没识别出 supplier,跳过(LLM 需自己问老板)
            warnings.append(f"未识别 supplier 的段: {seg[:30]}...")
            continue
        for pat, key, value, conf in KEY_RULES:
            m = re.search(pat, seg)
            if not m:
                continue
            tup = (supplier, key)
            if tup in seen:
                continue
            seen.add(tup)
            items.append({
                "supplier": supplier,
                "key": key,
                "value": value,
                "confidence": conf,
                "matched_text": m.group(0),
            })

    # block_entry=true 时,默认 block_reason 提示
    for it in items:
        if it["key"] == "block_entry" and it["value"] is True:
            if re.search(r"临时|暂时|先", utterance):
                warnings.append(f"{it['supplier']} 检测到 '临时' 关键词,block_reason 建议 '临时'")
            else:
                warnings.append(f"{it['supplier']} block_entry=true,block_reason 建议 '永久'")

    return items, warnings


def main():
    raw_bytes = sys.stdin.buffer.read() or b"{}"
    raw = raw_bytes.decode("utf-8", errors="replace").lstrip("\ufeff").strip() or "{}"
    try:
        p = json.loads(raw)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"invalid JSON: {e}"}), file=sys.stderr)
        sys.exit(1)

    utterance = p.get("utterance", "")
    if not utterance:
        print(json.dumps({"error": "utterance 必填"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)

    known = p.get("known_suppliers") or []
    items, warnings = extract(utterance, known)
    out = {"items": items, "warnings": warnings, "raw": utterance}
    print(json.dumps(out, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
