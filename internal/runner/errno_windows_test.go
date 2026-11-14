package runner

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// Windows reports a refused or reset connection as a Winsock error number, not
// the errno that package syscall names ECONNREFUSED and ECONNRESET there, so a
// plain errors.Is against those two never matches.
func TestWinsockRefusalsAreClassifiedLikeUnixOnes(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  string
	}{
		{"connection refused (WSAECONNREFUSED)", 10061, "connection_refused"},
		{"connection reset (WSAECONNRESET)", 10054, "connection_reset"},
	}
	for _, c := range cases {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connectex", c.errno)}
		if got := classify(err); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}
