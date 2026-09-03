# App Settings 配置(LLM 也读 PG 表)

> 本文件给运营 / AI 看,不直接给 LLM 读。LLM 跑规则时调 `query_app_settings(key)` 实时取最新值。
>
> PG 表:`app_settings` (K-V JSONB,见 `internal/store/pg.go` 初始化)

---

## 阈值类

### high_stock_threshold
- **类型**: number
- **默认**: 50
- **含义**: 商品库存 > 此值,触发 high_stock 规则
- **调整方向**:
  - 调高 (如 80) → 报得少,容忍更多库存
  - 调低 (如 30) → 报得多,更激进
- **建议**: 食品/日化 30-50, 烟酒/家电 100+, 季节性 20
- **运营调整 SQL**:
  ```sql
  UPDATE app_settings SET value = '80'::jsonb, updated_at = NOW()
  WHERE key = 'high_stock_threshold';
  ```

### low_movement_threshold_30d
- **类型**: number
- **默认**: 3
- **含义**: SKU 30 天销量 < 此值,触发 low_movement 规则(W4.2 启用)
- **建议**: 食品 3-5, 日化 2-3, 烟酒 1-2

---

## 分类白名单类

### duitou_kinds
- **类型**: JSONB array of string
- **默认**: `["堆头"]`
- **含义**: kind 在此数组的 promotion_fee,被识别为"堆头陈列" (category=highlight_dui)
- **调整**:
  ```sql
  UPDATE app_settings SET value = '["堆头","端架"]'::jsonb WHERE key = 'duitou_kinds';
  ```
- **注意**: 如果 duitou_kinds 和 others_kinds 有重叠,后启动的规则(flash_promo)优先

### others_kinds
- **类型**: JSONB array of string
- **默认**: `["端架","快讯","DM","特价","海报"]`
- **含义**: kind 在此数组的 promotion_fee,被识别为"快讯/活动" (category=highlight_others)
- **建议**: 跟门店实际打标签习惯对齐,新 kind 让 LLM 建议后人工确认

---

## 季节/应季相关(预留,W4.2 启用)

### season_words_override
- **类型**: JSONB object
- **默认**: `{}` (用 7-rules.md 的内置应季词表)
- **含义**: 运营覆盖/新增应季词,格式 `{"电火锅": ["winter"], "圣诞袜": ["winter"]}`
- **适用**: 新品类 / 地方性应季商品

### season_window_tolerance_days
- **类型**: number
- **默认**: 7
- **含义**: 季节切换前后 N 天,容忍反季判定(不报)

---

## 运行时调参 vs 改 SKILL.md

| 想改什么 | 改哪里 | 生效方式 |
|---|---|---|
| 阈值 / 分类白名单 | `app_settings` 表 (运营可改,无需开发) | 下次 LLM 跑读新值 |
| 加新应季词 | `app_settings.season_words_override` | 同上 |
| 加新规则 | `references/7-rules.md` + SKILL.md 流程图 | skill 热更新, 200ms |
| 改判定逻辑(降级/不降级) | `references/7-rules.md` | skill 热更新 |
| 加新 tool | Go 端 `internal/agent/tools/purchase_alert.go` + `references/query-tools.md` | **需重启 collect-ai** |
