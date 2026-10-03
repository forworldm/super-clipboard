// Command super-clipboard is the Go port of `python -m backend`.
//
// It loads the SUPER_CLIPBOARD_* configuration, opens (and migrates) the SQLite
// database, then serves the very same HTTP API as the FastAPI implementation.
package main

import (
	"log"
	"os"

	"github.com/pixia1234/super-clipboard/backend/internal/api"
	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	settings, err := config.Load()
	if err != nil {
		logger.Printf("ERROR:    配置加载失败 / configuration error: %v", err)
		os.Exit(1)
	}

	repo, err := repository.NewClipRepository(settings)
	if err != nil {
		logger.Printf("ERROR:    数据库初始化失败 / database error: %v", err)
		os.Exit(1)
	}
	defer repo.Close() //nolint:errcheck

	app := api.NewApp(settings, repo)
	if err := app.Run(); err != nil {
		logger.Printf("ERROR:    服务异常退出 / server stopped with error: %v", err)
		os.Exit(1)
	}
}
