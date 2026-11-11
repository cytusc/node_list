package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/runtimeenv"
)

const converterVersion = "strict-v1/sub-store-2.44.2"
const maxInput = 32 << 20

type Converted struct {
	Index            int                `json:"index"`
	Fingerprint      string             `json:"fingerprint"`
	Canonical        string             `json:"canonical"`
	Config           domain.ProxyConfig `json:"config"`
	Name             string             `json:"name"`
	RawLink          string             `json:"rawLink"`
	GeneratedLink    string             `json:"generatedLink"`
	OriginalEvidence string             `json:"originalEvidence"`
	LinkStatus       string             `json:"linkStatus"`
	Conflict         bool               `json:"conflict"`
}
type Rejection struct {
	Index int    `json:"index"`
	Code  string `json:"code"`
}
type Conversion struct {
	Version  string      `json:"version"`
	Nodes    []Converted `json:"nodes"`
	Rejected []Rejection `json:"rejected"`
	Total    int         `json:"total"`
}

var conversionSlot = make(chan struct{}, 1)

type Converter struct{ Binary, Script string }

func DefaultConverter() Converter {
	return Converter{runtimeenv.Env("SOURCE_NODE_BIN", "node"), runtimeenv.Env("SOURCE_CONVERTER", filepath.Join(runtimeenv.Root(), "services", "node-source-converter", "converter.mjs"))}
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("converter output limit")
	}
	return b.Buffer.Write(p)
}
func (c Converter) Convert(ctx context.Context, request any) (Conversion, error) {
	var result Conversion
	input, err := json.Marshal(request)
	if err != nil || len(input) > maxInput {
		return result, errors.New("source input exceeds limit or is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	select {
	case conversionSlot <- struct{}{}:
		defer func() { <-conversionSlot }()
	case <-ctx.Done():
		return result, errors.New("source conversion queue timed out")
	}
	dir, err := os.MkdirTemp("", "node-source-convert-*")
	if err != nil {
		return result, errors.New("converter workspace unavailable")
	}
	defer os.RemoveAll(dir)
	script, err := filepath.Abs(c.Script)
	if err != nil {
		return result, errors.New("converter path invalid")
	}
	cmd := exec.CommandContext(ctx, c.Binary, "--max-old-space-size=192", script)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	out := &boundedBuffer{limit: 64 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	// The default inherited environment can include provider/DB credentials. The converter needs none.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "LANG=C.UTF-8"}
	cmd.WaitDelay = 2 * time.Second
	if err = cmd.Run(); err != nil {
		return result, errors.New("source conversion failed or timed out")
	}
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.Version != converterVersion || result.Total > 20000 || len(result.Nodes)+len(result.Rejected) != result.Total {
		return Conversion{}, errors.New("invalid converter response")
	}
	return result, nil
}
