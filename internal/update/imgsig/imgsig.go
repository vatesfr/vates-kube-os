package imgsig

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"strings"
)

// magic is the first line of a signature file. It names the format, so a file
// from another system, or from a later revision, is refused with a message that
// says which -- instead of being parsed as a signature that then fails
// obscurely.
const magic = "vates-image-signature/1"

// domain separates this signature from every other use of the same key. The
// signed message is a versioned, prefixed string rather than the bare digest, so
// a signature cannot be replayed as a signature over something else.
const domain = "vates-image-signature/1:sha256:"

// Signature is a parsed detached signature.
type Signature struct {
	// KeyID is the first 8 bytes of the public key, lowercase hex, or "" when
	// the file carries none. It selects a key among several; it is neither a
	// secret nor, on its own, a guarantee.
	KeyID string
	// SHA256 is the artifact's SHA-256, lowercase hex.
	SHA256 string
	// Raw is the 64-byte Ed25519 signature.
	Raw []byte
}

// PublicKey is a trusted Ed25519 public key and the id derived from it.
type PublicKey struct {
	KeyID string
	Key   ed25519.PublicKey
}

// NewPublicKey wraps a raw Ed25519 public key and derives its id.
func NewPublicKey(key ed25519.PublicKey) PublicKey {
	sum := sha256.Sum256(key)
	return PublicKey{Key: key, KeyID: hex.EncodeToString(sum[:8])}
}

// String renders a signature in the format ParseSignature reads back, so that a
// publisher writes Sign(...).String() and the node parses the same bytes.
func (s *Signature) String() string {
	var b strings.Builder
	fmt.Fprintln(&b, magic)
	if s.KeyID != "" {
		fmt.Fprintf(&b, "keyid %s\n", s.KeyID)
	}
	fmt.Fprintf(&b, "sha256 %s\n", s.SHA256)
	fmt.Fprintf(&b, "sig %s\n", base64.StdEncoding.EncodeToString(s.Raw))
	return b.String()
}

// ParseSignature reads the text format. Unknown lines are ignored, so a
// publisher may annotate a signature (a date, a version) without breaking the
// verifier; the header line must match exactly, which is what refuses a foreign
// format.
func ParseSignature(data []byte) (*Signature, error) {
	fields := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	header := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !header {
			if line != magic {
				return nil, fmt.Errorf("not a vates image signature: header is %q, want %q", line, magic)
			}
			header = true
			continue
		}
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("signature line %q is not \"key value\"", line)
		}
		fields[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !header {
		return nil, fmt.Errorf("empty signature: no %q header", magic)
	}

	s := &Signature{}

	sha, ok := fields["sha256"]
	if !ok {
		return nil, fmt.Errorf("signature has no sha256 line")
	}
	sha = strings.ToLower(sha)
	if len(sha) != sha256.Size*2 {
		return nil, fmt.Errorf("signature sha256 is %d characters, not %d", len(sha), sha256.Size*2)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return nil, fmt.Errorf("signature sha256 is not hexadecimal: %w", err)
	}
	s.SHA256 = sha

	raw, ok := fields["sig"]
	if !ok {
		return nil, fmt.Errorf("signature has no sig line")
	}
	sig, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("signature sig is not base64: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature sig is %d bytes, not %d", len(sig), ed25519.SignatureSize)
	}
	s.Raw = sig

	if id, ok := fields["keyid"]; ok {
		id = strings.ToLower(id)
		if len(id) != 16 {
			return nil, fmt.Errorf("signature keyid is %d characters, not 16", len(id))
		}
		if _, err := hex.DecodeString(id); err != nil {
			return nil, fmt.Errorf("signature keyid is not hexadecimal: %w", err)
		}
		s.KeyID = id
	}
	return s, nil
}

// ParsePublicKey reads a trusted key: PEM (a PKIX "PUBLIC KEY", what OpenSSL
// writes), or the bare 32 bytes in base64 or hex. PEM is the primary form; the
// others exist so that a hand-written or generated key does not need a
// conversion step.
func ParsePublicKey(data []byte) (PublicKey, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Contains(trimmed, []byte("-----BEGIN")) {
		return parsePEM(trimmed)
	}
	s := string(trimmed)
	// Hex first: a bare key is as likely to be pasted as hex as base64, and a
	// 64-character hex string is also valid base64. Trying hex first removes the
	// ambiguity; a hex string of the wrong length falls through to base64.
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return NewPublicKey(ed25519.PublicKey(b)), nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return newPublicKeyBytes(b)
	}
	return PublicKey{}, fmt.Errorf("public key is neither PEM, base64 nor hex")
}

func parsePEM(data []byte) (PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return PublicKey{}, fmt.Errorf("public key is not valid PEM")
	}
	if block.Type != "PUBLIC KEY" {
		return PublicKey{}, fmt.Errorf("PEM block is %q, not a public key", block.Type)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return PublicKey{}, fmt.Errorf("parse public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return PublicKey{}, fmt.Errorf("public key is %T, not Ed25519", parsed)
	}
	return NewPublicKey(key), nil
}

func newPublicKeyBytes(b []byte) (PublicKey, error) {
	if len(b) != ed25519.PublicKeySize {
		return PublicKey{}, fmt.Errorf("public key is %d bytes, not %d", len(b), ed25519.PublicKeySize)
	}
	return NewPublicKey(ed25519.PublicKey(b)), nil
}

// PEM renders a public key in the form ParsePublicKey reads back, so a
// key-generation step and the verifier cannot disagree on the encoding.
func (k PublicKey) PEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(k.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Digest returns the SHA-256 of a stream, lowercase hex. It reads the whole
// stream and does not hold it in memory.
func Digest(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// message is what is signed: a versioned, domain-separated string, so this
// signature is usable for nothing but an image digest.
func message(digest string) []byte {
	return []byte(domain + digest)
}

// Verify checks a stream against a detached signature and the trusted keys.
//
// It first computes the artifact's digest and refuses it if it does not match
// the one the signature names, then verifies the Ed25519 signature. With a keyid
// present, only the key bearing that id is tried; without one, every trusted key
// is. The two failures are kept apart on purpose: "the image does not match its
// digest" and "the image is not signed by any key we trust" are different
// incidents, and an operator should be told which one happened.
func Verify(r io.Reader, sig *Signature, keys []PublicKey) error {
	if sig == nil {
		return fmt.Errorf("no signature")
	}
	if len(keys) == 0 {
		return fmt.Errorf("no trusted public key")
	}
	got, err := Digest(r)
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}
	if got != sig.SHA256 {
		return fmt.Errorf("the image does not match its published digest (signature %s, image %s)", sig.SHA256, got)
	}
	candidates := keys
	if sig.KeyID != "" {
		candidates = nil
		for _, k := range keys {
			if k.KeyID == sig.KeyID {
				candidates = append(candidates, k)
			}
		}
		if len(candidates) == 0 {
			return fmt.Errorf("no trusted key has id %s", sig.KeyID)
		}
	}
	msg := message(sig.SHA256)
	for _, k := range candidates {
		if ed25519.Verify(k.Key, msg, sig.Raw) {
			return nil
		}
	}
	return fmt.Errorf("the image signature is invalid")
}

// Sign produces a detached signature over a stream. It is what a publisher runs
// -- the build, or the provider -- never the node, which only verifies. The key
// id is derived from the private key's public half.
func Sign(priv ed25519.PrivateKey, r io.Reader) (*Signature, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, not %d", len(priv), ed25519.PrivateKeySize)
	}
	got, err := Digest(r)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("private key has no Ed25519 public half")
	}
	return &Signature{
		KeyID:  NewPublicKey(pub).KeyID,
		SHA256: got,
		Raw:    ed25519.Sign(priv, message(got)),
	}, nil
}
