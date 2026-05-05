package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
)

type closeFunc func()

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", err.Error()),
	}

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stackErr.StackTrace())))
	}


	customAttrs := linkoerr.Attrs(err)

	if len(customAttrs) > 0 {
		attrs = append(attrs, customAttrs...)
	}

	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}


		multiErr, meOk := errors.AsType[multiError](err)
		if meOk {
			var attrs []slog.Attr

			for i, err := range multiErr.Unwrap() {
				errExtra := errorAttrs(err)
				attrs = append(attrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errExtra...))
			}

			return slog.GroupAttrs("errors", attrs...)
		} 

		errExtra := errorAttrs(err)

		return slog.GroupAttrs("error", errExtra...)

	}
	return a
}

func initializeLogger() (*slog.Logger, closeFunc, error) {

	debugHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})

	logFilePath, exists := os.LookupEnv("LINKO_LOG_FILE")

	if exists {
		multiLoggerFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, func() {}, fmt.Errorf("failed to open log file: %v", err)
		}

		bufferedFile := bufio.NewWriterSize(multiLoggerFile, 8192)

		cleanup := func() {
			if err := bufferedFile.Flush(); err != nil {
				log.Printf("error flushing buffer to file: %v", err)
			}
			if err := multiLoggerFile.Close(); err != nil {
				log.Printf("error closing log file: %v", err)
			}
		}

		infoHandler := slog.NewJSONHandler(multiLoggerFile, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})

		logger := slog.New(slog.NewMultiHandler(
			debugHandler,
			infoHandler,
		))

		return logger, cleanup, nil
	}

	logger := slog.New(slog.NewMultiHandler(
		debugHandler,
	))

	return logger, func() {}, nil

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
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
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
		logger.Debug(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}
