// Package registry holds the small slice of the OCI distribution format that
// Ollama's model registry uses: manifests listing a config blob and layer
// blobs, each addressed by sha256 digest.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

const ManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Blobs returns config + layers.
func (m *Manifest) Blobs() []Descriptor {
	return append([]Descriptor{m.Config}, m.Layers...)
}

func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, b := range m.Blobs() {
		n += b.Size
	}
	return n
}

// Ref is "library/qwen2.5:7b-instruct-q4_K_M" split into name and tag.
type Ref struct {
	Name string // library/qwen2.5
	Tag  string // 7b-instruct-q4_K_M
}

func ParseRef(s string) (Ref, error) {
	name, tag, ok := strings.Cut(s, ":")
	if !ok || tag == "" {
		tag = "latest"
	}
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.ContainsAny(name+tag, `\ `) {
		return Ref{}, fmt.Errorf("registry: bad reference %q", s)
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return Ref{Name: name, Tag: tag}, nil
}

func (r Ref) String() string { return r.Name + ":" + r.Tag }

// LocalName is how the model is named inside Ollama on the node:
// "library/qwen2.5:7b" -> "qwen2.5:7b".
func (r Ref) LocalName() string {
	return strings.TrimPrefix(r.Name, "library/") + ":" + r.Tag
}

// ValidDigest accepts only "sha256:<64 hex>", so digests are safe as file names.
func ValidDigest(d string) bool {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

// BlobFile is the on-disk file name for a digest: "sha256-<hex>".
func BlobFile(digest string) string { return strings.Replace(digest, ":", "-", 1) }

// VerifyingWriter hashes everything written and checks it against a digest.
type VerifyingWriter struct {
	w      io.Writer
	h      hash.Hash
	want   string
	N      int64
	OnData func(n int)
}

func NewVerifyingWriter(w io.Writer, digest string) *VerifyingWriter {
	return &VerifyingWriter{w: w, h: sha256.New(), want: digest}
}

func (v *VerifyingWriter) Write(p []byte) (int, error) {
	n, err := v.w.Write(p)
	v.h.Write(p[:n])
	v.N += int64(n)
	if v.OnData != nil {
		v.OnData(n)
	}
	return n, err
}

var ErrDigestMismatch = errors.New("registry: content does not match digest")

func (v *VerifyingWriter) Verify() error {
	if "sha256:"+hex.EncodeToString(v.h.Sum(nil)) != v.want {
		return ErrDigestMismatch
	}
	return nil
}
