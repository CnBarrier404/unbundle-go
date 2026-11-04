// Package gateway serves the bounded native Messages inference endpoint.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"unbundle-go/src/internal/cli"
	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

type Server struct {
	config config.Config
	runner *cli.Adapter
	token  [32]byte
	active chan struct{}
}

func New(ctx context.Context, c config.Config) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(c.TokenFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("gateway token must be a private regular file (Unix mode 0600 or stricter)")
	}
	f, err := os.Open(c.TokenFile)
	if err != nil {
		return nil, errors.New("could not read gateway token")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	_ = f.Close()
	if err != nil || len(data) > 4096 {
		return nil, errors.New("could not read bounded gateway token")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if len(token) < 32 || len(token) > 256 {
		return nil, errors.New("gateway token requires 32..256 visible ASCII characters")
	}
	for _, c := range token {
		if c < 0x21 || c > 0x7e {
			return nil, errors.New("gateway token requires visible ASCII without whitespace")
		}
	}
	runner := cli.Runner{Config: c}
	if err := runner.CheckVersion(ctx); err != nil {
		return nil, err
	}
	adapter, err := cli.NewAdapter(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Server{config: c, runner: adapter, token: sha256.Sum256([]byte(token)), active: make(chan struct{}, c.Runtime.MaxActiveRequests)}, nil
}

func Serve(ctx context.Context, c config.Config) error {
	handler, err := New(ctx, c)
	if err != nil {
		return err
	}
	defer handler.runner.Close()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return errors.New("could not bind configured loopback listener")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("gateway HTTP server failed")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		<-done
		return nil
	}
}

func (s *Server) authorized(h http.Header) bool {
	a, k := h.Values("Authorization"), h.Values("X-Api-Key")
	if len(a) > 1 || len(k) > 1 || len(a)+len(k) == 0 {
		return false
	}
	valid := 1
	if len(a) == 1 {
		if !strings.HasPrefix(a[0], "Bearer ") {
			return false
		}
		actual := sha256.Sum256([]byte(strings.TrimPrefix(a[0], "Bearer ")))
		valid &= subtle.ConstantTimeCompare(actual[:], s.token[:])
	}
	if len(k) == 1 {
		actual := sha256.Sum256([]byte(k[0]))
		valid &= subtle.ConstantTimeCompare(actual[:], s.token[:])
	}
	// The official CLI can send both headers from apiKeyHelper. Every supplied
	// credential must match; neither header overrides a conflicting credential.
	return valid == 1
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) error {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(s.config.Runtime.WriteTimeoutSeconds) * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.MarshalWrite(w, value)
}

func apiError(err error) *cli.Error {
	var failure *cli.Error
	if errors.As(err, &failure) {
		return failure
	}
	var request *messages.Error
	if errors.As(err, &request) {
		message := request.Message
		if request.Field != "" {
			message = request.Field + ": " + message
		}
		return &cli.Error{Code: request.Code, Type: "invalid_request_error", Message: message, Stage: "validation", Outcome: "not_started", Status: 400}
	}
	code, status := "INVALID_JSON", 400
	var tooLarge *http.MaxBytesError
	if errors.Is(err, jsonutil.ErrTooLarge) || errors.As(err, &tooLarge) {
		code, status = "REQUEST_TOO_LARGE", 413
	}
	return &cli.Error{Code: code, Type: "invalid_request_error", Message: "invalid, unsupported or oversized request", Stage: "validation", Outcome: "not_started", Status: status}
}

func (s *Server) fail(w http.ResponseWriter, err error, requestID string) {
	e := apiError(err)
	_ = s.writeJSON(w, e.Status, map[string]any{"type": "error", "error": e, "request_id": requestID})
}

func reject(code, message string, status int) *cli.Error {
	e := cli.Failure(code, "validation", message, status)
	e.Type, e.Outcome = "invalid_request_error", "not_started"
	return e
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := "req_" + rand.Text()
	w.Header().Set("request-id", id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.authorized(r.Header) {
		e := reject("GATEWAY_AUTH_FAILED", "gateway authentication failed", 401)
		e.Type = "authentication_error"
		s.fail(w, e, id)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == messages.StatusPath {
		_ = s.writeJSON(w, 200, map[string]any{"status": "ok", "stage": "native_compaction", "session_capacity": s.config.Runtime.MaxSessions, "busy": len(s.active) == cap(s.active), "http_active_requests": len(s.active), "sessions": s.runner.Status(), "cli_version": s.config.CLI.Version, "upstream_auth": "not_checked", "inference": "not_checked"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == messages.ModelsPath {
		data := make([]map[string]string, 0, len(s.config.Models))
		for _, m := range s.config.Models {
			data = append(data, map[string]string{"id": m.ID, "type": "model", "display_name": m.DisplayName})
		}
		_ = s.writeJSON(w, 200, map[string]any{"data": data, "has_more": false, "first_id": s.config.Models[0].ID, "last_id": s.config.Models[len(s.config.Models)-1].ID})
		return
	}
	if r.URL.Path != messages.MessagesPath {
		s.fail(w, reject("NOT_FOUND", "endpoint not found", 404), id)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.fail(w, reject("METHOD_NOT_ALLOWED", "requires POST", 405), id)
		return
	}
	if query := r.URL.RawQuery; query != "" && query != "beta=true" {
		s.fail(w, reject("UNSUPPORTED_QUERY", "unsupported query parameters", 400), id)
		return
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		s.fail(w, reject("UNSUPPORTED_MEDIA_TYPE", "requires application/json", 415), id)
		return
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		s.fail(w, reject("UNSUPPORTED_ENCODING", "compressed request bodies are not supported", 415), id)
		return
	}
	if err := messages.ValidateHeaders(r.Header); err != nil {
		s.fail(w, err, id)
		return
	}
	identity, err := session.IdentityFromHeaders(r.Header)
	if err != nil {
		s.fail(w, reject("INVALID_SESSION_IDENTITY", "invalid session identity hints", 400), id)
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		e := reject("GATEWAY_BUSY", "HTTP request capacity is occupied", 503)
		e.Type, e.Retryable = "overloaded_error", true
		s.fail(w, e, id)
		return
	}
	// Let the shared decoder observe the sentinel byte and classify oversized
	// bodies as 413, while retaining the HTTP reader's independent hard cap.
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxRequestBytes+1)
	defer r.Body.Close()
	request, err := messages.Decode(r.Body, s.config)
	if err != nil {
		s.fail(w, err, id)
		return
	}
	if err := cli.ValidateTurn(request); err != nil {
		s.fail(w, err, id)
		return
	}
	s.infer(w, r, request, identity, id)
}

func (s *Server) infer(w http.ResponseWriter, r *http.Request, request messages.Request, identity session.Identity, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.config.Runtime.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	if !request.Stream {
		response, err := s.runner.Run(ctx, request, identity, nil)
		if err != nil {
			s.fail(w, err, id)
			return
		}
		if evidence := response.WebSearch; evidence != nil {
			w.Header().Set("X-Unbundle-Web-Search-Calls", strconv.Itoa(evidence.Calls))
			if evidence.Requests != nil {
				w.Header().Set("X-Unbundle-Web-Search-Requests", strconv.FormatInt(*evidence.Requests, 10))
			}
			if evidence.Hits != nil {
				w.Header().Set("X-Unbundle-Web-Search-Hits", strconv.FormatInt(*evidence.Hits, 10))
			}
		}
		recoveryHeaders(w, response.Recovery)
		if response.Reused {
			w.Header().Set("X-Unbundle-Response-Reused", "true")
		}
		_ = s.writeJSON(w, 200, response)
		return
	}
	// Completion is known after streaming starts. Standard HTTP trailers report
	// persistence separately from the Anthropic message and its inference result.
	w.Header().Set("Trailer", "X-Unbundle-Recovery, X-Unbundle-Recovery-Error, X-Unbundle-Response-Reused")
	events := make(chan cli.Event) // no unbounded queue between CLI and socket
	type result struct {
		response messages.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := s.runner.Run(ctx, request, identity, func(event cli.Event) error {
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- result{response, err}
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	started := false
	send := func(kind string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(time.Duration(s.config.Runtime.WriteTimeoutSeconds) * time.Second)); err != nil {
			return err
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		if _, err := io.WriteString(w, "event: "+kind+"\ndata: "+string(data)+"\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := send("ping", map[string]string{"type": "ping"}); err != nil {
				return
			}
		case event := <-events:
			if err := send(event.Type, event.Data); err != nil {
				return
			}
		case result := <-done:
			finished = true
			if result.err != nil {
				if !started {
					s.fail(w, result.err, id)
				} else {
					_ = send("error", map[string]any{"type": "error", "error": apiError(result.err), "request_id": id})
				}
				return
			}
			recoveryHeaders(w, result.response.Recovery)
			if result.response.Reused {
				w.Header().Set("X-Unbundle-Response-Reused", "true")
			}
			if err := send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": result.response.StopReason, "stop_sequence": nil}, "usage": result.response.Usage}); err != nil {
				return
			}
			_ = send("message_stop", map[string]string{"type": "message_stop"})
			return
		}
	}
}

func recoveryHeaders(w http.ResponseWriter, recovery *messages.Recovery) {
	if recovery != nil {
		w.Header().Set("X-Unbundle-Recovery", recovery.State)
		if recovery.Code != "" {
			w.Header().Set("X-Unbundle-Recovery-Error", recovery.Code)
		}
	}
}
