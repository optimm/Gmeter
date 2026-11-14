//go:build !windows

package runner

import (
	"errors"
	"syscall"
)

func isConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

func isConnReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
