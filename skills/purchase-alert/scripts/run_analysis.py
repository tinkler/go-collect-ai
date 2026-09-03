#!/usr/bin/env python3
# scripts/run_analysis.py
#
# Purchase Alert skill 辅助脚本 (W4.2)
# 用途: LLM 调 invoke_skill('purchase-alert', 'run_script', 'run_analysis.py', input_json)
#      后, 这个脚本做"数据预查 + 结构化结果" 减轻 LLM 推理负担
#
# 输入 (stdin JSON):
# {
#   "session_id": "...",
#   "supplier_name": "...",
#   "rows": [...]
# }
#
# 输出 (stdout JSON):
# {
#   "queries": {
#     "supplier_policy": {...},
#     "promotion_fee": [...],
#     "calendar": [...],
#     "app_settings": {...}
#   },
#   "candidate_alerts": [...]  // 数据已就位, LLM 只需做"判定 + 降级"决策
# }
#
# 设计哲学: 这个脚本 0 业务判断, 只做"批量查询 + 数据预聚合"。

import sys
import json
import os
import urllib.request
import urllib.parse
from datetime import datetime, timezone


def call_api(method, path, body=None, query=None):
    """调 collect-ai HTTP API"""
    base = os.environ.get("COLLECTAI_API", "http://localhost:8089")
    if query:
        path = f"{path}?{urllib.parse.urlencode(query)}"
    url = f"{base}{path}"
    data = json.dumps(body).encode("utf-8") if body else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read())
    except Exception as e:
        return {"_error": str(e)}


def main():
    input_data = json.load(sys.stdin)
    session_id = input_data.get("session_id")
    supplier = input_data.get("supplier_name")
    rows = input_data.get("rows", [])

    if not session_id or not supplier:
        print(json.dumps({"_error": "session_id and supplier_name required"}))
        sys.exit(1)

    # 1) 批量前置查询 (实际 LLM 跑时, 可能会调更多 tool)
    queries = {}

    # 1.1 supplier_policy 全量
    r = call_api("GET", "/api/v1/internal/tools/supplier_policy",
                 query={"supplier": supplier})
    queries["supplier_policy"] = r

    # 1.2 promotion_fee 当前在期
    r = call_api("GET", "/api/v1/internal/tools/promotion_fee",
                 query={"supplier": supplier})
    queries["promotion_fee"] = r

    # 1.3 接下来 90 天日历
    r = call_api("GET", "/api/v1/internal/tools/calendar",
                 query={"lead_days": 90})
    queries["calendar"] = r

    # 1.4 app_settings (3 个 key)
    settings = {}
    for k in ("high_stock_threshold", "duitou_kinds", "others_kinds"):
        r = call_api("GET", "/api/v1/internal/tools/app_settings",
                     query={"key": k})
        settings[k] = r
    queries["app_settings"] = settings

    # 2) 候选 alerts (空, LLM 自己填)
    candidate_alerts = []

    # 输出: LLM 拿这个结果, 跑 7-rules.md 判定
    print(json.dumps({
        "queries": queries,
        "candidate_alerts": candidate_alerts,
        "as_of": datetime.now(timezone.utc).isoformat(),
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
