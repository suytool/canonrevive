// CanonRevive 相机屏显还原 — a native fnOS app (Go) that restores the
// "camera LCD look" to exported photos: a bit brighter, punchier contrast,
// more vivid colors, plus optional dreamy filters.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/canonrevive/app/internal/server"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := envOr("PORT", "18080")
	dataDir := envOr("APP_DATA", "data")
	inDir := envOr("APP_IN", "share/in")
	outDir := envOr("APP_OUT", "share/out")

	// 数据目录必须可写（上传/临时文件依赖），失败则终止
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("无法创建数据目录 %s: %v", dataDir, err)
	}

	srv := server.New(dataDir, inDir, outDir, port)
	// 批量目录自愈：共享目录被清理工具删除时自动降级到数据目录，不阻断启动
	if warn, ok := srv.EnsureBatchDirs(); warn != "" {
		if ok {
			log.Printf("警告: %s", warn)
		} else {
			log.Printf("警告: %s（批量处理将不可用，单张处理正常）", warn)
		}
	}
	addr := ":" + port
	log.Printf("Canon助手 启动参数: PORT=%s APP_DATA=%s APP_IN=%s APP_OUT=%s", port, dataDir, inDir, outDir)

	// 绑定端口：应用重启/升级时旧实例可能正在退出、端口短暂未释放，
	// 这里做有界重试，避免“旧进程刚退、新进程立刻 bind 失败→误报端口被占用”。
	var ln net.Listener
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		if isAddrInUse(err) {
			log.Printf("端口 %s 暂被占用，等待旧实例退出后重试（%d/10）", port, attempt)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		log.Fatalf("无法监听端口 %s: %v", port, err)
	}
	if err != nil {
		log.Fatalf("端口 %s 被占用且等待后仍无法绑定：%v\n"+
			"若该端口被其他程序占用，请在应用设置中更换端口；若为应用异常残留，请重启 NAS 或停止残留进程后再启动。", port, err)
	}
	log.Printf("监听地址: http://localhost%s", addr)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
	}

	// 优雅关闭：fnOS 停止/升级应用时发 SIGTERM/SIGINT，正常关闭监听与在途请求，
	// 避免被强杀后留下占着端口的残留进程。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常: %v", serveErr)
		}
	}()

	sig := <-stop
	log.Printf("收到信号 %v，开始优雅关闭 ...", sig)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("优雅关闭超时，强制结束: %v", err)
		_ = httpSrv.Close()
	}
	log.Printf("已停止")
}

// isAddrInUse 判断错误是否为“地址已被占用”。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "bind:") && strings.Contains(msg, "in use")
}
