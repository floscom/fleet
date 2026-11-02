package hub

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"fleet/internal/team"
)

// DefaultListen is where the hub listens without --autocert: loopback, for
// a reverse proxy in front of it.
const DefaultListen = "127.0.0.1:7430"

// operatorFile is the file `fleet hub code` reads: the operator token and
// the loopback address of the operator endpoint. Only the user running the
// hub can read it (0600).
type operatorFile struct {
	Token string `json:"token"`
	Addr  string `json:"addr"`
}

// ServeOptions configure Serve.
type ServeOptions struct {
	Options
	// Listen is the plain HTTP address (DefaultListen when ""), used
	// unless Autocert is set.
	Listen string
	// Autocert is a domain to get a Let's Encrypt certificate for; the hub
	// then serves HTTPS on :443 and the ACME challenge (and a redirect) on
	// :80.
	Autocert string
}

// Serve runs the hub until ctx is done. It always opens a loopback
// listener for the operator endpoint and writes operator.json.
func Serve(ctx context.Context, opts ServeOptions) error {
	s, err := New(opts.Options)
	if err != nil {
		return err
	}
	opLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer opLn.Close()
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	op := operatorFile{Token: hex.EncodeToString(token), Addr: opLn.Addr().String()}
	opJSON, _ := json.Marshal(op)
	if err := writeFileAtomic(filepath.Join(opts.DataDir, "operator.json"), opJSON); err != nil {
		return fmt.Errorf("write operator file: %w", err)
	}
	defer os.Remove(filepath.Join(opts.DataDir, "operator.json"))

	opSrv := &http.Server{Handler: s.operatorHandler(op.Token), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = opSrv.Serve(opLn) }()
	defer opSrv.Close()

	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// Bodies are small but connections may be slow; the events long
		// poll holds a request for pollWait.
		ReadTimeout:  2 * time.Minute,
		WriteTimeout: pollWait + 30*time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	errc := make(chan error, 2)
	var redirect *http.Server
	if opts.Autocert != "" {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(opts.Autocert),
			Cache:      autocert.DirCache(filepath.Join(opts.DataDir, "autocert")),
		}
		srv.Addr = ":443"
		srv.TLSConfig = m.TLSConfig()
		redirect = &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- redirect.ListenAndServe() }()
		go func() { errc <- srv.ListenAndServeTLS("", "") }()
		s.log.Info("hub listening", "https", opts.Autocert, "data", opts.DataDir, "open", opts.Open)
	} else {
		srv.Addr = opts.Listen
		if srv.Addr == "" {
			srv.Addr = DefaultListen
		}
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		go func() { errc <- srv.Serve(ln) }()
		s.log.Info("hub listening", "addr", ln.Addr().String(), "data", opts.DataDir, "open", opts.Open)
	}
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if redirect != nil {
		_ = redirect.Shutdown(shutdownCtx)
	}
	return srv.Shutdown(shutdownCtx)
}

// operatorHandler serves the loopback operator endpoint:
// POST /v1/admin/codes with "Authorization: Bearer <token>".
func (s *Server) operatorHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/codes", func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "bad operator token")
			return
		}
		code, exp := s.codes.issue()
		writeJSON(w, http.StatusOK, team.CodeReply{Code: code, Expires: exp})
	})
	return mux
}

// RequestCode asks the hub running on dataDir for a one-time team creation
// code (`fleet hub code`).
func RequestCode(ctx context.Context, dataDir string) (team.CodeReply, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "operator.json"))
	if errors.Is(err, os.ErrNotExist) {
		return team.CodeReply{}, fmt.Errorf("no hub is running on %s (no operator.json)", dataDir)
	}
	if err != nil {
		return team.CodeReply{}, err
	}
	var op operatorFile
	if err := json.Unmarshal(data, &op); err != nil || op.Token == "" || op.Addr == "" {
		return team.CodeReply{}, errors.New("operator.json is malformed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+op.Addr+"/v1/admin/codes", nil)
	if err != nil {
		return team.CodeReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+op.Token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return team.CodeReply{}, fmt.Errorf("the hub on %s is not answering: %w", dataDir, err)
	}
	defer resp.Body.Close()
	var out team.CodeReply
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("hub: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
