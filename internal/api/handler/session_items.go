// Package handler / session_items.go
//
// 2026-09-03 新增: 把 session rows 里的 barcode 反查 hbpos t_bd_item_info,
//   拿到商品内码 item_no 写到 row.ItemNo, 同时取单位 unit_no 写到 row.Unit (不入库).
//
// 设计动机:
//   前端 "采购收货单详情" 页面在企业微信桌面端提供"复制 item_no\tqty 到剪贴板"按钮
//   (见 F:/weixinapp/supermarket-ai/js/session.js). 用户想要的是 hbpos
//   t_bd_item_info.item_no (商超内部编码), 而不是 matched_barcode (国际条码 EAN-13).
//   二者不一致: 同一商品在不同商超可能条码相同, 但内码 (item_no) 各异.
//   用户在采购系统/对账软件里粘贴, 系统只认 item_no.
//   Unit 同步返回, 让前端"1 件/1 瓶/1 包"等显示更准确.
//
// 改动范围 (本文件):
//   - enrichRowsWithItemNo(ctx, rows): 调 cube t_bd_item_info 批量反查
//   - 失败 (cube 不可达 / 无结果) 不阻塞响应, ItemNo / Unit 留空 → 前端 fallback
//   - 性能: 单次 session 一般 5-50 行, 1 次 IN 查询, < 200ms
//
// 接入点:
//   - GetSession (handler.go:716): 返回前调一次
//   - CreateSession (handler.go:~676): 返回前调一次
//
// cube 假设 (2026-09-03 fix 后):
//   - t_bd_item_info cube 实际暴露的 dimension: item_no, item_subno, item_name,
//     item_brandname, unit_no, supplier_name, ... (见 cube-agent-server/plugins/t_bd_item_info/plugin.yaml)
//   - 关键: HBPoS 数据库没有独立 barcode 字段, 业务 barcode 物理上 = item_no
//     (configs/mappings.yaml:89 已声明), 所以 cube dim/filter 必须用 item_no
//   - 2026-09-03 修复: 之前用 t_bd_item_info.barcode 作为 dim/filter → 400 错误
//     (cube 里没有这个 dimension), 改用 t_bd_item_info.item_no
//   - filter 接受 values 数组 (cube-agent-server 标准 IN 语义)
package handler

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

// enrichRowsWithItemNoAsync 2026-09-04: 异步 fire-and-forget 版本的 enrich
//   - 启动 goroutine 用 detached ctx (Background + 5s timeout) 跑 enrich
//   - 客户端响应立即返回,不被 1s 的 cube 反查阻塞
//   - enrich 失败/超时只 log,不影响主流程 (GetSession 仍会同步 enrich 一次)
//   - 关键: rows 是 handler 层局部变量, enrich 改它没用 (handler 已返响应),
//     所以真正"持久化"靠 GetSession 调 enrichRowsWithItemNo
//   - 当前这个 fire-and-forget 主要价值是: 提前 heat-up cube + 1s 内 cube
//     缓存住结果, 让 H5 GET 第一次调时也快 (warm cache 效果)
func (h *Handler) enrichRowsWithItemNoAsync(_ string, rows []model.SkuRow) {
	if h.Agent == nil || len(rows) == 0 {
		return
	}
	// 拷一份 rows 切片头 (浅拷), goroutine 改不会影响主流程的 s
	//   enrichRowsWithItemNo 内部用 range + 索引改 rows[i].ItemNo
	//   浅拷足够,因为 cube 查询只用 rows 里的 barcode 字段 (值类型拷贝)
	rowsCopy := make([]model.SkuRow, len(rows))
	copy(rowsCopy, rows)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 静默跑, 失败/超时只 log (与同步版本一致)
		h.enrichRowsWithItemNo(ctx, rowsCopy)
	}()
}

// enrichRowsWithItemNo 2026-09-03: 批量反查 hbpos t_bd_item_info,
//   把 barcode → item_no 写回 rows[i].ItemNo, 顺便取 unit_no 写回 rows[i].Unit.
//   - cube 失败 / 超时: log + 静默返回 (ItemNo / Unit 留空, 前端 fallback)
//   - 无 Agent client: 静默返回 (测试环境友好)
//   - 空 rows: 直接返回
func (h *Handler) enrichRowsWithItemNo(ctx context.Context, rows []model.SkuRow) {
	if h.Agent == nil || len(rows) == 0 {
		return
	}

	// 1) 收集所有非空 barcode, 去重
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		bc := firstBarcode(r)
		if bc != "" {
			seen[bc] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return
	}
	barcodes := make([]string, 0, len(seen))
	for b := range seen {
		barcodes = append(barcodes, b)
	}

	// 2) 调 cube t_bd_item_info: dim = item_no + unit_no
	//    measures 为空 (只要维度, 不聚合)
	//    filter: item_no IN [...barcodes]
	//
	// 2026-09-03 关键修复: HBPoS 没有独立 barcode 字段, 业务 barcode 物理上
	//   等于 item_no (configs/mappings.yaml:89). cube t_bd_item_info 也没有
	//   barcode dimension, 之前用 t_bd_item_info.barcode → 400 错误. 改用
	//   item_no 后, "barcode IN [barcodes]" 等价于 "item_no IN [barcodes]".
	//   顺手把 unit_no 一起带回, 一次查询给前端两个字段.
	cubeRes, err := h.Agent.Execute("t_bd_item_info",
		[]string{}, // measures
		[]string{"t_bd_item_info.item_no", "t_bd_item_info.unit_no"},
		[]map[string]any{{
			"member":   "t_bd_item_info.item_no",
			"operator": "equals",
			"values":   barcodes,
		}},
		nil, // segments
		0,   // limit
	)
	if err != nil {
		log.Printf("[enrichRowsWithItemNo] cube query failed: %v (rows will fall back to barcode)", err)
		return
	}

	// 3) 建 barcode → {item_no, unit} 索引
	type meta struct{ itemNo, unit string }
	bc2meta := make(map[string]meta, len(cubeRes))
	for _, m := range cubeRes {
		itemNo, _ := m["t_bd_item_info.item_no"].(string)
		unit, _ := m["t_bd_item_info.unit_no"].(string)
		itemNo = strings.TrimSpace(itemNo)
		unit = strings.TrimSpace(unit)
		if itemNo == "" {
			continue
		}
		// cube 里的 item_no == 输入的 barcode (HBPoS 设计), 用 item_no 当 lookup key
		bc2meta[itemNo] = meta{itemNo: itemNo, unit: unit}
	}

	// 4) 回写 (in-place)
	hit, miss, unitFilled := 0, 0, 0
	for i := range rows {
		bc := firstBarcode(rows[i])
		if bc == "" {
			continue
		}
		if v, ok := bc2meta[bc]; ok {
			rows[i].ItemNo = v.itemNo
			if v.unit != "" {
				rows[i].Unit = v.unit
				unitFilled++
			}
			hit++
		} else {
			miss++
		}
	}
	log.Printf("[enrichRowsWithItemNo] rows=%d barcode_unique=%d cube_rows=%d hits=%d misses=%d unit_filled=%d",
		len(rows), len(seen), len(cubeRes), hit, miss, unitFilled)
}

// firstBarcode 优先 matched (解析匹配后的) → fallback raw (OCR/VLM 原始识别)
//
//	matched_barcode 更可信 (经过 SkuMatcher 校准)
//	raw_barcode 是兜底 (matched 缺失时仍能定位商品)
//
// 与 session.js copyPurchaseData 的优先级保持一致
func firstBarcode(r model.SkuRow) string {
	if s := strings.TrimSpace(r.MatchedBarcode); s != "" {
		return s
	}
	return strings.TrimSpace(r.RawBarcode)
}
