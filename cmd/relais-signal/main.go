// relais-signal is a bounded authenticated HTTPS frontend for test callers.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	Listen, Control, AdvertiseIPs string
	TokenEnv, TokenFile           string
	CertFile, KeyFile, CertOut    string
}

func parseConfig(args []string) (config, error) {
	var c config
	flags := flag.NewFlagSet("relais-signal", flag.ContinueOnError)
	flags.StringVar(&c.Listen, "listen", "", "explicit private or loopback IP:port for HTTPS (for example 127.0.0.1:9443)")
	flags.StringVar(&c.Control, "control", "", "private control-plane HTTP(S) origin with a literal IP")
	flags.StringVar(&c.AdvertiseIPs, "advertise-ips", "", "comma-separated advertised IP SANs; required for generated certificates")
	flags.StringVar(&c.TokenEnv, "token-env", "RELAIS_SIGNAL_TOKEN", "environment variable containing the bearer token")
	flags.StringVar(&c.TokenFile, "token-file", "", "read bearer token from this file instead of the environment")
	flags.StringVar(&c.CertFile, "cert", "", "existing PEM certificate chain; requires -key")
	flags.StringVar(&c.KeyFile, "key", "", "existing PEM private key; requires -cert")
	flags.StringVar(&c.CertOut, "cert-out", "", "write the generated public certificate PEM here for caller trust")
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	if flags.NArg() != 0 {
		return c, errors.New("positional arguments are refused")
	}
	return c, nil
}

func privateIP(ip netip.Addr) bool {
	return ip.IsValid() && (ip.IsPrivate() || ip.IsLoopback()) && ip.Zone() == ""
}
func (c config) validate() (*url.URL, error) {
	bind, err := netip.ParseAddrPort(c.Listen)
	if err != nil || !privateIP(bind.Addr()) || bind.Port() == 0 {
		return nil, errors.New("-listen requires a specific private or loopback IP and nonzero port")
	}
	u, err := url.Parse(c.Control)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("-control requires a private HTTP(S) origin without credentials, path, query or fragment")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !privateIP(ip) {
		return nil, errors.New("-control must use a literal private or loopback IP")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid control-plane port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid control-plane port")
	}
	u.Path = ""
	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, errors.New("-cert and -key must be supplied together")
	}
	if c.CertFile != "" && c.CertOut != "" {
		return nil, errors.New("-cert-out is only for generated certificates")
	}
	return u, nil
}

func loadToken(c config) (string, error) {
	value := os.Getenv(c.TokenEnv)
	if c.TokenFile != "" {
		file, err := os.Open(c.TokenFile)
		if err != nil {
			return "", errors.New("cannot open token file")
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		if err != nil || len(data) > 16<<10 {
			return "", errors.New("cannot read token file or token file too large")
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if len(value) < 32 || len(value) > 16<<10 {
		return "", errors.New("bearer token must contain 32 to 16384 characters from the environment or a file")
	}
	for _, char := range value {
		if !isTokenChar(char) {
			return "", errors.New("bearer token contains invalid characters")
		}
	}
	return value, nil
}

func newServer(handler http.Handler, cert tls.Certificate) *http.Server {
	return &http.Server{
		Handler:                      handler,
		DisableGeneralOptionsHandler: true,
		TLSConfig:                    &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  15 * time.Second,
		WriteTimeout:                 15 * time.Second,
		IdleTimeout:                  30 * time.Second,
		MaxHeaderBytes:               32 << 10,
	}
}

func run(ctx context.Context, c config, output io.Writer) error {
	target, err := c.validate()
	if err != nil {
		return err
	}
	token, err := loadToken(c)
	if err != nil {
		return err
	}
	cert, pemData, fingerprint, err := certificate(c)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if c.CertOut != "" {
		// Never silently replace a prior run's trust root.
		file, err := os.OpenFile(c.CertOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return fmt.Errorf("write public certificate: %w", err)
		}
		_, writeErr := file.Write(pemData)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}
	transport := upstreamTransport()
	defer transport.CloseIdleConnections()
	handler, err := newHandler(target, token, transport)
	if err != nil {
		return err
	}
	server := newServer(handler, cert)
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, "", "") }()
	if _, err := fmt.Fprintf(output, "https=%s certificate_sha256=%s\n", listener.Addr(), fingerprint); err != nil {
		_ = server.Close()
		<-done
		return err
	}
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		<-done
		return err
	}
}

func main() {
	c, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = run(ctx, c, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relais-signal:", err)
		os.Exit(1)
	}
}

func isTokenChar(char rune) bool {
	return isIDChar(char) || strings.ContainsRune(".~+/=", char)
}
