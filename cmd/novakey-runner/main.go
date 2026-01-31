package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"novakey/cmd/novakey-runner/internal/proto"
	"novakey/cmd/novakey-runner/internal/registry"
	"novakey/cmd/novakey-runner/internal/runner"
)

type config struct {
	Listen        string
	Transport     string // unix|tcp
	RegistryPath  string
	MaxFrameBytes int
}

func main() {
	var c config
	flag.StringVar(&c.Transport, "transport", defaultTransport(), "ipc transport: unix|tcp")
	flag.StringVar(&c.Listen, "listen", defaultListenAddr(), "listen address (unix path or host:port)")
	flag.StringVar(&c.RegistryPath, "registry", defaultRegistryPath(), "action registry path (.yaml/.json)")
	flag.IntVar(&c.MaxFrameBytes, "max-frame", 1<<20, "max request/response frame bytes")
	flag.Parse()

	reg, err := registry.Load(c.RegistryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load registry: %v\n", err)
		os.Exit(2)
	}
	r := runner.New(reg)

	ln, err := listen(c.Transport, c.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(2)
	}
	defer ln.Close()

	fmt.Printf("novakey-runner listening (%s) at %s\n", c.Transport, c.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// transient
			continue
		}
		go handleConn(ctx, conn, r, c.MaxFrameBytes)
	}
}

func handleConn(ctx context.Context, conn net.Conn, r *runner.Runner, maxFrame int) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	reqBytes, err := readFrame(br, maxFrame)
	if err != nil {
		_ = writeFrame(bw, maxFrame, mustJSON(proto.ExecResponse{
			V: 1, OK: false, Error: "bad request",
		}))
		_ = bw.Flush()
		return
	}

	var req proto.ExecRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil || req.V != 1 || req.Action == "" || req.Req == "" {
		_ = writeFrame(bw, maxFrame, mustJSON(proto.ExecResponse{
			V: 1, OK: false, Error: "bad request",
		}))
		_ = bw.Flush()
		return
	}

	res, _ := r.Execute(ctx, req.Action, req.Params)
	out := proto.ExecResponse{
		V:          1,
		Req:        req.Req,
		OK:         res.OK,
		Error:      res.Err,
		ExitCode:   res.ExitCode,
		DurationMS: res.Duration.Milliseconds(),
		StdoutB64:  res.StdoutB64,
		StderrB64:  res.StderrB64,
	}
	_ = writeFrame(bw, maxFrame, mustJSON(out))
	_ = bw.Flush()
}

func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	if n == 0 || int(n) > max {
		return nil, fmt.Errorf("frame too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeFrame(w *bufio.Writer, max int, payload []byte) error {
	if len(payload) == 0 || len(payload) > max {
		return fmt.Errorf("frame too large")
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(payload))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func defaultTransport() string {
	if runtime.GOOS == "windows" {
		return "tcp"
	}
	return "unix"
}

func defaultListenAddr() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.1:60769"
	}
	return "/run/novakey/runner.sock"
}

func defaultRegistryPath() string {
	if runtime.GOOS == "windows" {
		return filepathJoin(os.Getenv("APPDATA"), "NovaKey", "actions.yaml")
	}
	return "/etc/novakey/actions.yaml"
}

func filepathJoin(parts ...string) string {
	// tiny join without importing path/filepath everywhere in this file
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out == "" {
			out = p
			continue
		}
		sep := string(os.PathSeparator)
		if strings.HasSuffix(out, sep) {
			out += p
		} else {
			out += sep + p
		}
	}
	return out
}

func listen(transport, addr string) (net.Listener, error) {
	switch strings.ToLower(transport) {
	case "tcp":
		// WARNING: ensure addr is loopback in production configs.
		return net.Listen("tcp", addr)
	case "unix":
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("unix sockets not supported on windows")
		}
		_ = os.Remove(addr)
		// Ensure parent dir exists
		if dir := strings.TrimSpace(addr); dir != "" {
			if i := strings.LastIndex(dir, "/"); i > 0 {
				_ = os.MkdirAll(dir[:i], 0o755)
			}
		}
		ln, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		// Restrictive perms; systemd/launchd should set owner/group as needed.
		_ = os.Chmod(addr, 0o660)
		return ln, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", transport)
	}
}
