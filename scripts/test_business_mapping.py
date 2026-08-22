# ============================================================
# collect-ai 业务层端到端测试
# ============================================================
# 验证业务字段名 → 物理字段名 → agent → 物理响应 → 业务响应
# 全链路走 cube-agent-server
#
# 前置:
#   - cube-agent-server 跑在 :8088
#   - erp SQLite / hbpos SQL Server 都可达
#   - 业务字段映射已 hardcode 在 internal/business/mapping.go
# ============================================================

import sys
import json
import urllib.request
import urllib.error

BASE_AGENT = "http://localhost:8088"
RESULTS = []


def post(url, body, timeout=30):
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8")
        return e.code, body


def get(url, timeout=10):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8")
        return e.code, body


def check(name, ok, detail=""):
    status = "PASS" if ok else "FAIL"
    RESULTS.append((name, ok, detail))
    print(f"[{status}] {name}: {detail}")


def main():
    print("=" * 70)
    print("collect-ai 业务层(业务字段 ↔ 物理字段)端到端测试")
    print(f"agent: {BASE_AGENT}")
    print("=" * 70)

    # 0. 健康检查
    code, _ = get(f"{BASE_AGENT}/livez")
    check("0. agent /livez", code == 200, f"HTTP {code}")

    # ============================================================
    # 模拟 collect-ai business.Executor 的翻译逻辑
    # ============================================================
    #   业务字段 (products entity):
    #     barcode / product_name / supplier_id / supplier_name
    #     category / brand / stock_qty
    #   erp 物理 cube = products,字段:
    #     products.barcode / products.name / products.main_supp_id
    #     products.main_supp_name / (空 category/brand) / products.qty
    #   hbpos 物理 cube = t_bd_item_info,字段:
    #     t_bd_item_info.item_no / t_bd_item_info.item_name
    #     t_bd_item_info.main_supcust / t_bd_item_info.supplier_name
    #     t_bd_item_info.item_clsno / t_bd_item_info.item_brandname
    #     t_bd_item_info.price (作为 stock_qty 兜底)

    # ---- 1. ERP 端 products 业务查询 ----
    # 业务字段:barcode, product_name, supplier_name, stock_qty
    # 翻译为物理 query
    erp_query = {
        "measures":   ["products.qty"],
        "dimensions": [
            "products.barcode",
            "products.name",
            "products.main_supp_name",
        ],
        "filters": [{
            "member":   "products.main_supp_name",
            "operator": "contains",
            "values":   ["商"],
        }],
        "limit": 5,
    }
    code, resp = post(f"{BASE_AGENT}/v1/load", erp_query)
    rows = (resp.get("data") or {}).get("Rows") or [] if isinstance(resp, dict) else []
    # 翻译响应回业务字段名
    if code == 200 and len(rows) > 0:
        biz_rows = []
        for r in rows:
            biz_rows.append({
                "barcode":       r.get("products.barcode"),
                "product_name":  r.get("products.name"),
                "supplier_name": r.get("products.main_supp_name"),
                "stock_qty":     r.get("products.qty"),
            })
        ok = all(biz_rows[0].get("supplier_name") and biz_rows[0].get("product_name") for _ in [0])
        check("1. ERP 端 products 业务字段查询(barcode/product_name/supplier_name/stock_qty)",
              ok, f"rows={len(biz_rows)}, sample={biz_rows[0]}")
    else:
        check("1. ERP 端 products 业务字段查询", False, f"HTTP {code} resp={str(resp)[:200]}")

    # ---- 2. HBPoS 端 products 业务查询(跨 join 取 supplier_name) ----
    hbpos_query = {
        "measures":   ["t_bd_item_info.price"],
        "dimensions": [
            "t_bd_item_info.item_no",
            "t_bd_item_info.item_name",
            "t_bd_item_info.supplier_name",
            "t_bd_item_info.item_brandname",
        ],
        "limit": 5,
    }
    code, resp = post(f"{BASE_AGENT}/v1/load", hbpos_query)
    rows = (resp.get("data") or {}).get("Rows") or [] if isinstance(resp, dict) else []
    if code == 200 and len(rows) > 0:
        biz_rows = []
        for r in rows:
            biz_rows.append({
                "barcode":       (r.get("t_bd_item_info.item_no") or "").strip(),
                "product_name":  r.get("t_bd_item_info.item_name"),
                "supplier_name": r.get("t_bd_item_info.supplier_name"),
                "brand":         r.get("t_bd_item_info.item_brandname"),
            })
        ok = biz_rows[0].get("supplier_name") is not None and biz_rows[0].get("product_name") is not None
        check("2. HBPoS 端 products 业务字段查询(supplier_name 跨 join)",
              ok, f"rows={len(biz_rows)}, sample={biz_rows[0]}")
    else:
        check("2. HBPoS 端 products 业务字段查询", False, f"HTTP {code} resp={str(resp)[:200]}")

    # ---- 3. 切换 datasource 验证物理 cube 不同 ----
    # 业务字段 supplier_name 翻译为不同物理字段
    #   erp:   products.main_supp_name
    #   hbpos: t_bd_item_info.supplier_name
    erp_supplier = "products.main_supp_name"
    hbpos_supplier = "t_bd_item_info.supplier_name"
    erp_q = {"measures": ["products.qty"], "dimensions": [erp_supplier], "limit": 1}
    hbpos_q = {"measures": ["t_bd_item_info.price"], "dimensions": [hbpos_supplier], "limit": 1}
    e_code, e_resp = post(f"{BASE_AGENT}/v1/load", erp_q)
    h_code, h_resp = post(f"{BASE_AGENT}/v1/load", hbpos_q)
    erp_ok = e_code == 200 and len((e_resp.get("data") or {}).get("Rows") or []) > 0
    hbpos_ok = h_code == 200 and len((h_resp.get("data") or {}).get("Rows") or []) > 0
    check("3. 同一业务字段 supplier_name → 不同物理字段(erp / hbpos)",
          erp_ok and hbpos_ok,
          f"erp rows={len((e_resp.get('data') or {}).get('Rows') or [])}, hbpos rows={len((h_resp.get('data') or {}).get('Rows') or [])}")

    # ---- 4. ERP 端 suppliers 业务查询 ----
    erp_suppliers_q = {
        "measures":   ["products.qty"],
        "dimensions": ["products.main_supp_name"],
        "limit": 10,
    }
    code, resp = post(f"{BASE_AGENT}/v1/load", erp_suppliers_q)
    rows = (resp.get("data") or {}).get("Rows") or [] if isinstance(resp, dict) else []
    if code == 200 and len(rows) > 0:
        # 翻译 supplier_name 物理 ref 为业务名
        names = set()
        for r in rows:
            n = (r.get("products.main_supp_name") or "").strip()
            if n:
                names.add(n)
        check("4. ERP 端 suppliers(从 products DISTINCT)",
              len(names) > 0, f"distinct suppliers={len(names)}")
    else:
        check("4. ERP 端 suppliers", False, f"HTTP {code}")

    # ---- 5. HBPoS 端 suppliers 业务查询 ----
    hbpos_suppliers_q = {
        "measures":   ["suppliers.count"],
        "dimensions": ["suppliers.sup_name"],
        "limit": 10,
    }
    code, resp = post(f"{BASE_AGENT}/v1/load", hbpos_suppliers_q)
    rows = (resp.get("data") or {}).get("Rows") or [] if isinstance(resp, dict) else []
    if code == 200 and len(rows) > 0:
        names = set()
        for r in rows:
            n = (r.get("suppliers.sup_name") or "").strip()
            if n:
                names.add(n)
        check("5. HBPoS 端 suppliers(t_bd_supcust_info 直读)",
              len(names) > 0, f"distinct suppliers={len(names)}, sample={list(names)[:3]}")
    else:
        check("5. HBPoS 端 suppliers", False, f"HTTP {code} resp={str(resp)[:200]}")

    # ---- 总结 ----
    print("=" * 70)
    passed = sum(1 for _, ok, _ in RESULTS if ok)
    total = len(RESULTS)
    print(f"结果: {passed}/{total} PASS")
    if passed < total:
        print("\n失败项:")
        for n, ok, d in RESULTS:
            if not ok:
                print(f"  - {n}: {d}")
        sys.exit(1)


if __name__ == "__main__":
    main()
