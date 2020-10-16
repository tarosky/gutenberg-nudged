package main

import (
	"context"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/yookoala/gofast"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func createLogger() *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000000Z0700")
	log, err := cfg.Build(zap.WithCaller(false))
	if err != nil {
		panic("failed to initialize logger")
	}

	return log
}

var (
	log *zap.Logger
)

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
	}

	app.Action = func(c *cli.Context) error {
		log = createLogger()
		defer log.Sync()

		if nudgeType := c.String("nudge-type"); nudgeType != "mtime" {
			log.Fatal("illegal nudge-type parameter", zap.String("parameter", nudgeType))
		}

		ctx, cancel := context.WithCancel(context.Background())

		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, os.Kill)
		go func() {
			<-sig
			cancel()
		}()

		poll(
			ctx,
			createInvalidator(c.Path("fastcgi-socket"), c.Path("invalidator-file")),
			c.Path("nudge-file"),
			c.Int("check-interval"))

		<-ctx.Done()

		return nil
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Fatal("failed to run app", zap.Error(err))
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
				st, err := os.Stat(file)
				if err != nil {
					log.Warn("failed to stat file", zap.Error(err))
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
		}

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
		}
		resp.WriteTo(&ResWriter{}, os.Stderr)
	}
}
