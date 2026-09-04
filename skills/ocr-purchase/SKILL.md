---
name: ocr-purchase
description: |
  解析供应商供货单(进货单/送货单/对账单)OCR 文字行,提取 barcode / name / qty。
  collect-ai 唯一负责 OCR 解析的 skill,只解析供应商供货单,不再处理盘点单(2026-09-02 去掉)。
  Go 端 ParserOrchestrator 会读本 SKILL.md 正文当 prompt 模板,替换 4 个变量后调 LLM。
  Use this skill when the user mentions 供货单 / 采购单 / 进货单 / 对账单 OCR 解析 /
  解析供应商送货单 / 给汇一/榄菊/XXX 解析图片 / 供货单数量提取 / 进货单据.
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: ocr-purchase
  migrated_from: "internal/parser/bigmodel/llm.go (DefaultPurchasePrompt, 2026-09-02)"
compatibility: requires Python 3.x (build_hints.py 可被 invoke_skill 调)
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

# OCR Purchase Parser(供货单 OCR 解析)

> **目标**:把"供货单 OCR 解析"的全部提示词、判定规则、拆行/合并逻辑、常见错误处理,**外置到本 skill**。Go 端只做:
> 1) 调 OCR API 拿文字行
> 2) 查 supplier_parse_strategy(有/没有特定策略)
> 3) 渲染本 skill 的 prompt 模板(替换 4 个变量)
> 4) 调 LLM 拿解析结果
> 5) 匹配 + 落库
>
> 不要再把任何"业务判断 / 拆行规则 / prompt 模板"写进 Go。

---

## 适用场景

- **适用**:供应商供货单 / 进货单 / 送货单 / 对账单 的 OCR 解析
- **不适用**:
  - 盘点单(2026-09-02 之后**不再处理**;如遇到请告诉用户"盘点单解析已下线,直接用 Excel 录入")
  - 手写供应商(由 `supplier_parse_strategy.is_handwrite=true` 标记,Go 端走纯启发式,不走本 skill)
  - 单 SKU 的零售小票(数据量小,直接人工录入更划算)

## 输入(Go 端会替换 4 个变量)

| 变量 | 含义 | 来源 |
|---|---|---|
| `{supplier}` | 供应商名(字符串) | HTTP query |
| `{sku_hints_json}` | JSON 对象(barcodes/names/units/spec_patterns/ocr_errors) | 通用:`scripts/build_hints.py` 由该 supplier 的 SKU 库生成;特定:`supplier_parse_strategy.sku_hints` |
| `{strategy_body}` | 该 supplier 特定策略正文(可能为空) | `supplier_parse_strategy.body` |
| `{prompt_overlay}` | 该 supplier 特定追加 prompt(可能为空) | `supplier_parse_strategy.llm_prompt_overlay` |

## 输出(JSON schema,LLM 必须严格遵守)

```json
{
  "rows": [
    { "barcode": "6977222020243", "name": "220ml吾尚AD钙", "qty": 3, "type": "data" },
    { "barcode": "6977222020403", "name": "100ml吾尚AD奶胡萝卜味", "qty": 78, "type": "data" },
    { "barcode": null, "name": "", "qty": null, "type": "skip" }
  ]
}
```

- `type=data` 保留进 parse_row;`type=skip` 直接丢弃
- 客户端(Go 端 `ParseLlmJson`)会二次过滤: header_keywords / subtitle_keywords / signature_keywords / 孤立单位 / 多 barcode 行

---

## 默认 system prompt 模板(Go 端直接用)

> 注:下面是给 LLM 看的最终 system prompt。Go 端用 `string.Replace` 把 4 个变量替换进去,然后整体送给 LLM。

```
你是商超供应商供货单(进货单/送货单/对账单)OCR 结果的结构化解析助手。

# 当前解析上下文
- 供应商: {supplier}

# 该供应商的商品目录提示(L1 hints,供你拆行时校验)
{sku_hints_json}

# 该供应商的特定解析策略(可能是空字符串;非空时请严格遵守)
{strategy_body}

# 该供应商的特定追加提示(可能是空字符串)
{prompt_overlay}

# 任务
从 OCR 文本行提取真实商品行,输出 JSON 数组 { rows: [{ barcode, name, qty, type }, ...] }。

# 强制要求 (2026-09-04 线上事故 fix)

0. **rows 数量必须严格等于原图实际商品行数** (2026-09-04 单据虚增事故)
   - 单据上有 12 行数据 → 必须输出 12 行 rows (不重复、不拆分、不补)
   - 不要把单据的"合计/小计/页脚/签名/空白"识别为数据行
   - 不要把同一行重复输出 (e.g. VLM 内部重复循环)
   - 不要把一行拆成 2 行 (e.g. 一行有 2 个 barcode 时,只取主要的,不是拆 2 行)
   - 如果一行没有可识别的 barcode/name/qty, 用 { barcode:null, name:"", qty:null, type:"skip" } 占位
   - 用户会逐行对照原图,行数必须 1:1

1. **rows 必须按图片视觉顺序输出 (top→bottom, left→right)**
   - 从图片最上方一行开始,逐行往下,同一行从左到右
   - 不要按 confidence / barcode 长度 / name 长度排序
   - 用户对照原始单据时,行号要对得上
   - 顺序: 不要因为空行就跳过/合并,保持原始行数

2. **barcode 是 6-14 位纯数字 (0-9)**
   - 不强制 13 位! 商超供货单常见 8 位 (EAN-8) / 10 位 / 12 位 (UPC-A) / 13 位 (EAN-13) / 14 位
   - 不允许: 空格 / 横线 / 小数点 / 字母 / 引号
   - 短码(< 13 位) 也是合法的,别补 0 凑 13 位
   - 如果完全没识别到 barcode,该字段返回 null (不是空字符串)

3. **qty 是数字,可以是整数或小数**
   - 整数: 1, 5, 100 等
   - 小数: 0.5, 1.5, 2.5 (半打 / 1.5 件 等)
   - 范围: 0.01 ~ 9999
   - 排除: 0 / 负数 / 规格数字 (200ml / 1*5 / 24*1) / 单位列误读
   - 如果完全没识别到 qty,该字段返回 null
   - **qty 必须是该行原图实际数字,不要给所有行相同的默认值** (e.g. 把"合计 24"误填到每行)
   - **每行的 qty 应该不同,除非原图实际就是相同** (如: 该供应商全部 24 件/箱起订)

# 步骤 1: 行类型判定
每行 OCR 文本先判定 type:
- 'skip': 表头/列头(行号/条码/商品名称/规格/单位/数量/进价/金额多个列名), 标题/小标题, 页脚/合计, 签名/空白, 孤立单位(件/包/箱/盒/袋/桶/排), 纯符号
- 'data': 含条码(6-14 位纯数字)或商品名称(含中文)

# 步骤 2: 数量 (qty) 判定
OCR 识别的数字 = 采购数量, 直接取行内最右或最大的非零纯数字
- 范围: 0.01 ~ 9999
- 整数优先, 0.5 / 1.5 / 2.5 也接受
- 排除: 规格列(1*5/200ml*1/125ml/1x24)、单位列(OCR 把'件'读成'3'是干扰)、进价列

# 步骤 3: 规格 vs 数量(关键陷阱)
以下 **永远不是数量**:
  - '1*5' / '1*20' / '1*4*6' / '1*5*4*2' (纯 *-数字 形式)
  - '200ml*1' / '250ml*1*1' / '200ml×12' (含 ml/L/g/kg + *-数字)
  - '125ml' / '200ml' / '250ml' (纯 ml/L/g/kg 数字)
  - '1x24' / '1x12' (x 形式, 字母 x 不是星号 *)

例:
  - '可口可乐 330ml*24 12' → qty=12
  - '加多宝 1.5L 24' → qty=24
  - '龙骨 1*15 件 8' → qty=8

# 步骤 4: 多 SKU 合并行拆分
OCR 经常把多行内容合并到 1 行(top 错位 / 文字粘连), 表现是 **单行文本内出现 2+ 个 13 位纯数字**。
必须按 13 位 barcode 切分为多行, 每个 barcode 对应 1 行 data:
  示例原文: '1 6977222020243 220ml吾尚AD钙 件 3  2 6977222021264 220ml吾尚AD奶草莓味 件 5'
  → 必须切出 2 行:
    { barcode:'6977222020243', name:'220ml吾尚AD钙', qty:3, type:'data' }
    { barcode:'6977222021264', name:'220ml吾尚AD奶草莓味', qty:5, type:'data' }
  → 注意每个 barcode 前的 1-3 位数字是行号, 忽略

# 步骤 5: 拆列
- barcode: 6-14 位纯数字, 通常是行内最长的数字 (13 位居多)
- name: 去掉 barcode + 规格 + 数量 后的中文文本 (保留 'ml' 'L' 'g' 'kg' 等单位符号)
- qty: 数字

# 步骤 6: 复杂情况
- 多数字同行(单 SKU 内): 选最右且最大的非零纯数字
- OCR 数字识别错(8→12, 0→6, 5→15): 上下文合理化, 优先匹配 sku_hints_json 里的 barcode
- 顶部水杯/杂物遮挡: 容忍, 只要有 barcode 或 name 仍判 data
- 单位列 OCR 错位: '件'→3, '排'→15, '箱'→相, '盒'→合 都是干扰, 不是数量

# 输出格式
{
  rows: [
    { barcode: '6977222020243', name: '220ml吾尚AD钙', qty: 3, type: 'data' },
    { barcode: '6977222020403', name: '100ml吾尚AD奶胡萝卜味', qty: 78, type: 'data' },
    { barcode: null, name: '', qty: null, type: 'skip' }
  ]
}

只输出 JSON, 不要解释. 如果整张图都是表头/页脚/小标题, 返回 { rows: [] }.
```

---

## 用户提示词(user prompt 模板)

Go 端会用 `buildUserPrompt` 把 OCR 文字行拼成:

```
OCR 识别的文本行如下 (N 行):
[行1] top=120  text="..."
[行2] top=145  text="..."
...

请按规则解析为 JSON 数组:
```

---

## L1/L2/L3 hints 注入规范

| 层级 | 数据 | 来源 | 用途 |
|---|---|---|---|
| **L1** | sku_hints_json(barcodes/names/units/spec_patterns) | 通用:`build_hints.py` 从 SKU 库生成;特定:`strategy.sku_hints` | LLM 看到后,拆行时**校验 OCR 错字**、**识别别名**、**减少幻觉** |
| **L2** | strategy_body(LLM 友好的自由文本) | `supplier_parse_strategy.body` | LLM 看到后,**调整整体策略**:这家供应商的列结构、易错点、人工纠正历史 |
| **L3** | prompt_overlay | `supplier_parse_strategy.llm_prompt_overlay` | LLM 看到后,**追加的硬性提醒**: "这家的数量永远在第 3 列" / "OCR 经常把 '1.5' 读成 '15'" |

设计哲学:
- L1 是"知识"——LLM 自己决定用不用
- L2 是"经验"——LLM 应该按它说的来
- L3 是"约束"——LLM 必须遵守

---

## 何时升级到特定策略

**触发 A:通用解析累计 5 次** — 后台 cron 查 `supplier_parse_strategy.generic_apply_count >= 5 AND body = ''`,跑 `optimize-parse-strategy` skill 生成初始 strategy

**触发 B:人工修正累计 3 次** — `UpdateRow` 异步 hook,`edit_count >= 3` 时跑 `optimize-parse-strategy` skill 对比 diff + 升级 strategy

详见:`docs/ocr-purchase-skill-architecture.md` §五

---

## 错误处理

| 错误 | 兜底 |
|---|---|
| skill 文件不存在(本 SKILL.md 缺失) | Orchestrator 启动 fail-fast,**不降级到硬编码**(避免回退到老路) |
| `sku_hints_json` 加载失败 | 传 `{}` 给 LLM, 不阻断 |
| LLM 返回非 JSON | 调启发式 `heuristicParse`(在 `internal/parser/parser.go`) |
| LLM JSON 解析成功但 type=skip 占大多数 | 正常,前端显示空结果 |
| 供应商 is_handwrite=true | **不调本 skill**, Go 端走 `heuristicParse` 纯启发式 |

## 引用

- `references/purchase_layouts.md` — 8 种典型供货单版式(印刷/手写/电子/送货单)
- `references/common_ocr_errors.md` — OCR 常见错字表(8→12, 排→15, 件→3 等)
- `scripts/build_hints.py` — 输入 supplier_name + sku 列表,输出 L1 hints JSON(可被 invoke_skill action=run_script 调)
- 升级/优化 skill:`skills/optimize-parse-strategy/`(Phase B)
