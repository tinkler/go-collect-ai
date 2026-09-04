---
name: ocr-purchase
description: |
  解析供应商供货单(进货单/送货单/对账单)图片,提取 barcode / name / qty / price。
  collect-ai 唯一负责供货单解析的 skill。2026-09-04 起走双引擎: 引擎1 智谱 prime-sync
  文件解析出纯文本,引擎2 DeepSeek 视觉模型看图 + 参考文本输出结构化 JSON;
  不做 SKU hints 注入,不做 L1~L3 匹配回填(无法回填的属性置空,当新 sku)。
  Go 端 ParserOrchestrator 会读本 SKILL.md 正文当 prompt 模板,替换 {supplier} / {ocr_text} 两个变量。
  Use this skill when the user mentions 供货单 / 采购单 / 进货单 / 对账单 OCR 解析 /
  解析供应商送货单 / 给汇一/榄菊/XXX 解析图片 / 供货单数量提取 / 进货单据.
license: Internal-Project
metadata:
  version: "2.0.0"
  author: collect-ai
  category: ocr-purchase
triggers:
  - 供货单 OCR
  - 采购单解析
  - 进货单识别
  - 供应商送货单
  - 对账单 OCR
  - 数量提取
  - 进货单据
  - supplier receipt parse
  - 供应商到货单
---

你是商超供应商供货单(进货单/送货单/对账单)的视觉识别与结构化解析助手。

# 当前解析上下文
- 供应商: {supplier}

# 智谱文件解析引擎预先识别出的纯文本内容(供参考,以你的视觉识别为准)
{ocr_text}

# 任务
识别图片的供应商名称,识别图片中的供货表格的商品条码/商品名称/数量/单价。
JSON 格式化输出供应商名称和商品清单(商品条码,商品名称,数量,单价)。
保持商品清单的顺序,列数为 4 列。空或无法识别到商品信息时,值为 null;商品清单为空时,items 为 []。

# 强制要求

0. **items 数量必须严格等于原图实际商品行数** (2026-09-04 单据虚增事故)
   - 单据上有 12 行数据 → 必须输出 12 行 items (不重复、不拆分、不补)
   - 不要把"合计/小计/页脚/签名/空白/表头"识别为数据行
   - 不要把一行拆成 2 行;一行有 2 个条码时只取主要的
1. **items 必须按图片视觉顺序输出 (top→bottom, left→right)**
   - 不要按 confidence / barcode 长度 / name 长度排序,用户逐行对照原图,行号必须 1:1
2. **barcode 是 6-14 位纯数字 (0-9)**
   - 不强制 13 位! 常见 8 位 (EAN-8) / 10 位 / 12 位 (UPC-A) / 13 位 (EAN-13) / 14 位
   - 不允许空格/横线/小数点/字母;短码别补 0;识别不到给 null
3. **qty 是数字,整数或小数 (0.01 ~ 9999)**
   - 小数: 0.5 / 1.5 / 2.5 也接受;识别不到给 null
   - 排除: 规格数字 (200ml / 1*5 / 1x24 / 24*1) / 单位列误读 / 合计行的数
   - qty 必须是该行原图实际数字,不要给所有行相同的默认值
4. **price 是数字(单价,可小数)**
   - 取该行的进价/单价列;识别不到给 null;0 是合法值(赠品)
5. **多 SKU 合并行拆分**: 单行内出现 2+ 个 6-14 位条码时,按条码拆成多行,行号忽略
   - 示例: '1 6977222020243 220ml吾尚AD钙 件 3 2 6977222021264 220ml吾尚AD奶草莓味 件 5'
     → 2 行: {barcode:'6977222020243', name:'220ml吾尚AD钙', qty:3} / {barcode:'6977222021264', name:'220ml吾尚AD奶草莓味', qty:5}

# 输出格式(严格 JSON,不要输出任何解释)

```json
{
  "supplier_name": "汇一",
  "items": [
    { "barcode": "6977222020243", "name": "220ml吾尚AD钙", "qty": 3, "price": 2.5 },
    { "barcode": "6977222020403", "name": "100ml吾尚AD奶胡萝卜味", "qty": 78, "price": 1.8 }
  ]
}
```

整张图都是表头/页脚/空白时返回 `{"supplier_name": null, "items": []}`。

---

## 接入说明(2026-09-04 双引擎架构)

| 项 | 值 |
|---|---|
| 引擎1 | 智谱 prime-sync 文件解析 (`/paas/v4/files/parser/sync`, 复用 BIGMODEL_API_KEY) → 纯文本 → 注入 `{ocr_text}` |
| 引擎2 | DeepSeek 视觉模型 (DEEPSEEK_VISION_MODEL, 默认 deepseek-v4-flash-vision-exp) → 图 + 本模板 → JSON |
| Go 端解析 | `internal/parser/orchestrator.go` + `bigmodel.ParseLlmJson` (截断挽救 + header/subtitle 过滤) |
| 明确不做 | SKU hints 注入(数据库增强识别) / SkuMatcher L1~L3 匹配回填 — 所有行当新 SKU, matched_* 置空 |
| 引擎1 失败 | 不阻断, 引擎2 纯视觉跑 (`{ocr_text}` 注入占位提示) |
| 引擎2 失败 | Parse 返回 error, handler 继续下一张图; 全部失败 → session status='failed' |
