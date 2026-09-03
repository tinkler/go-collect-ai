package skill

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher fsnotify 热更新监听
//   - 启动时:把每个 root 加到 watcher,再递归加每个 skill 子目录
//   - 任何 CREATE/REMOVE/RENAME 在 skill 目录层级 → 触发 reload
//   - 任何 WRITE 到 SKILL.md → 触发 reload(允许编辑后即时生效)
//   - 防抖:200ms 内合并多个事件(编辑器常用"先清空再写入"模式,会触发 2 个事件)
type Watcher struct {
	store    *Store
	loader   Loader
	debounce time.Duration

	mu           sync.Mutex
	watcher      *fsnotify.Watcher
	stopCh       chan struct{}
	stopOnce     sync.Once
	pendingTimer *time.Timer
}

// Loader 抽象:把"扫描一组 root"封装成 func,便于测试时替换
type Loader func(roots []string) (*LoadResult, error)

// NewWatcher 构造并启动监听;不阻塞,出错返 error(调用方决定是否继续)
func NewWatcher(store *Store, roots []string, load Loader) (*Watcher, error) {
	w := &Watcher{
		store:    store,
		loader:   load,
		debounce: 200 * time.Millisecond,
		stopCh:   make(chan struct{}),
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify.NewWatcher: %w", err)
	}
	w.watcher = fsw

	// 1. 加入所有 root
	added := 0
	for _, root := range roots {
		if root == "" {
			continue
		}
		if err := fsw.Add(root); err != nil {
			// root 不存在不算硬错(可能后续 npx skills 装上时才会建)
			if !isNotExist(err) {
				log.Printf("[skill-watcher] add root %s: %v", root, err)
			}
			continue
		}
		added++

		// 2. 把 root 下现有 skill 子目录也加进去
		entries, _ := readDir(root)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillDir := filepath.Join(root, e.Name())
			_ = fsw.Add(skillDir)
		}
	}
	log.Printf("[skill-watcher] 启动,监听了 %d 个 root", added)

	go w.loop()
	return w, nil
}

// Stop 关闭监听
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		if w.watcher != nil {
			_ = w.watcher.Close()
		}
		if w.pendingTimer != nil {
			w.pendingTimer.Stop()
		}
	})
}

// loop 主循环:接收 fsnotify 事件 → 防抖 → 触发 reload
func (w *Watcher) loop() {
	for {
		select {
		case <-w.stopCh:
			return
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[skill-watcher] error: %v", err)
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	// 只关心 SKILL.md 和目录变化
	if !isRelevant(ev) {
		return
	}
	// 新建子目录 → 立即加入监听
	if ev.Has(fsnotify.Create) {
		if isDir(ev.Name) {
			_ = w.watcher.Add(ev.Name)
		}
	}
	// 防抖
	w.mu.Lock()
	if w.pendingTimer != nil {
		w.pendingTimer.Stop()
	}
	w.pendingTimer = time.AfterFunc(w.debounce, w.reload)
	w.mu.Unlock()
}

func (w *Watcher) reload() {
	roots := w.store.Roots()
	if len(roots) == 0 {
		return
	}
	res, err := w.loader(roots)
	if err != nil {
		log.Printf("[skill-watcher] reload: loader error: %v", err)
		return
	}
	w.store.Replace(res.Skills)
	log.Printf("[skill-watcher] reload: %d skill(s) now active", len(res.Skills))
	if msg := res.FormatErrors(); msg != "" {
		log.Printf("[skill-watcher] reload warnings:\n%s", msg)
	}
}

// === helpers ===

func isRelevant(ev fsnotify.Event) bool {
	// 关心:CREATE / REMOVE / WRITE / RENAME
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Remove) &&
		!ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Rename) {
		return false
	}
	base := filepath.Base(ev.Name)
	// 关注 SKILL.md 和子目录层级
	if base == "SKILL.md" {
		return true
	}
	// 子目录的 CREATE/REMOVE(技能增删)
	if isDir(ev.Name) {
		return true
	}
	return false
}

func isDir(path string) bool {
	info, err := statDir(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func isNotExist(err error) bool {
	return err != nil && (filepath.SkipDir == err ||
		// 简化:任何路径不存在的错误都放过
		err.Error() == "no such file or directory")
}
