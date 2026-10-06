// Package server implements the HTTP API and web UI for the app.
package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/canonrevive/app/internal/imageproc"
	"github.com/disintegration/imaging"
	"github.com/rwcarlsen/goexif/exif"
)

//go:embed all:web
var webFS embed.FS

// Server holds runtime state.
type Server struct {
	dataDir    string // temp / upload storage
	inDir      string // 当前生效的批量输入目录（可能已降级）
	outDir     string // 当前生效的批量输出目录（可能已降级）
	origInDir  string // 配置的共享输入目录（降级探测基准）
	origOutDir string // 配置的共享输出目录
	port       string
	dirMu      sync.Mutex
	degraded   bool // 共享目录不可用、已降级到数据目录时为 true
}

func New(dataDir, inDir, outDir, port string) *Server {
	return &Server{dataDir: dataDir, inDir: inDir, outDir: outDir, origInDir: inDir, origOutDir: outDir, port: port}
}

// EnsureBatchDirs 确保批量输入/输出目录存在并可写。
// 共享目录可能被系统清理工具删除且应用无权重建父目录，
// 此时自动降级到应用数据目录下的 batch_in/batch_out，保证批量功能始终可用；
// 共享目录恢复后会自动切回。返回值 warn 为降级/警告说明（空串表示正常），ok=false 表示彻底不可用。
func (s *Server) EnsureBatchDirs() (warn string, ok bool) {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	if err := os.MkdirAll(s.origInDir, 0o755); err == nil {
		if err2 := os.MkdirAll(s.origOutDir, 0o755); err2 == nil {
			s.inDir = s.origInDir
			s.outDir = s.origOutDir
			s.degraded = false
			return "", true
		}
	}
	// 降级：应用数据目录（应用自身数据区，一定可写）
	fbIn := filepath.Join(s.dataDir, "batch_in")
	fbOut := filepath.Join(s.dataDir, "batch_out")
	if err := os.MkdirAll(fbIn, 0o755); err != nil {
		return "批量输入目录创建失败: " + err.Error(), false
	}
	if err := os.MkdirAll(fbOut, 0o755); err != nil {
		return "批量输出目录创建失败: " + err.Error(), false
	}
	s.inDir = fbIn
	s.outDir = fbOut
	wasDegraded := s.degraded
	s.degraded = true
	if !wasDegraded {
		return "共享目录不可用（可能被清理工具删除），已自动切换到应用数据目录 " + fbIn + "，批量功能仍可使用", true
	}
	return "", true
}

const (
	previewMaxEdge = 1600
	maxUploadBytes = 64 << 20 // 64MB
	exportQuality  = 95
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ready", s.handleReady)
	mux.HandleFunc("/api/presets", s.handlePresets)
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/render", s.handleRender)
	mux.HandleFunc("/api/batch", s.handleBatch)
	mux.Handle("/api/original/", http.StripPrefix("/api/original/", http.HandlerFunc(s.handleOriginal)))
	mux.Handle("/api/preview/", http.StripPrefix("/api/preview/", http.HandlerFunc(s.handlePreview)))
	mux.HandleFunc("/", s.handleIndex)
	return logRequests(recoverMiddleware(adaptPrefix(mux)))
}

// handleReady 供 fpk 启动脚本做就绪探测：只要 HTTP 服务起来了即返回 200。
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

// recoverMiddleware 兜住任何 handler 里的 panic（例如个别损坏图片触发的解码/越界），
// 返回 500 而不是让整个进程崩溃退出——这是应用"用着用着异常退出"的关键防线。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("请求处理异常已恢复 %s %s: %v", r.Method, r.URL.Path, rec)
				if !sw.wrote {
					writeJSONStatus(sw, http.StatusInternalServerError, map[string]any{"error": "处理该图片时出错，请换一张或调整参数后重试"})
				}
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// adaptPrefix 兼容 fnOS 内置 Nginx 反向代理的子路径访问。
// 直连模式下路径为 /、/api/xxx、/app.js；反代(CGI)模式下系统会带上应用前缀，
// 形如 /app-path/api/xxx、/app-path/app.js。这里统一剥掉前缀，两种部署方式都能工作。
func adaptPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/" && !strings.HasPrefix(p, "/api/") {
			if i := strings.Index(p, "/api/"); i >= 0 {
				// /app-path/api/xxx → /api/xxx
				r.URL.Path = p[i:]
			} else if isAssetName(filepath.Base(p)) {
				// /app-path/app.js → /app.js（静态资源均位于 web 根目录）
				r.URL.Path = "/" + filepath.Base(p)
			} else {
				// /app-path 或 /app-path/ → 首页
				r.URL.Path = "/"
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isAssetName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".css", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".gif", ".webp", ".html":
		return true
	}
	return false
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	// serve embedded static web assets (index.html / app.js / style.css)
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "text/plain; charset=utf-8"
	switch filepath.Ext(path) {
	case ".html":
		ct = "text/html; charset=utf-8"
	case ".js":
		ct = "application/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	case ".png":
		ct = "image/png"
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	// 禁止缓存：应用升级后浏览器必须加载最新前端资源
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(data)
}

// ---------------------------------------------------------------------------
// API: presets

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, imageproc.DefaultPresets())
}

// API: info — returns batch directories
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	writeJSON(w, map[string]any{"inDir": s.inDir, "outDir": s.outDir, "degraded": s.degraded})
}

// ---------------------------------------------------------------------------
// API: upload

type uploadResp struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	PreviewURL string `json:"previewUrl"`
	OriginalURL string `json:"originalUrl"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "请选择要上传的图片文件", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if !isImageExt(header.Filename) {
		http.Error(w, "仅支持 JPG/JPEG/PNG 图片", http.StatusBadRequest)
		return
	}

	// 提取原始 EXIF APP1 段（修正 Orientation 后保存，渲染时插回）
	exifSeg := extractUploadExif(file)

	img := decodeWithOrientation(file)
	if img == nil {
		http.Error(w, "无法解析图片，请确认是有效的 JPG/PNG 文件", http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("%d_%s", time.Now().UnixNano(), randHex(8))
	path := filepath.Join(s.dataDir, id+".jpg")
	if err := saveJPEG(path, img, 95); err != nil {
		http.Error(w, "保存图片失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if exifSeg != nil {
		os.WriteFile(filepath.Join(s.dataDir, id+".exif"), exifSeg, 0o644)
	}
	s.cleanupOld(2 * time.Hour)

	b := img.Bounds()
	writeJSON(w, uploadResp{
		ID:          id,
		Name:        header.Filename,
		Width:       b.Dx(),
		Height:      b.Dy(),
		PreviewURL:  "/api/preview/" + id,
		OriginalURL: "/api/original/" + id,
	})
}

// handlePreview serves a downscaled JPEG for fast client-side live preview.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.URL.Path, "/")
	img, err := s.load(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	thumb := imaging.Fit(img, previewMaxEdge, previewMaxEdge, imaging.Lanczos)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	jpeg.Encode(w, thumb, &jpeg.Options{Quality: 82})
}

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.URL.Path, "/")
	img, err := s.load(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	jpeg.Encode(w, img, &jpeg.Options{Quality: 92})
}

func (s *Server) load(id string) (image.Image, error) {
	if !validID(id) {
		return nil, fmt.Errorf("bad id")
	}
	f, err := os.Open(filepath.Join(s.dataDir, id+".jpg"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jpeg.Decode(f)
}

// ---------------------------------------------------------------------------
// API: render

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     string            `json:"id"`
		Params imageproc.Params  `json:"params"`
		File   string            `json:"file"` // output filename hint
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "参数错误: "+err.Error(), http.StatusBadRequest)
		return
	}
	img, err := s.load(req.ID)
	if err != nil {
		http.Error(w, "图片不存在或已过期，请重新上传", http.StatusNotFound)
		return
	}
	out := imageproc.Apply(img, req.Params)

	name := req.File
	if name == "" || !strings.HasSuffix(strings.ToLower(name), ".jpg") {
		name = "restored.jpg"
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	writeJPEGWithExif(w, out, s.exifPath(req.ID))
}

// ---------------------------------------------------------------------------
// API: batch

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Params imageproc.Params `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "参数错误: " + err.Error()})
		return
	}
	warn, ok := s.EnsureBatchDirs()
	if !ok {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": warn})
		return
	}
	if warn == "" && s.degraded {
		warn = "共享目录不可用（可能被清理工具删除），当前使用应用数据目录 " + s.inDir + "，批量功能正常；建议重启应用以恢复共享目录"
	}

	entries, _ := os.ReadDir(s.inDir)
	processed := []string{}
	skipped := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isImageExt(name) {
			continue
		}
		srcPath := filepath.Join(s.inDir, name)
		raw, err := os.ReadFile(srcPath)
		if err != nil {
			skipped = append(skipped, name+"（读取失败）")
			continue
		}
		exifSeg := fixExifOrientation(extractExifAPP1(raw))
		orient := orientationOf(bytes.NewReader(raw))
		var img image.Image
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("批量处理 %s 异常已跳过: %v", name, rec)
					img = nil
				}
			}()
			decoded, _, derr := image.Decode(bytes.NewReader(raw))
			if derr != nil {
				skipped = append(skipped, name+"（解码失败）")
				return
			}
			img = applyOrientationInt(decoded, orient)
			out := imageproc.Apply(img, req.Params)
			outPath := filepath.Join(s.outDir, baseOutName(name))
			if err := saveJPEGWithExif(outPath, out, exifSeg); err != nil {
				skipped = append(skipped, name+"（写入失败）")
				return
			}
			processed = append(processed, name)
		}()
		if img == nil && !containsSkip(skipped, name) {
			skipped = append(skipped, name+"（处理失败）")
		}
	}
	writeJSON(w, map[string]any{"processed": processed, "skipped": skipped, "inDir": s.inDir, "outDir": s.outDir, "warning": warn})
}

func containsSkip(skipped []string, name string) bool {
	prefix := name + "（"
	for _, s := range skipped {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// helpers

func (s *Server) cleanupOld(age time.Duration) {
	cutoff := time.Now().Add(-age)
	entries, _ := os.ReadDir(s.dataDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(s.dataDir, e.Name()))
		}
	}
}

func saveJPEG(path string, img image.Image, quality int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
}

// writeJPEGWithExif encodes img as JPEG and writes it to w with the original
// Exif APP1 segment re-inserted (if available).
func writeJPEGWithExif(w io.Writer, img image.Image, exifPath string) error {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: exportQuality}); err != nil {
		return err
	}
	data := buf.Bytes()
	if exif, err := os.ReadFile(exifPath); err == nil && len(exif) > 0 {
		data = injectAPP1(data, exif)
	}
	_, err := w.Write(data)
	return err
}

// saveJPEGWithExif writes a JPEG file with the Exif APP1 segment re-inserted.
func saveJPEGWithExif(path string, img image.Image, exifSeg []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: exportQuality}); err != nil {
		return err
	}
	data := buf.Bytes()
	if len(exifSeg) > 0 {
		data = injectAPP1(data, exifSeg)
	}
	_, err = f.Write(data)
	return err
}

// extractUploadExif reads the multipart file bytes and returns the fixed Exif
// APP1 segment, restoring the reader position afterwards.
func extractUploadExif(file io.ReadSeeker) []byte {
	file.Seek(0, io.SeekStart)
	raw, err := io.ReadAll(io.LimitReader(file, maxUploadBytes))
	file.Seek(0, io.SeekStart)
	if err != nil {
		return nil
	}
	return fixExifOrientation(extractExifAPP1(raw))
}

func (s *Server) exifPath(id string) string {
	return filepath.Join(s.dataDir, id+".exif")
}

func isImageExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png"
}

func baseOutName(name string) string {
	base := strings.TrimSuffix(strings.ToLower(name), filepath.Ext(name))
	return base + "_restored.jpg"
}

// decodeWithOrientation reads a JPEG (or falls back to a generic decoder),
// applies the EXIF orientation and returns an upright image.
func decodeWithOrientation(r io.ReadSeeker) image.Image {
	orient := 1
	r.Seek(0, io.SeekStart)
	orient = orientationOf(r)
	r.Seek(0, io.SeekStart)
	img, err := jpeg.Decode(r)
	if err != nil {
		r.Seek(0, io.SeekStart)
		img, _, err = image.Decode(r)
		if err != nil {
			return nil
		}
	}
	return applyOrientationInt(img, orient)
}

// orientationOf extracts the EXIF orientation tag (defaults to 1).
func orientationOf(src io.Reader) int {
	orient := 1
	if ex, err := exif.Decode(src); err == nil {
		if tag, err := ex.Get(exif.Orientation); err == nil {
			if v, err := tag.Int(0); err == nil && v >= 1 && v <= 8 {
				orient = v
			}
		}
	}
	return orient
}

// applyOrientationInt rotates/flips the image according to an EXIF orientation.
func applyOrientationInt(img image.Image, orient int) image.Image {
	switch orient {
	case 1:
		return img
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.FlipV(imaging.Rotate270(img))
	case 6:
		return imaging.Rotate270(img) // 90° clockwise
	case 7:
		return imaging.FlipV(imaging.Rotate90(img))
	case 8:
		return imaging.Rotate90(img) // 270° clockwise
	}
	return img
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func validID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '_') {
			return false
		}
	}
	return true
}

var randHex = func(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := range b {
		now = now*6364136223846793005 + 1442695040888963407
		b[i] = hex[(now>>33)&0xf]
	}
	return string(b)
}
