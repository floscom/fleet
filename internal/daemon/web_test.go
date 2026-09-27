package daemon

import (
	"errors"
	"net"
	"net/http"
	"testing"

	"fleet/internal/web"
)

func TestLanPort(t *testing.T) {
	if lanPort(nil) != 0 {
		t.Fatal("nil listener advertised")
	}
	for addr, lan := range map[string]bool{"127.0.0.1:0": false, "[::1]:0": false, "0.0.0.0:0": true} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("%s: %v", addr, err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if got := lanPort(ln); (got == port) != lan || (!lan && got != 0) {
			t.Errorf("lanPort(%s) = %d, port %d, want lan=%v", addr, got, port, lan)
		}
		ln.Close()
	}
}

func TestWebError(t *testing.T) {
	for err, status := range map[error]int{
		errf(codeNotFound, "no root"): http.StatusNotFound,
		errf(codeInvalid, "bad"):      http.StatusBadRequest,
		errf(codeDenied, "no"):        http.StatusForbidden,
	} {
		var we *web.Error
		if !errors.As(webError(err), &we) || we.Status != status || we.Msg != err.Error() {
			t.Errorf("webError(%v) = %v, want status %d", err, webError(err), status)
		}
	}
	internal := errors.New("disk full")
	if webError(internal) != internal || webError(nil) != nil {
		t.Fatal("internal errors must stay internal")
	}
}
