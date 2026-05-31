package proxy

import (
	"context"
	"errors"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

type Runner struct{}

func (Runner) RunProxy(context.Context, proxyconfig.Config) error {
	return errors.New("proxy runtime is not implemented yet")
}
