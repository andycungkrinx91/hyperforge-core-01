package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hyperforge/internal/forge"
)

// rawHTTPDo sends a tiny HTTP/1.1 request over plain TCP (avoids pulling net/http + crypto/tls into the binary).
func rawHTTPDo(method, rawURL, contentType string, body []byte) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", method, path, u.Host)
	if contentType != "" {
		fmt.Fprintf(&req, "Content-Type: %s\r\n", contentType)
	}
	if len(body) > 0 {
		fmt.Fprintf(&req, "Content-Length: %d\r\n", len(body))
	}
	req.WriteString("\r\n")
	if len(body) > 0 {
		req.Write(body)
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return 0, nil, err
	}
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		return 0, nil, fmt.Errorf("malformed HTTP response")
	}
	statusLineEnd := bytes.Index(raw[:sep], []byte("\r\n"))
	statusLine := raw[:sep]
	if statusLineEnd >= 0 {
		statusLine = raw[:statusLineEnd]
	}
	parts := bytes.SplitN(statusLine, []byte(" "), 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("malformed HTTP status line")
	}
	code, err := strconv.Atoi(string(parts[1]))
	if err != nil {
		return 0, nil, err
	}
	return code, raw[sep+4:], nil
}

func registerWithEnv(envURL string, port int) error {
	payload := []byte(fmt.Sprintf(`{"port":%d}`, port))
	delay := 50 * time.Millisecond
	for {
		code, body, err := rawHTTPDo("POST", envURL+"/register", "application/json", payload)
		if err == nil {
			if code == 200 || code == 409 {
				return nil
			}
			if code == 400 || code == 405 {
				return fmt.Errorf("register failed with HTTP %d: %s", code, string(body))
			}
		}
		time.Sleep(delay)
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func waitForDone(envURL string) {
	delay := 300 * time.Millisecond
	for {
		code, rawBody, err := rawHTTPDo("GET", envURL+"/done", "", nil)
		if err == nil && code == 200 {
			// Handle chunked or plain JSON body cleanly by locating '{'..'}'.
			if start := bytes.IndexByte(rawBody, '{'); start >= 0 {
				if end := bytes.LastIndexByte(rawBody, '}'); end > start {
					var body struct {
						Done bool `json:"done"`
					}
					if json.Unmarshal(rawBody[start:end+1], &body) == nil && body.Done {
						return
					}
				}
			}
			delay = 500 * time.Millisecond
		}
		time.Sleep(delay)
		if delay < 1500*time.Millisecond {
			delay += 250 * time.Millisecond
		}
	}
}

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(768 << 20)

	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	trialNow := os.Getenv("MAKEFUN_TRIAL_NOW")
	if trialNow == "" {
		trialNow = "2026-11-14T00:00:00Z"
	}
	bindPort := 0
	if p := os.Getenv("MAKEFUN_FORGE_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v >= 0 && v <= 65535 {
			bindPort = v
		}
	}

	store := forge.NewStore(login, token, trialNow)
	srv, err := forge.NewEpollServer(store, bindPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen failed: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	go func() {
		if err := srv.Serve(); err != nil {
			fmt.Fprintf(os.Stderr, "[forge] serve error: %v\n", err)
			os.Exit(1)
		}
	}()

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintf(os.Stdout, "PORT=%d\n", srv.Port())
		select {}
	}

	fmt.Fprintf(os.Stdout, "[entry] forge listening on port %d\n", srv.Port())
	if err := registerWithEnv(envURL, srv.Port()); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for done")
	waitForDone(envURL)
	srv.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}
