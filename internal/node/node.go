package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/starfall-org/serverside-generative/internal/terminal"
)

const ProtocolVersion = 4
const TicketLifetime = 2 * time.Minute

type Config struct {
	TCPListen       string
	Advertise       string
	HTTPListen      string
	IPCSocket       string
	IdentityDir     string
	Password        string
	Human           bool
	HTTPOnly        bool
	AllowPublicHTTP bool
	EnableTerminal  bool
	TerminalCWD     string
}
type Invitation struct {
	Protocol    int    `json:"protocol"`
	Endpoint    string `json:"endpoint"`
	Fingerprint string `json:"fingerprint"`
	Ticket      string `json:"ticket,omitempty"`
	Expires     int64  `json:"expires"`
}
type HumanInvitation struct {
	Invitation
	PIN string `json:"pin"`
	URI string `json:"uri"`
}
type pairEntry struct {
	ID         string `json:"id"`
	SecretHash string `json:"secret_hash"`
}
type Node struct {
	Config              Config
	Certificate         tls.Certificate
	Fingerprint         string
	mu                  sync.Mutex
	tickets             map[[32]byte]time.Time
	pins                map[[32]byte]time.Time
	pairs               map[string]pairEntry
	attempts            map[string]*attempt
	sessions            map[string]*Session
	shared              syncedContext
	activeConnection    map[string]uint64
	nextConnectionEpoch uint64
	Terminals           *terminal.Manager
}
type attempt struct {
	Count int
	Until time.Time
}

func RandomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func randomPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%012d", n.Int64()), nil
}
func loadOrCreateIdentity(dir string) (tls.Certificate, string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return tls.Certificate{}, "", err
	}
	certfile := filepath.Join(dir, "identity.crt")
	keyfile := filepath.Join(dir, "identity.key")
	cert, err := tls.LoadX509KeyPair(certfile, keyfile)
	if errors.Is(err, os.ErrNotExist) {
		key, kerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		serial, kerr := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "serverside-generative"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(3, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
		der, kerr := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
		if kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		priv, kerr := x509.MarshalECPrivateKey(key)
		if kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		// Persist only a TLS identity. Never persist chat, event, API or MCP state.
		if kerr = atomicPrivateWrite(keyfile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv})); kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		if kerr = atomicPrivateWrite(certfile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); kerr != nil {
			return tls.Certificate{}, "", kerr
		}
		cert, err = tls.LoadX509KeyPair(certfile, keyfile)
	}
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, hex.EncodeToString(sum[:]), nil
}
func atomicPrivateWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func New(c Config) (*Node, error) {
	if c.TCPListen == "" {
		c.TCPListen = "127.0.0.1:7090"
	}
	if c.Advertise == "" {
		c.Advertise = c.TCPListen
	}
	if c.IdentityDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		c.IdentityDir = filepath.Join(base, "serverside-generative")
	}
	if c.IPCSocket == "" {
		c.IPCSocket = filepath.Join(c.IdentityDir, "control.sock")
	}
	cert, fp, err := loadOrCreateIdentity(c.IdentityDir)
	if err != nil {
		return nil, err
	}
	n := &Node{Config: c, Certificate: cert, Fingerprint: fp, tickets: make(map[[32]byte]time.Time), pins: make(map[[32]byte]time.Time), pairs: make(map[string]pairEntry), attempts: make(map[string]*attempt), sessions: make(map[string]*Session), activeConnection: make(map[string]uint64)}
	data, err := os.ReadFile(filepath.Join(c.IdentityDir, "clients.json"))
	if err == nil {
		var pairs []pairEntry
		if err = json.Unmarshal(data, &pairs); err != nil {
			return nil, fmt.Errorf("invalid clients store: %w", err)
		}
		for _, p := range pairs {
			n.pairs[p.ID] = p
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := n.loadSharedConfig(); err != nil {
		return nil, err
	}
	if c.EnableTerminal {
		mgr, err := terminal.NewManager(c.TerminalCWD)
		if err != nil {
			return nil, err
		}
		n.Terminals = mgr
	}
	return n, nil
}
func (n *Node) persistPairsLocked() error {
	pairs := make([]pairEntry, 0, len(n.pairs))
	for _, p := range n.pairs {
		pairs = append(pairs, p)
	}
	data, err := json.MarshalIndent(pairs, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivateWrite(filepath.Join(n.Config.IdentityDir, "clients.json"), data)
}
func (n *Node) IssueTicket() (Invitation, error) {
	token, err := RandomToken(32)
	if err != nil {
		return Invitation{}, err
	}
	expires := time.Now().Add(TicketLifetime)
	hash := sha256.Sum256([]byte(token))
	n.mu.Lock()
	n.tickets[hash] = expires
	n.mu.Unlock()
	return Invitation{Protocol: ProtocolVersion, Endpoint: n.Config.Advertise, Fingerprint: n.Fingerprint, Ticket: token, Expires: expires.Unix()}, nil
}
func (n *Node) IssueHuman() (HumanInvitation, error) {
	invite, err := n.IssueTicket()
	if err != nil {
		return HumanInvitation{}, err
	}
	pin, err := randomPIN()
	if err != nil {
		return HumanInvitation{}, err
	}
	h := sha256.Sum256([]byte(pin))
	n.mu.Lock()
	n.pins[h] = time.Now().Add(TicketLifetime)
	n.mu.Unlock()
	uri := "generative://pair?endpoint=" + urlQueryEscape(invite.Endpoint) + "&ticket=" + urlQueryEscape(invite.Ticket) + "&fingerprint=" + urlQueryEscape(invite.Fingerprint)
	return HumanInvitation{Invitation: invite, PIN: pin, URI: uri}, nil
}
func urlQueryEscape(value string) string {
	// Only unreserved characters and ':' appear in advertised addresses/tokens.
	r := strings.NewReplacer("%", "%25", "&", "%26", "?", "%3F", "#", "%23", "+", "%2B", " ", "%20")
	return r.Replace(value)
}
func (n *Node) limit(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	a := n.attempts[key]
	now := time.Now()
	if a == nil || now.After(a.Until) {
		n.attempts[key] = &attempt{Count: 1, Until: now.Add(5 * time.Minute)}
		return true
	}
	if a.Count >= 10 {
		return false
	}
	a.Count++
	return true
}
func (n *Node) blocked(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	a := n.attempts[key]
	if a == nil {
		return false
	}
	if time.Now().After(a.Until) {
		delete(n.attempts, key)
		return false
	}
	return a.Count >= 10
}
func (n *Node) checkPassword(password string) bool {
	if n.Config.Password == "" {
		return false
	}
	want := sha256.Sum256([]byte(n.Config.Password))
	got := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}
func (n *Node) pairWithTicket(ticket, pin string) (string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	matched := false
	if ticket != "" {
		key := sha256.Sum256([]byte(ticket))
		if expiry, ok := n.tickets[key]; ok && now.Before(expiry) {
			delete(n.tickets, key)
			matched = true
		}
	}
	if !matched && pin != "" {
		key := sha256.Sum256([]byte(pin))
		if expiry, ok := n.pins[key]; ok && now.Before(expiry) {
			delete(n.pins, key)
			matched = true
		}
	}
	if !matched {
		return "", "", errors.New("invalid or expired pairing credential")
	}
	id, err := RandomToken(16)
	if err != nil {
		return "", "", err
	}
	secret, err := RandomToken(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(secret))
	n.pairs[id] = pairEntry{ID: id, SecretHash: hex.EncodeToString(sum[:])}
	if err = n.persistPairsLocked(); err != nil {
		delete(n.pairs, id)
		return "", "", err
	}
	return id, secret, nil
}
func (n *Node) verifyPair(id, secret string) bool {
	n.mu.Lock()
	pair, ok := n.pairs[id]
	n.mu.Unlock()
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	want, _ := hex.DecodeString(pair.SecretHash)
	return len(want) == 32 && subtle.ConstantTimeCompare(want, sum[:]) == 1
}
func (n *Node) revokePair(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.pairs[id]; !ok {
		return errors.New("unknown client")
	}
	previous := n.pairs[id]
	delete(n.pairs, id)
	if err := n.persistPairsLocked(); err != nil {
		n.pairs[id] = previous
		return err
	}
	delete(n.activeConnection, id)
	for key, session := range n.sessions {
		if session.Owner != id {
			continue
		}
		delete(n.sessions, key)
		session.mu.Lock()
		for _, cancel := range session.Jobs {
			cancel()
		}
		session.mu.Unlock()
	}
	return nil
}
func (n *Node) Close() {
	if n.Terminals != nil {
		n.Terminals.CloseAll()
	}
}
