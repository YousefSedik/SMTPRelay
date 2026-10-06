// smtp-relay: accepts SMTP connections and forwards every message to an
// upstream SMTP server using username/password (AUTH PLAIN) authentication.
//
// Config via environment variables:
//
//	LISTEN_ADDR     address to listen on              (default ":2525")
//	UPSTREAM_HOST   upstream SMTP host                (required)
//	UPSTREAM_PORT   upstream SMTP port                (default "587"; 465 = implicit TLS)
//	UPSTREAM_USER   upstream username                 (required)
//	UPSTREAM_PASS   upstream password                 (required)
//	FROM_OVERRIDE   if set, replaces the envelope sender (some providers require this)
//	LOCAL_USER      if set (with LOCAL_PASS), clients must authenticate to this relay
//	LOCAL_PASS      password for local clients
package main

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	netsmtp "net/smtp"
	"os"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/joho/godotenv"

)

type config struct {
	listenAddr   string
	upstreamHost string
	upstreamPort string
	upstreamUser string
	upstreamPass string
	fromOverride string
	localUser    string
	localPass    string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	c := config{
		listenAddr:   env("LISTEN_ADDR", ":2525"),
		upstreamHost: os.Getenv("UPSTREAM_HOST"),
		upstreamPort: env("UPSTREAM_PORT", "587"),
		upstreamUser: os.Getenv("UPSTREAM_USER"),
		upstreamPass: os.Getenv("UPSTREAM_PASS"),
		fromOverride: os.Getenv("FROM_OVERRIDE"),
		localUser:    os.Getenv("LOCAL_USER"),
		localPass:    os.Getenv("LOCAL_PASS"),
	}
	if c.upstreamHost == "" || c.upstreamUser == "" || c.upstreamPass == "" {
		log.Fatal("UPSTREAM_HOST, UPSTREAM_USER and UPSTREAM_PASS are required")
	}
	return c
}

// ---- upstream delivery ----------------------------------------------------

func (c config) forward(from string, to []string, data []byte) error {
	addr := net.JoinHostPort(c.upstreamHost, c.upstreamPort)
	tlsCfg := &tls.Config{ServerName: c.upstreamHost, MinVersion: tls.VersionTLS12}

	var client *netsmtp.Client
	if c.upstreamPort == "465" {
		// Implicit TLS (SMTPS)
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			return err
		}
		client, err = netsmtp.NewClient(conn, c.upstreamHost)
		if err != nil {
			conn.Close()
			return err
		}
	} else {
		conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			return err
		}
		client, err = netsmtp.NewClient(conn, c.upstreamHost)
		if err != nil {
			conn.Close()
			return err
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				client.Close()
				return err
			}
		} else {
			client.Close()
			return errors.New("upstream does not support STARTTLS; refusing to send credentials in clear text")
		}
	}
	defer client.Close()

	auth := netsmtp.PlainAuth("", c.upstreamUser, c.upstreamPass, c.upstreamHost)
	if err := client.Auth(auth); err != nil {
		return err
	}

	if c.fromOverride != "" {
		from = c.fromOverride
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := client.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// ---- local SMTP server ----------------------------------------------------

type backend struct{ cfg config }

func (b *backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &session{cfg: b.cfg, authed: b.cfg.localUser == ""}, nil
}

type session struct {
	cfg    config
	authed bool
	from   string
	to     []string
}

// AuthSession: optional authentication for clients connecting to the relay.
func (s *session) AuthMechanisms() []string {
	if s.cfg.localUser == "" {
		return nil
	}
	return []string{sasl.Plain}
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username != s.cfg.localUser || password != s.cfg.localPass {
			return errors.New("invalid credentials")
		}
		s.authed = true
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := s.cfg.forward(s.from, s.to, data); err != nil {
		log.Printf("forward failed (from=%s to=%v): %v", s.from, s.to, err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 4, 0},
			Message:      "upstream delivery failed, try again later",
		}
	}
	log.Printf("relayed message from=%s to=%v bytes=%d", s.from, s.to, len(data))
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error { return nil }

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	
	cfg := loadConfig()

	srv := smtp.NewServer(&backend{cfg: cfg})
	srv.Addr = cfg.listenAddr
	srv.Domain = "localhost"
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	srv.MaxMessageBytes = 25 * 1024 * 1024
	srv.MaxRecipients = 100
	// Local listener is plain text. Only enable this on localhost / trusted networks.
	srv.AllowInsecureAuth = true

	log.Printf("SMTP relay listening on %s -> %s:%s", cfg.listenAddr, cfg.upstreamHost, cfg.upstreamPort)
	log.Fatal(srv.ListenAndServe())
}