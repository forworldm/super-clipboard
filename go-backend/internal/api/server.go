// Package api is the Go port of backend/main.py: the FastAPI application,
// its routes, the CORS middleware, the static mounts and the cleanup worker.
package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
	"github.com/pixia1234/super-clipboard/backend/internal/utils"
)

// staticMount mirrors app.mount("/static", StaticFiles(directory=...)).
type staticMount struct {
	prefix string
	dir    string
}

// App is the FastAPI `app` object.
type App struct {
	Settings *config.Settings
	Repo     *repository.ClipRepository

	router *router
	mounts []staticMount
	logger *log.Logger
	client *http.Client
}

// NewApp builds the application. Route registration order matches main.py,
// which matters because Starlette serves the first matching route.
func NewApp(settings *config.Settings, repo *repository.ClipRepository) *App {
	app := &App{
		Settings: settings,
		Repo:     repo,
		router:   newRouter(),
		logger:   log.New(os.Stderr, "", log.LstdFlags),
		client:   &http.Client{Timeout: time.Duration(settings.CaptchaTimeoutSeconds * float64(time.Second))},
	}

	// if settings.static_root.exists(): app.mount("/static", ...) / app.mount("/assets", ...)
	if settings.StaticRootExists() {
		app.mounts = append(app.mounts, staticMount{prefix: "/static", dir: settings.StaticRoot})
		if settings.AssetsDirExists() {
			app.mounts = append(app.mounts, staticMount{prefix: "/assets", dir: settings.AssetsDir()})
		}
	}

	app.router.add("/healthz", []string{http.MethodGet}, app.handleHealthz)
	app.router.add("/", []string{http.MethodGet}, app.handleIndex)

	app.router.add("/api/clips", []string{http.MethodGet}, app.handleListClips)
	app.router.add("/api/tokens/register", []string{http.MethodPost}, app.handleRegisterToken)
	app.router.add("/api/config", []string{http.MethodGet}, app.handleConfig)
	app.router.add("/api/clips/{clip_id}", []string{http.MethodGet}, app.handleGetClip)
	app.router.add("/api/clips/code/{access_code}", []string{http.MethodGet}, app.handleGetClipByCode)
	app.router.add("/api/clips", []string{http.MethodPost}, app.handleCreateClip)
	app.router.add("/api/clips/{clip_id}", []string{http.MethodDelete}, app.handleDeleteClip)
	app.router.add("/api/clips/{clip_id}/download", []string{http.MethodPost}, app.handleTrackDownload)
	app.router.add("/api/clips/{clip_id}/file", []string{http.MethodGet}, app.handleDownloadFile)

	// Chunked uploads (must precede /{access_code} catch-alls).
	app.router.add("/api/uploads/init", []string{http.MethodPost}, app.handleInitUpload)
	app.router.add("/api/uploads/{id}/chunks/{index}", []string{http.MethodPut}, app.handlePutChunk)
	app.router.add("/api/uploads/{id}", []string{http.MethodGet}, app.handleGetUpload)
	app.router.add("/api/uploads/{id}/complete", []string{http.MethodPost}, app.handleCompleteUpload)
	app.router.add("/api/uploads/{id}", []string{http.MethodDelete}, app.handleAbortUpload)

	app.router.add("/{access_code}/raw", []string{http.MethodGet}, app.handleResolveRaw)
	app.router.add("/{access_code}", []string{http.MethodGet}, app.handleResolve)

	return app
}

// Handler returns the composed http.Handler (middleware + routes).
func (a *App) Handler() http.Handler {
	return a.recoverMiddleware(a.accessLog(corsMiddleware(staticMiddleware(a.mounts, a.router))))
}

// ServeHTTP makes *App itself usable as an http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.Handler().ServeHTTP(w, r)
}

func (a *App) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Printf("ERROR:    unhandled panic while serving %s %s: %v", r.Method, r.URL.Path, recovered)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog mirrors uvicorn's request logging.
func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(recorder, r)
		client := utils.ExtractClientIP(r)
		if client == "" {
			client = "-"
		}
		a.logger.Printf("INFO:     %s - \"%s %s %s\" %d %.0fms",
			client, r.Method, r.URL.RequestURI(), r.Proto, recorder.status, float64(time.Since(started).Microseconds())/1000.0)
	})
}

// staticMiddleware mirrors the StaticFiles mounts: they are evaluated before
// the routed endpoints, exactly like Starlette does.
func staticMiddleware(mounts []staticMount, next http.Handler) http.Handler {
	if len(mounts) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath := requestPath(r)
		for _, mount := range mounts {
			if requestPath != mount.prefix && !strings.HasPrefix(requestPath, mount.prefix+"/") {
				continue
			}
			serveStaticFile(w, r, mount, requestPath)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveStaticFile(w http.ResponseWriter, r *http.Request, mount staticMount, requestPath string) {
	remainder := strings.TrimPrefix(requestPath, mount.prefix)
	root, err := filepath.Abs(mount.dir)
	if err != nil {
		root = mount.dir
	}
	target, ok := safeJoinRoot(root, remainder)
	if !ok {
		writePlainText(w, http.StatusNotFound, "Not Found")
		return
	}
	file, err := os.Open(target)
	if err != nil {
		writePlainText(w, http.StatusNotFound, "Not Found")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writePlainText(w, http.StatusNotFound, "Not Found")
		return
	}

	// StaticFiles uses FileResponse without a filename: the media type is
	// guessed from the extension (falling back to text/plain) and no
	// Content-Disposition header is emitted.
	mediaType := mime.TypeByExtension(filepath.Ext(target))
	if mediaType == "" {
		mediaType = "text/plain"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// safeJoinRoot maps a URL remainder onto a file under root. The remainder is
// cleaned as a URL path (so ".." cannot walk out of the mount) and the final
// filesystem path is checked with filepath.Rel as defense in depth.
//
// Unlike a naive strings.Contains(remainder, "..") check this allows legitimate
// names such as "vendor..chunk.js".
func safeJoinRoot(root, remainder string) (string, bool) {
	remainder = strings.TrimPrefix(remainder, "/")
	if remainder == "" || strings.ContainsRune(remainder, 0) {
		return "", false
	}
	cleaned := path.Clean("/" + remainder)
	if cleaned == "/" || cleaned == "." {
		return "", false
	}
	relative := strings.TrimPrefix(cleaned, "/")
	if relative == "" {
		return "", false
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return absTarget, true
}

// Run reproduces the uvicorn entrypoint: startup purge, periodic cleanup worker
// (asyncio.create_task in main.py) and graceful shutdown on SIGINT/SIGTERM.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.RunWithContext(ctx)
}

// RunWithContext serves until ctx is cancelled.
func (a *App) RunWithContext(ctx context.Context) error {
	// @app.on_event("startup")
	if purged, err := a.Repo.PurgeInactive(); err != nil {
		a.logger.Printf("ERROR:    startup purge failed: %v", err)
	} else if purged > 0 {
		a.logger.Printf("INFO:     startup purge removed %d inactive clip(s)", purged)
	}
	// Chunked-upload crash recovery (power loss / kill -9 mid-assembly).
	a.ReconcileUploadsOnStartup()

	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	go a.cleanupWorker(workerCtx)

	address := fmt.Sprintf("%s:%d", a.Settings.AppHost, a.Settings.AppPort)
	server := &http.Server{
		Addr:              address,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errChan := make(chan error, 1)
	go func() {
		a.logger.Printf("INFO:     Uvicorn equivalent listening on http://%s (Go port)", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
			return
		}
		errChan <- nil
	}()

	select {
	case err := <-errChan:
		cancelWorker()
		return err
	case <-ctx.Done():
		a.logger.Printf("INFO:     shutting down")
		cancelWorker()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		<-errChan
		if err := a.Repo.Close(); err != nil {
			return err
		}
		return nil
	}
}

// cleanupWorker mirrors `async def cleanup_worker()` in main.py.
func (a *App) cleanupWorker(ctx context.Context) {
	interval := time.Duration(a.Settings.CleanupIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 300 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.Repo.PurgeInactive(); err != nil {
				a.logger.Printf("ERROR:    cleanup worker failed: %v", err)
			}
			// Timeout rollback for abandoned upload sessions (cancel without
			// DELETE, network drop, browser closed mid-upload).
			a.purgeExpiredUploadsPeriodic()
		}
	}
}
