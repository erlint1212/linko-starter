package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

type closeFunc func()

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func initializeLogger() (*slog.Logger, closeFunc, error) {
	logFilePath, exists := os.LookupEnv("LINKO_LOG_FILE")

	if exists {
		multiLoggerFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, func() {}, fmt.Errorf("failed to open log file: %v", err)
		}

		bufferedFile := bufio.NewWriterSize(multiLoggerFile, 8192)

		cleanup := func() {
			if err := bufferedFile.Flush(); err != nil {
				fmt.Printf("error flushing buffer to file: %v", err)
			}
			if err := multiLoggerFile.Close(); err != nil {
				fmt.Printf("error closing log file: %v", err)
			}
		}

		return slog.New(slog.NewTextHandler(bufferedFile, nil)), cleanup, nil
	}

	return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}, nil

}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {

	logger, cleanup, err := initializeLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer cleanup()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Info(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Info(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Info(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}
