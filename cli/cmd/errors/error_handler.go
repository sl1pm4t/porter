package errors

import (
	"fmt"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/getsentry/sentry-go"
	"github.com/porter-dev/porter/cli/cmd/config"
)

var SentryDSN string = ""

type errorHandler interface {
	HandleError(error)
}

type standardErrorHandler struct{}

func (h *standardErrorHandler) HandleError(err error) {
	color.New(color.FgRed).Fprintf(os.Stderr, "error: %s\n", err.Error())
}

type sentryErrorHandler struct{}

func (h *sentryErrorHandler) HandleError(err error) {
	if SentryDSN != "" {
		localHub := sentry.CurrentHub().Clone()

		localHub.ConfigureScope(func(scope *sentry.Scope) {
			scope.SetTags(map[string]string{
				"host":    config.GetCLIConfig().Host,
				"project": fmt.Sprintf("%d", config.GetCLIConfig().Project),
				"cluster": fmt.Sprintf("%d", config.GetCLIConfig().Cluster),
			})
		})

		localHub.CaptureException(err)
		sentry.Flush(2 * time.Second)
	}

	color.New(color.FgRed).Fprintf(os.Stderr, "error: %s\n", err.Error())
}

func GetErrorHandler() errorHandler {
	if SentryDSN != "" {
		return &sentryErrorHandler{}
	}

	return &standardErrorHandler{}
}
