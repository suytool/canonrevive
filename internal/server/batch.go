package server

import (
	"bytes"
	"encoding/json"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/canonrevive/app/internal/imageproc"
)

// BatchJob 是独立于任何 HTTP 请求、在服务端后台运行的批量任务。
// 即使关闭界面、断开连接，任务仍继续执行，进度与结果保留在内存中，
// 重新打开页面时通过状态查询恢复，不会因界面关闭而中止。
type BatchJob struct {
	mu sync.Mutex

	names  []string        // 启动时快照的待处理文件名
	params imageproc.Params // 处理参数
	inDir  string          // 实际输入目录
	outDir string          // 实际输出目录

	total     int      // 待处理总数
	done      int      // 已处理（含失败）张数
	current   string   // 当前正在处理的文件名
	ok        int      // 成功张数
	processed []string // 成功文件名
	skipped   []string // 跳过项（含原因）

	running   bool      // 是否正在运行
	finished  bool      // 是否正常完成
	canceled  bool      // 是否被用户请求停止
	warning   string    // 降级等提示
	createdAt time.Time // 创建时间
}

// snapshot 返回当前状态的 JSON 友好快照（线程安全）。
func (j *BatchJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := "idle"
	switch {
	case j.running:
		state = "running"
	case j.canceled:
		state = "canceled"
	case j.finished:
		state = "finished"
	}
	return map[string]any{
		"state":     state,
		"running":   j.running,
		"finished":  j.finished,
		"canceled":  j.canceled,
		"total":     j.total,
		"done":      j.done,
		"current":   j.current,
		"ok":        j.ok,
		"skip":      len(j.skipped),
		"processed": j.processed,
		"skipped":   j.skipped,
		"inDir":     j.inDir,
		"outDir":    j.outDir,
		"warning":   j.warning,
	}
}

// handleBatchStart 启动（或复用）一个后台批量任务。
// 任务启动后立即返回，实际处理在独立 goroutine 中进行。
func (s *Server) handleBatchStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req struct {
		Params imageproc.Params `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "参数错误: " + err.Error()})
		return
	}

	// 已有正在运行的任务：不重复启动，直接返回当前状态（幂等）。
	s.jobMu.Lock()
	if s.job != nil && s.job.running {
		cur := s.job.snapshot()
		cur["alreadyRunning"] = true
		s.jobMu.Unlock()
		writeJSON(w, cur)
		return
	}
	s.jobMu.Unlock()

	// 准备批量目录（含降级逻辑）。
	warn, ok := s.EnsureBatchDirs()
	if !ok {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": warn})
		return
	}
	if warn == "" && s.degraded {
		warn = "共享目录不可用（可能被清理工具删除），当前使用应用数据目录 " + s.inDir + "，批量功能正常；建议重启应用以恢复共享目录"
	}

	// 扫描待处理图片（启动时快照，保证总数准确）。
	entries, _ := os.ReadDir(s.inDir)
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && isImageExt(e.Name()) {
			names = append(names, e.Name())
		}
	}

	job := &BatchJob{
		names:     names,
		params:    req.Params,
		inDir:     s.inDir,
		outDir:    s.outDir,
		total:     len(names),
		processed: []string{},
		skipped:   []string{},
		running:   true,
		warning:   warn,
		createdAt: time.Now(),
	}
	s.jobMu.Lock()
	s.job = job
	s.jobMu.Unlock()

	go s.runBatch(job)

	writeJSON(w, job.snapshot())
}

// runBatch 在后台执行任务，不依赖任何请求上下文；
// 关闭界面不影响它，只有“停止”请求会置 canceled 标志。
func (s *Server) runBatch(job *BatchJob) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("批量后台任务异常终止: %v", rec)
		}
		job.mu.Lock()
		job.running = false
		job.finished = !job.canceled
		job.current = ""
		job.mu.Unlock()
	}()

	for idx, name := range job.names {
		job.mu.Lock()
		canceled := job.canceled
		job.current = name
		job.mu.Unlock()
		if canceled {
			break
		}

		failed := s.processOne(job, name)

		job.mu.Lock()
		job.done = idx + 1
		if !failed {
			job.ok++
			job.processed = append(job.processed, name)
		}
		job.mu.Unlock()
	}
}

// processOne 处理单张图片；失败时记录原因并返回 true。
func (s *Server) processOne(job *BatchJob, name string) bool {
	srcPath := filepath.Join(job.inDir, name)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		job.mu.Lock()
		job.skipped = append(job.skipped, name+"（读取失败）")
		job.mu.Unlock()
		return true
	}
	exifSeg := fixExifOrientation(extractExifAPP1(raw))
	orient := orientationOf(bytes.NewReader(raw))

	var img image.Image
	var failReason string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("批量处理 %s 异常已跳过: %v", name, rec)
				failReason = name + "（处理失败）"
				img = nil
			}
		}()
		decoded, _, derr := image.Decode(bytes.NewReader(raw))
		if derr != nil {
			failReason = name + "（解码失败）"
			return
		}
		img = applyOrientationInt(decoded, orient)
		out := imageproc.Apply(img, job.params)
		outPath := filepath.Join(job.outDir, baseOutName(name))
		if werr := saveJPEGWithExif(outPath, out, exifSeg); werr != nil {
			failReason = name + "（写入失败）"
			return
		}
	}()
	if failReason != "" {
		job.mu.Lock()
		job.skipped = append(job.skipped, failReason)
		job.mu.Unlock()
		return true
	}
	return false
}

// handleBatchStatus 返回当前任务的状态（用于前端轮询和进入页面恢复）。
func (s *Server) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.jobMu.Lock()
	job := s.job
	s.jobMu.Unlock()
	if job == nil {
		writeJSON(w, map[string]any{"state": "idle", "running": false, "finished": false, "canceled": false, "total": 0, "done": 0})
		return
	}
	writeJSON(w, job.snapshot())
}

// handleBatchStop 请求停止后台任务；任务在处理完当前张（或检查到标志）后停止。
func (s *Server) handleBatchStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.jobMu.Lock()
	job := s.job
	s.jobMu.Unlock()
	if job == nil {
		writeJSON(w, map[string]any{"state": "idle"})
		return
	}
	job.mu.Lock()
	if job.running {
		job.canceled = true
	}
	job.mu.Unlock()
	writeJSON(w, job.snapshot())
}
