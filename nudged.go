package main

import (
	"context"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/yookoala/gofast"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	log *zap.Logger
)

// This implements zapcore.WriteSyncer interface.
type lockedFileWriteSyncer struct {
	m    sync.Mutex
	f    *os.File
	path string
}

func newLockedFileWriteSyncer(path string) *lockedFileWriteSyncer {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error while creating log file: path: %s", err.Error())
		panic(err)
	}

	return &lockedFileWriteSyncer{
		f:    f,
		path: path,
	}
}

func (s *lockedFileWriteSyncer) Write(bs []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Write(bs)
}

func (s *lockedFileWriteSyncer) Sync() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Sync()
}

func (s *lockedFileWriteSyncer) reopen() {
	s.m.Lock()
	defer s.m.Unlock()

	if err := s.f.Close(); err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
	}

	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
		panic(err)
	}

	s.f = f
}

func (s *lockedFileWriteSyncer) Close() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Close()
}

func createLogger(ctx context.Context, logPath, errorLogPath string) *zap.Logger {
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        zapcore.OmitKey,
		CallerKey:      zapcore.OmitKey,
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  zapcore.OmitKey,
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})

	out := newLockedFileWriteSyncer(logPath)
	errOut := newLockedFileWriteSyncer(errorLogPath)

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)

	go func() {
		for {
			select {
			case _, ok := <-sigusr1:
				if !ok {
					break
				}
				out.reopen()
				errOut.reopen()
			case <-ctx.Done():
				signal.Stop(sigusr1)
				// closing sigusr1 causes panic (close of closed channel)
				break
			}
		}
	}()

	return zap.New(
		zapcore.NewCore(enc, out, zap.NewAtomicLevelAt(zap.DebugLevel)),
		zap.ErrorOutput(errOut),
		zap.Development(),
		zap.WithCaller(false))
}

func setPIDFile(path string) func() {
	if path == "" {
		return func() {}
	}

	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := ioutil.WriteFile(path, pid, 0644); err != nil {
		log.Panic(
			"failed to create PID file",
			zap.String("path", path),
			zap.Error(err))
	}

	return func() {
		if err := os.Remove(path); err != nil {
			log.Error(
				"failed to remove PID file",
				zap.String("path", path),
				zap.Error(err))
		}
	}
}

func main() {
	app := cli.NewApp()
	app.Name = "nudged"
	app.Description = "invalidate OPcache when nudged"

	app.Flags = []cli.Flag{
		&cli.PathFlag{
			Name:     "nudge-file",
			Aliases:  []string{"f"},
			Required: true,
			Usage:    "File to nudge when change happens.",
		},
		&cli.StringFlag{
			Name:    "nudge-type",
			Aliases: []string{"t"},
			Value:   "mtime",
			Usage:   "The way gutenberg-nudge nudges using the file. Currently the only supported value is mtime.",
		},
		&cli.IntFlag{
			Name:    "check-interval",
			Aliases: []string{"i"},
			Value:   1000,
			Usage:   "Polling interval.",
		},
		&cli.PathFlag{
			Name:     "invalidator-file",
			Aliases:  []string{"p"},
			Required: true,
			Usage:    "PHP file to invalidate OPcache.",
		},
		&cli.PathFlag{
			Name:     "fastcgi-socket",
			Aliases:  []string{"s"},
			Required: true,
			Usage:    "FastCGI domain socket path.",
		},
		&cli.PathFlag{
			Name:     "log-path",
			Aliases:  []string{"l"},
			Required: true,
		},
		&cli.PathFlag{
			Name:     "error-log-path",
			Aliases:  []string{"el"},
			Required: true,
		},
		&cli.PathFlag{
			Name:    "pid-file",
			Aliases: []string{"id"},
		},
	}

	app.Action = func(c *cli.Context) error {
		mustGetAbsPath := func(name string) string {
			path, err := filepath.Abs(c.Path(name))
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get %s: %s", name, err.Error())
				panic(err)
			}
			return path
		}

		log = createLogger(
			c.Context,
			mustGetAbsPath("log-path"),
			mustGetAbsPath("error-log-path"))
		defer log.Sync()

		if nudgeType := c.String("nudge-type"); nudgeType != "mtime" {
			log.Panic("illegal nudge-type parameter", zap.String("parameter", nudgeType))
		}

		removePIDFile := setPIDFile(mustGetAbsPath("pid-file"))
		defer removePIDFile()

		ctx, cancel := context.WithCancel(context.Background())

		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			defer func() {
				signal.Stop(sig)
				close(sig)
			}()

			<-sig
			cancel()
		}()

		invalidator := createInvalidator(
			mustGetAbsPath("fastcgi-socket"), mustGetAbsPath("invalidator-file"))
		poll(ctx, invalidator, mustGetAbsPath("nudge-file"), c.Int("check-interval"))

		<-ctx.Done()

		return nil
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Panic("failed to run app", zap.Error(err))
	}
}

// ResWriter is a simple implementation of http.ResponseWriter.
type ResWriter struct{}

// Header is.
func (w *ResWriter) Header() http.Header {
	return http.Header{}
}

// Write is.
func (w *ResWriter) Write(bs []byte) (int, error) {
	return os.Stderr.Write(bs)
}

// WriteHeader is.
func (w *ResWriter) WriteHeader(statusCode int) {
	log.Debug("php response", zap.Int("status code", statusCode))
}

func fileInfo(path string) os.FileInfo {
	// Ignore NFS cache by opening file before stat().
	f, err := os.Open(path)
	if err != nil {
		log.Error("failed to open nudge file", zap.Error(err))
		return nil
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Error("failed to close nudge file", zap.Error(err))
		}
	}()

	st, err := f.Stat()
	if err != nil {
		log.Error("failed to stat nudge file", zap.Error(err))
		return nil
	}

	return st
}

func poll(ctx context.Context, invalidate func(), file string, interval int) {
	go func() {
		mtime := time.Time{}
		pace := time.Duration(interval) * time.Millisecond
		pacemaker := time.NewTicker(pace)

		for {
			select {
			case <-ctx.Done():
				pacemaker.Stop()
				break

			case <-pacemaker.C:
				st := fileInfo(file)
				if st == nil {
					continue
				}

				t := st.ModTime()
				if mtime.Equal(t) {
					continue
				}

				mtime = t
				log.Debug("nudged", zap.Time("mtime", mtime))
				invalidate()
			}
		}
	}()
}

func createInvalidator(socket, phpFile string) func() {
	return func() {
		connFactory := gofast.SimpleConnFactory("unix", socket)

		client, err := gofast.SimpleClientFactory(connFactory, 0)()
		if err != nil {
			log.Error("client", zap.Error(err))
			return
		}
		defer client.Close()

		resp, err := client.Do(&gofast.Request{
			Role: gofast.RoleResponder,
			Params: map[string]string{
				"SCRIPT_FILENAME": phpFile,
				"REQUEST_METHOD":  "POST",
				// "GATEWAY_INTERFACE": "",
				// "SERVER_SOFTWARE":   "",
				// "QUERY_STRING":      "",
				// "CONTENT_TYPE":      "",
				// "SCRIPT_NAME":       "",
				// "REQUEST_URI":       "",
				// "DOCUMENT_URI":      "",
				// "DOCUMENT_ROOT":     "",
				// "SERVER_PROTOCOL":   "",
				// "REQUEST_SCHEME":    "",
				// "REMOTE_ADDR":       "",
				// "SERVER_ADDR":       "",
				// "SERVER_NAME":       "",
				// "CONTENT_LENGTH":    "",
				// "REMOTE_PORT":       "",
				// "SERVER_PORT":       "",
			},
		})
		if err != nil {
			log.Error("fastcgi failure", zap.Error(err))
			return
		}
		resp.WriteTo(&ResWriter{}, os.Stderr)
	}
}
