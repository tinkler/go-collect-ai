# Icon 段位映射 (前端契约)

> 配合 SKILL.md 使用。每条 alert 的 `category` 字段决定前端显示哪个 icon。

---

## 5 个 category 段位

| category | 颜色 | glyph | 含义 | 典型规则 |
|---|---|---|---|---|
| `block` | 🔴 红色 (`#e54d42`) | `!` (实心圆+感叹号) | 限入场 | block_entry |
| `warn` | 🟠 橙色 (`#f37d1a`) | `!` (实心圆+感叹号) | 警告 | no_return, high_stock (未降级) |
| `info` | ⚪ 灰 (`#888`) | `!` (实心圆+感叹号) | 提示 | offseason, holiday_lead, high_stock (降级后) |
| `highlight_dui` | 🟢 绿色 (`#07c160`) | `🏷` (贴纸/标签) | 堆头陈列 (亮点) | has_duitou |
| `highlight_others` | 🟢 绿色 (`#07c160`) | `📌` (图钉) | 快讯/活动 (亮点) | flash_promo |

---

## 渲染建议

### 表格行内 (alerts[] 按 row_id 关联 rows[])

```html
<td class="alert-icons">
  <!-- 同一行可能多 alert, 全部显示 -->
  <span class="icon-block" title="限入场">🔴</span>
  <span class="icon-warn" title="高库存">🟠</span>
</td>
```

### 总结栏 (summary[] 显示在图片卡片下)

```html
<div class="summary">
  <h3>📌 总结</h3>
  <ul>
    <li class="highlight-dui">🏷 本期堆头陈列: [汇一] 堆头 ¥5000/月(至 10-15)</li>
    <li class="info">⚪ 距 [中秋节] 还有 12 天</li>
  </ul>
</div>
```

### 点击查看详情

```html
<dialog class="alert-detail">
  <h4>{{ alert.rule }}</h4>
  <p>等级: {{ alert.severity }}</p>
  <p>类别: {{ alert.category }}</p>
  <p>消息: {{ alert.message }}</p>
  <p>时间: {{ alert.created_at }}</p>
  <button @click="ack(alert.alert_id)">标记已读</button>
</dialog>
```

---

## 颜色无障碍 (a11y)

- 不要只靠颜色:每个 icon 加 glyph (图形) + title (悬浮提示)
- 屏幕阅读器: `<span aria-label="限入场,红色警告">🔴</span>`

---

## 跟 supplier-policy / restock-strategy 的 icon 一致性

为了用户体验统一:
- 限入场 = block = 🔴  (跟 supplier-policy 描述 "拉黑/不进货" 一致)
- 不让退 = warn = 🟠  (跟供应商政策里的 "warn" 一致)
- 难消化 = info = ⚪  (跟 restock-strategy 的 P2/P3 一致)
- 堆头/快讯 = highlight = 🟢  (跟 settlement-suggestion 的 "亮点" 提示一致)
