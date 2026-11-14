package runner

import (
	"errors"
	"syscall"
)

// Winsock error numbers, which is what Windows reports for these two failures.
const (
	wsaECONNRESET   syscall.Errno = 10054
	wsaECONNREFUSED syscall.Errno = 10061
)

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaECONNREFUSED)
}

func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, wsaECONNRESET)
}
