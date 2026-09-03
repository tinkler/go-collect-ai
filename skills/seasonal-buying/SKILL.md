---
name: seasonal-buying
description: 判定"应季采购窗口"——根据当前日期、节假日日历、季节切换、促销档期,自动给老板建议"从哪一天开始备货,备多少倍"。Use this skill when the user asks about 应季/换季/节假日备货/节前预警/中秋/春节/618/双11/夏季饮料/冬季火锅底料/雪糕季/开学季, or when the user says "下个月要过节了" / "最近要备什么" / "季节到了" / "这周要上什么新品".
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: buying-strategy
  migrated_from: "internal/agent/tools/calendar.go (record_special_date / query_upcoming_dates 推理层)"
compatibility: requires Python 3.x
triggers:
  - 应季采购
  - 换季备货
  - 节前预警
  - 中秋节备货
  - 春节备货
  - 618 / 双11 备货
  - 雪糕季
  - 火锅季
  - 开学季
  - 季节性商品
  - 备货窗口
  - 备货倍数
---

# Seasonal Buying(应季采购)

> **目标**:把"什么时候备货,备多少倍"这件事,从一个**硬编码的查表工具**,升级成 LLM + 事实表 + 工具协同的**推理流程**。
>
> **之前**:所有判定都靠老板人工输入 + `record_special_date` 白名单(holiday/promo/blackout/season_start/season_end)+ `lead_days` 提前天数。LLM 只负责"听懂老板的话然后调 tool"。
>
> **现在**:LLM 主动读取事实表(references/),识别当前日期距离下一个事件的天数,对照老板的"备货规则",给出建议 + 推荐 lead_days;调 `query_upcoming_dates` / `record_special_date` 把决策落库。

## When to use this skill

触发此 skill 的场景:

1. **用户主动问"应季 / 换季"**: "下个月要过节了,要不要备货?" / "雪糕季快到了,怎么备?"
2. **触发式提醒**: 老板话里出现"季节"、"节"、"季"、"换季"、"新品"、"档期"
3. **数据请求**: 老板话里出现"最近 N 天有什么节"、"接下来该备什么"、"哪些是季节性商品"

**不适用**:
- 一般商品补货(走 `restock` 模块,不调本 skill)
- 纯数据查询(直接调 `query_upcoming_dates` tool)

## How to use this skill(LLM 工作流)

### 步骤 0:加载事实表

调用 `invoke_skill` action=`read_file` 路径=`references/chinese_holidays_2026.md`。
> 该文件包含 2026 全年的法定节假日、农历节气、主流电商促销档期,以及**默认 lead_days 建议值**。

如果你判断用户问的是 2026 年以外的日期,**明确告诉老板事实表过期**,并请求补充新事实表。

### 步骤 1:确定当前窗口

读取 `references/current_window.json`(可被 `scripts/compute_window.py` 重新生成),里面是:

```json
{
  "today": "2026-09-02",
  "next_event": { "name": "中秋节", "date": "2026-09-25", "days_until": 23, "recommended_lead_days": 7, "recommended_multiplier": 5.0 },
  "active_seasons": ["夏季饮料尾季", "月饼季"]
}
```

如果文件不存在,直接调 `scripts/compute_window.py`(传入 today 日期)生成。

### 步骤 2:综合判定

结合以下信息,给老板一段**简洁**的判断(单条 ≤ 200 字,推企微时尤其重要):

- 当前事实表里的 `next_event` 和 `days_until`
- 老板已有规则(从对话上下文 / 长期记忆)
- `query_upcoming_dates` 工具返回的"已存在的特殊日期"

### 步骤 3:落库(可选)

如果老板确认备货计划:

1. 先调 `record_special_date` 工具,**`dry_run=true`** 预览要写的内容
2. 给老板念一遍:"记下: {date} {type=season_start} {name=雪糕季} lead_days=7。对吗?"
3. 老板回 OK → 改 `dry_run=false` 真写
4. 写完用 `query_upcoming_dates` 复检一次

## Scripts

### `scripts/compute_window.py`

**作用**:给定 today 日期,输出"下一个重要事件"和"当前活跃季节"。

**入参(stdin JSON)**: `{"today": "2026-09-02"}`

**出参(stdout JSON)**:

```json
{
  "today": "2026-09-02",
  "next_event": {
    "name": "中秋节",
    "date": "2026-09-25",
    "days_until": 23,
    "recommended_lead_days": 7,
    "recommended_multiplier": 5.0,
    "reason": "中秋节是月饼/酒水/礼盒的核心档期"
  },
  "active_seasons": [
    { "name": "夏季饮料尾季", "end_date": "2026-09-15", "rationale": "气温仍高,冰饮走量稳定" }
  ]
}
```

**核心逻辑**(脚本里硬编码,改 script 即生效):
1. 读 `references/chinese_holidays_2026.md`,取所有 `category: holiday` 的事件
2. 找 `date > today` 的最近一条
3. 按事件名映射到 `multiplier` 字典(月饼=5.0, 雪糕=3.0, 春节=8.0 等)
4. 输出 JSON

**调用方式**:`invoke_skill` action=`run_script` path=`scripts/compute_window.py` args=`{"today": "2026-09-02"}`

## References

- `references/chinese_holidays_2026.md` — 2026 全年节假日 + 促销档期 + 默认 lead_days
- `references/season_taxonomy.md` — "雪糕季"/"火锅季"/"月饼季"等季节定义,什么时候开始/结束

## Common Patterns

### 模式 A:老板问"下个月要过节了,要不要备货?"

```
用户: 下个月要过节了
LLM:
  1. 调 invoke_skill(action=load, skill_name=seasonal-buying, input=...)
  2. 调 invoke_skill(action=run_script, path=scripts/compute_window.py, args={today})
  3. 拿到 next_event 信息
  4. 调 query_upcoming_dates tool 看老板以前记的同类
  5. 回答: 中秋节 9/25,还有 23 天。建议 9/18 开始备货,备 5 倍量。要记下吗?
```

### 模式 B:老板说"雪糕到了,备点"

```
用户: 雪糕到了
LLM:
  1. 调 invoke_skill(load)
  2. 读 references/season_taxonomy.md 查"雪糕季"定义
  3. 调 record_special_date(type=season_start, name=雪糕季, lead_days=7) — dry_run=true
  4. 给老板念: 记下: 2026-05-01 season_start 雪糕季 lead_days=7。对吗?
```

### 模式 C:批量预测("接下来 30 天有什么要备的")

```
用户: 接下来 30 天有什么要备的
LLM:
  1. 调 query_upcoming_dates(type=holiday, days_ahead=30)
  2. 调 query_upcoming_dates(type=promo, days_ahead=30)
  3. 综合输出表格
```

## Guidelines

- **description 是唯一触发信号**:本 skill 的 description 必须保留"应季/换季/节前预警/中秋/春节/618/双11/雪糕/火锅/开学"这些关键词,LLM 才会自主激活
- **不要把"下一个节日是哪天"硬编码在 Go 工具里**:所有事实数据放 references/,脚本只做"找最近的"这种通用操作
- **老板的话优先于事实表**:如果老板说"这次中秋少备点",以老板为准
- **dry_run 二次确认**:任何写库动作必须先 dry_run=true,等老板点头再真写

## Keywords

应季, 换季, 备货, 节前, 节假日, 中秋, 春节, 元旦, 国庆, 端午, 清明, 618, 双11, 双12, 年货节, 雪糕, 火锅, 月饼, 粽子, 啤酒, 饮料, 开学季, 季节性, 档期, 备货窗口, lead_days, 备货倍数
