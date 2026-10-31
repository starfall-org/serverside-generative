package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/starfall-org/serverside-generative/internal/node"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, "serverside: memory-only session runtime")
	fmt.Fprintln(os.Stderr, "Commands: serve, handshake --stdio, pair, version")
}
func defaultIPC() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "serverside-generative", "control.sock")
}
func printHuman(h node.HumanInvitation) {
	fmt.Println("\n=== Human Relay (2-minute one-time pairing) ===")
	fmt.Println("Endpoint:       ", h.Endpoint)
	fmt.Println("Manual PIN:     ", h.PIN)
	fmt.Println("TLS SHA-256:    ", h.Fingerprint)
	fmt.Println("Invite URI:     ", h.URI)
	qrterminal.Generate(h.URI, qrterminal.L, os.Stdout)
	fmt.Println("Note: This does not forward traffic or bypass NAT.")
}
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "version":
		fmt.Println("serverside", version)
		return nil
	case "handshake", "ticket", "pair":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		ipc := fs.String("ipc", defaultIPC(), "daemon Unix socket")
		stdio := fs.Bool("stdio", false, "write one JSON invitation to stdout (for ssh remote command)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		method := "ticket"
		if args[0] == "pair" {
			method = "human"
		}
		if args[0] == "handshake" && !*stdio {
			return fmt.Errorf("use handshake --stdio over SSH")
		}
		raw, err := node.ControlRequest(*ipc, method)
		if err != nil {
			return err
		}
		if method == "human" {
			var h node.HumanInvitation
			if err = json.Unmarshal(raw, &h); err != nil {
				return err
			}
			printHuman(h)
		} else {
			fmt.Println(string(raw))
		}
		return nil
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		tcpAddr := fs.String("tcp-listen", "127.0.0.1:7090", "TLS/TCP data listener")
		advertise := fs.String("advertise", "", "TCP endpoint advertised to clients (host:port)")
		httpAddr := fs.String("http-listen", "127.0.0.1:7091", "HTTP/WS handshake listener; 'off' disables")
		password := fs.String("password", os.Getenv("GENERATIVE_HANDSHAKE_PASSWORD"), "handshake password (prefer environment variable)")
		identityDir := fs.String("identity-dir", "", "connection identity and paired client credential hashes only")
		ipc := fs.String("ipc", "", "control socket for SSH handshake and human pairing")
		human := fs.Bool("human", false, "display Human Relay QR and PIN on startup")
		onlyHTTP := fs.Bool("http-only", false, "disable WS to test HTTP fallback")
		allowPublic := fs.Bool("allow-public-http", false, "dangerous: allow plain HTTP binding beyond loopback")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := node.SafeHTTPBind(*httpAddr, *allowPublic); err != nil {
			return err
		}
		if *password != "" && len(*password) < 12 {
			return fmt.Errorf("HTTP/WS password must be at least 12 characters")
		}
		listener, err := net.Listen("tcp", *tcpAddr)
		if err != nil {
			return err
		}
		defer listener.Close()
		ep := *advertise
		if ep == "" {
			ep = listener.Addr().String()
		}
		host, _, err := net.SplitHostPort(ep)
		if err != nil {
			return err
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			return fmt.Errorf("set --advertise to a reachable address instead of %q", ep)
		}
		c := node.Config{TCPListen: listener.Addr().String(), Advertise: ep, HTTPListen: *httpAddr, IPCSocket: *ipc, IdentityDir: *identityDir, Password: *password, Human: *human, HTTPOnly: *onlyHTTP, AllowPublicHTTP: *allowPublic}
		n, err := node.New(c)
		if err != nil {
			return err
		}
		sock := n.Config.IPCSocket
		control, err := node.StartIPCSocket(sock)
		if err != nil {
			return err
		}
		defer func() { _ = control.Close(); _ = os.Remove(sock) }()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go n.ServeIPC(ctx, control)
		go n.ServeTCP(ctx, listener)
		var httpSrv *http.Server
		if *password != "" && !strings.EqualFold(*httpAddr, "off") {
			httpListener, err := net.Listen("tcp", *httpAddr)
			if err != nil {
				return err
			}
			httpSrv = &http.Server{Handler: n.HTTPHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
			go func() {
				if err := httpSrv.Serve(httpListener); err != nil && err != http.ErrServerClosed {
					log.Print("HTTP error: ", err)
				}
			}()
			fmt.Println("HTTP/WS handshake:", httpListener.Addr().String())
		} else {
			fmt.Println("HTTP/WS handshake: disabled (set GENERATIVE_HANDSHAKE_PASSWORD)")
		}
		fmt.Println("Data transport (TLS 1.3):", listener.Addr().String())
		fmt.Println("Advertised endpoint:     ", ep)
		fmt.Println("Server TLS fingerprint:  ", n.Fingerprint)
		fmt.Println("SSH handshake command:    ssh -T USER@HOST serverside handshake --stdio")
		fmt.Println("Pairing control socket:  ", sock)
		fmt.Println("All chat/session/API/MCP contents remain in RAM.")
		if *human {
			invite, err := n.IssueHuman()
			if err != nil {
				return err
			}
			printHuman(invite)
		}
		<-ctx.Done()
		if httpSrv != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdown)
		}
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}