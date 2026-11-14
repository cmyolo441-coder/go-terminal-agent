package supplychain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// In-toto / SLSA constants.
const (
	InTotoStatementType   = "https://in-toto.io/Statement/v1"
	SLSAProvenanceType    = "https://slsa.dev/provenance/v1"
	InTotoPayloadType     = "application/vnd.in-toto+json"
	DefaultBuildType      = "https://github.com/cmyolo441-coder/go-terminal-agent/buildtypes/gta-build/v1"
	maxEnvelopePayloadLen = 32 << 20
)

// ResourceDescriptor is an in-toto v1 resource descriptor.
type ResourceDescriptor struct {
	Name             string            `json:"name,omitempty"`
	URI              string            `json:"uri,omitempty"`
	Digest           map[string]string `json:"digest,omitempty"`
	MediaType        string            `json:"mediaType,omitempty"`
	Annotations      map[string]any    `json:"annotations,omitempty"`
	DownloadLocation string            `json:"downloadLocation,omitempty"`
}

// Statement is an in-toto v1 Statement carrying SLSA provenance.
type Statement struct {
	Type          string               `json:"_type"`
	Subject       []ResourceDescriptor `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     Provenance           `json:"predicate"`
}

// Provenance is the SLSA v1 provenance predicate.
type Provenance struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

// BuildDefinition describes the build inputs.
type BuildDefinition struct {
	BuildType            string               `json:"buildType"`
	ExternalParameters   map[string]any       `json:"externalParameters"`
	InternalParameters   map[string]any       `json:"internalParameters,omitempty"`
	ResolvedDependencies []ResourceDescriptor `json:"resolvedDependencies,omitempty"`
}

// RunDetails describes the build execution.
type RunDetails struct {
	Builder    Builder              `json:"builder"`
	Metadata   *BuildMetadata       `json:"metadata,omitempty"`
	Byproducts []ResourceDescriptor `json:"byproducts,omitempty"`
}

// Builder identifies the build platform.
type Builder struct {
	ID                  string               `json:"id"`
	Version             map[string]string    `json:"version,omitempty"`
	BuilderDependencies []ResourceDescriptor `json:"builderDependencies,omitempty"`
}

// BuildMetadata holds invocation metadata.
type BuildMetadata struct {
	InvocationID string     `json:"invocationId,omitempty"`
	StartedOn    *time.Time `json:"startedOn,omitempty"`
	FinishedOn   *time.Time `json:"finishedOn,omitempty"`
}

// ProvenanceInput is the information needed to describe a build.
type ProvenanceInput struct {
	Subjects           []ResourceDescriptor
	BuildType          string
	BuilderID          string
	BuilderVersion     map[string]string
	ExternalParameters map[string]any
	InternalParameters map[string]any
	// Dependencies are resolved inputs; when Graph is set its pinned
	// packages are appended as purl descriptors with their hashes.
	Dependencies []ResourceDescriptor
	Graph        *Graph
	InvocationID string
	StartedOn    time.Time
	FinishedOn   time.Time
}

// SubjectsFromFiles hashes files (SHA-256 and SHA-512) into subjects named
// by their path relative to base (or base name when base is empty).
func SubjectsFromFiles(base string, paths ...string) ([]ResourceDescriptor, error) {
	var out []ResourceDescriptor
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		h256, h512 := sha256.New(), sha512.New()
		_, err = io.Copy(io.MultiWriter(h256, h512), f)
		f.Close()
		if err != nil {
			return nil, err
		}
		name := filepath.Base(p)
		if base != "" {
			if rel, err := filepath.Rel(base, p); err == nil {
				name = filepath.ToSlash(rel)
			}
		}
		out = append(out, ResourceDescriptor{Name: name, Digest: map[string]string{
			"sha256": hex.EncodeToString(h256.Sum(nil)),
			"sha512": hex.EncodeToString(h512.Sum(nil)),
		}})
	}
	return out, nil
}

// NewProvenance builds an in-toto Statement with a SLSA v1 predicate.
func NewProvenance(in ProvenanceInput) (*Statement, error) {
	if len(in.Subjects) == 0 {
		return nil, errors.New("slsa: at least one subject is required")
	}
	for _, s := range in.Subjects {
		if s.Name == "" || len(s.Digest) == 0 {
			return nil, fmt.Errorf("slsa: subject %q needs a name and digest", s.Name)
		}
		for alg, v := range s.Digest {
			if _, err := hex.DecodeString(v); err != nil || v == "" {
				return nil, fmt.Errorf("slsa: subject %q: %s digest is not hex", s.Name, alg)
			}
		}
	}
	if in.BuilderID == "" {
		return nil, errors.New("slsa: builder id is required")
	}
	bt := in.BuildType
	if bt == "" {
		bt = DefaultBuildType
	}
	ext := in.ExternalParameters
	if ext == nil {
		ext = map[string]any{}
	}
	deps := append([]ResourceDescriptor(nil), in.Dependencies...)
	if in.Graph != nil {
		for _, p := range in.Graph.Packages() {
			if p.Local || p.Unpinned || p.Version == "" {
				continue
			}
			rd := ResourceDescriptor{URI: p.PURL}
			for _, h := range p.Hashes {
				if rd.Digest == nil {
					rd.Digest = map[string]string{}
				}
				rd.Digest[map[HashAlg]string{HashSHA1: "sha1", HashSHA256: "sha256", HashSHA384: "sha384",
					HashSHA512: "sha512", HashMD5: "md5"}[h.Alg]] = h.Hex
			}
			deps = append(deps, rd)
		}
	}
	subjects := append([]ResourceDescriptor(nil), in.Subjects...)
	sort.SliceStable(subjects, func(i, j int) bool { return subjects[i].Name < subjects[j].Name })
	st := &Statement{Type: InTotoStatementType, Subject: subjects, PredicateType: SLSAProvenanceType,
		Predicate: Provenance{
			BuildDefinition: BuildDefinition{BuildType: bt, ExternalParameters: ext,
				InternalParameters: in.InternalParameters, ResolvedDependencies: deps},
			RunDetails: RunDetails{Builder: Builder{ID: in.BuilderID, Version: in.BuilderVersion}},
		}}
	if in.InvocationID != "" || !in.StartedOn.IsZero() || !in.FinishedOn.IsZero() {
		md := &BuildMetadata{InvocationID: in.InvocationID}
		if !in.StartedOn.IsZero() {
			t := in.StartedOn.UTC()
			md.StartedOn = &t
		}
		if !in.FinishedOn.IsZero() {
			t := in.FinishedOn.UTC()
			md.FinishedOn = &t
		}
		st.Predicate.RunDetails.Metadata = md
	}
	return st, nil
}

// ---- DSSE --------------------------------------------------------------------

// Envelope is a DSSE (Dead Simple Signing Envelope) v1 envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one DSSE signature.
type Signature struct {
	KeyID string `json:"keyid,omitempty"`
	Sig   string `json:"sig"`
}

// PAE is the DSSE v1 Pre-Authentication Encoding:
// "DSSEv1" SP LEN(type) SP type SP LEN(body) SP body.
func PAE(payloadType string, payload []byte) []byte {
	b := make([]byte, 0, len(payload)+len(payloadType)+32)
	b = append(b, "DSSEv1 "...)
	b = strconv.AppendInt(b, int64(len(payloadType)), 10)
	b = append(b, ' ')
	b = append(b, payloadType...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(payload)), 10)
	b = append(b, ' ')
	b = append(b, payload...)
	return b
}

// KeyID returns the hex SHA-256 of an ed25519 public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// SignEnvelope signs payload with ed25519 over its PAE.
func SignEnvelope(payloadType string, payload []byte, key ed25519.PrivateKey) (*Envelope, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("dsse: invalid ed25519 private key")
	}
	sig := ed25519.Sign(key, PAE(payloadType, payload))
	pub := key.Public().(ed25519.PublicKey)
	return &Envelope{PayloadType: payloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(sig)}}}, nil
}

// AddSignature appends another signer's signature to the envelope.
func (e *Envelope) AddSignature(key ed25519.PrivateKey) error {
	payload, err := e.DecodePayload()
	if err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("dsse: invalid ed25519 private key")
	}
	sig := ed25519.Sign(key, PAE(e.PayloadType, payload))
	e.Signatures = append(e.Signatures, Signature{KeyID: KeyID(key.Public().(ed25519.PublicKey)),
		Sig: base64.StdEncoding.EncodeToString(sig)})
	return nil
}

// DecodePayload returns the raw payload (standard or URL-safe base64).
func (e *Envelope) DecodePayload() ([]byte, error) {
	if len(e.Payload) > maxEnvelopePayloadLen*4/3+4 {
		return nil, errors.New("dsse: payload too large")
	}
	return decodeB64(e.Payload)
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("dsse: invalid base64")
}

// VerifyEnvelope checks that at least threshold distinct trusted keys
// produced valid signatures and returns the verified payload. Signatures
// with a keyid are only checked against the key with that id.
func VerifyEnvelope(e *Envelope, trusted []ed25519.PublicKey, threshold int) ([]byte, error) {
	if e == nil {
		return nil, errors.New("dsse: nil envelope")
	}
	if threshold < 1 {
		threshold = 1
	}
	payload, err := e.DecodePayload()
	if err != nil {
		return nil, err
	}
	msg := PAE(e.PayloadType, payload)
	verified := map[string]bool{}
	for _, s := range e.Signatures {
		sig, err := decodeB64(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		for _, k := range trusted {
			if len(k) != ed25519.PublicKeySize {
				continue
			}
			kid := KeyID(k)
			if s.KeyID != "" && subtle.ConstantTimeCompare([]byte(s.KeyID), []byte(kid)) != 1 {
				continue
			}
			if ed25519.Verify(k, msg, sig) {
				verified[kid] = true
			}
		}
	}
	if len(verified) < threshold {
		return nil, fmt.Errorf("dsse: %d of %d required signatures verified", len(verified), threshold)
	}
	return payload, nil
}

// SignProvenance serialises the statement and signs it as a DSSE envelope.
func SignProvenance(st *Statement, key ed25519.PrivateKey) (*Envelope, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	return SignEnvelope(InTotoPayloadType, payload, key)
}

// VerifyProvenance verifies the envelope signature(s), checks the payload
// and statement types, and — when artifacts is non-empty — that every
// artifact's SHA-256 matches a subject of the same name.
func VerifyProvenance(e *Envelope, trusted []ed25519.PublicKey, artifacts map[string][]byte) (*Statement, error) {
	if e == nil {
		return nil, errors.New("slsa: nil envelope")
	}
	if e.PayloadType != InTotoPayloadType {
		return nil, fmt.Errorf("slsa: unexpected payload type %q", e.PayloadType)
	}
	payload, err := VerifyEnvelope(e, trusted, 1)
	if err != nil {
		return nil, err
	}
	var st Statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, fmt.Errorf("slsa: decode statement: %w", err)
	}
	if st.Type != InTotoStatementType {
		return nil, fmt.Errorf("slsa: unexpected statement type %q", st.Type)
	}
	if st.PredicateType != SLSAProvenanceType {
		return nil, fmt.Errorf("slsa: unexpected predicate type %q", st.PredicateType)
	}
	for name, data := range artifacts {
		sum := sha256.Sum256(data)
		want := hex.EncodeToString(sum[:])
		ok := false
		for _, s := range st.Subject {
			if s.Name == name && subtle.ConstantTimeCompare([]byte(s.Digest["sha256"]), []byte(want)) == 1 {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("slsa: artifact %q does not match any subject digest", name)
		}
	}
	return &st, nil
}
