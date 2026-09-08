package glmocr

// 智谱同步文件解析 /files/parser/sync 类型定义 (2026-09-04 从 tin-nova/pkg/glmocr 裁剪)
//
// 注意: 智谱有多个 OCR/解析端点, 能力完全不同:
//   - /paas/v4/files/ocr           仅手写体 (hand_write), 印刷体会乱码 → 本包不含
//   - /paas/v4/files/parser/sync   ⭐印刷体/表格/复杂版面 (prime-sync) → 本包唯一入口
//   - /paas/v4/layout_parsing      glm-ocr 大模型 Markdown+布局 → 本包不含

// ToolType /files/parser/sync 的解析工具类型
type ToolType string

// ToolTypePrimeSync 高精度同步解析: 印刷体、表格、复杂版面、公式
const ToolTypePrimeSync ToolType = "prime-sync"

// ParserFileType 文件解析同步 API 支持的文件类型 (全大写)
type ParserFileType string

const (
	FilePDF  ParserFileType = "PDF"
	FilePNG  ParserFileType = "PNG"
	FileJPG  ParserFileType = "JPG"
	FileJPEG ParserFileType = "JPEG"
	FileBMP  ParserFileType = "BMP"
	FileWEBP ParserFileType = "WEBP"
)

// TaskStatus 任务处理状态
type TaskStatus string

const (
	StatusSucceeded TaskStatus = "succeeded"
	StatusFailed    TaskStatus = "failed"
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
)

// FileParserSyncRequest 同步文件解析请求 (⭐印刷体、表格、复杂版面首选)
type FileParserSyncRequest struct {
	// FileData 文件二进制数据 (PNG/JPG/BMP/WEBP/PDF)
	FileData []byte
	// FileName 文件名, 用于解析 file_type
	FileName string
	// FileType 显式指定文件类型 (全大写); 留空按 FileName 扩展名推断
	FileType ParserFileType
	// ToolType 解析工具: 留空 = ToolTypePrimeSync
	ToolType ToolType
}

// FileParserSyncResponse 同步解析响应
type FileParserSyncResponse struct {
	TaskID             string     `json:"task_id"`
	Message            string     `json:"message"`
	Status             TaskStatus `json:"status"`
	Content            string     `json:"content"`            // 纯文本解析结果 ⭐
	ParsingResultURL   string     `json:"parsing_result_url"` // 下载链接: 图片 + Markdown + 布局 JSON (24h 有效)
	DownloadExpireHour int        `json:"download_expire_hour,omitempty"`
}

// OCRError 智谱 API 错误响应
type OCRError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *OCRError) Error() string {
	return "glm parser error: " + e.Code + ": " + e.Message
}
