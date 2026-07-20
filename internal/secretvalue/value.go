// Package secretvalue provides an explicitly revealed, always-redacted secret.
package secretvalue

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

const (
	Redacted = "[REDACTED]"
	MaxBytes = 1 << 20
)

// Value keeps a private copy of launch-scoped secret bytes. Call Destroy as
// soon as delivery is acknowledged. Copies of Value share the same storage.
type Value struct {
	state *state
}

type state struct {
	mu        sync.Mutex
	bytes     []byte
	destroyed bool
}

func New(value []byte) (Value, error) {
	if len(value) == 0 || len(value) > MaxBytes {
		return Value{}, fmt.Errorf("secretvalue: value must contain 1..%d bytes", MaxBytes)
	}
	return Value{state: &state{bytes: append([]byte(nil), value...)}}, nil
}

func (v Value) WithBytes(use func([]byte) error) error {
	if use == nil {
		return errors.New("secretvalue: callback is required")
	}
	if v.state == nil {
		return errors.New("secretvalue: value is empty")
	}
	v.state.mu.Lock()
	defer v.state.mu.Unlock()
	if v.state.destroyed {
		return errors.New("secretvalue: value is destroyed")
	}
	temporary := append([]byte(nil), v.state.bytes...)
	defer zero(temporary)
	return use(temporary)
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func (v Value) Destroy() {
	if v.state == nil {
		return
	}
	v.state.mu.Lock()
	defer v.state.mu.Unlock()
	for i := range v.state.bytes {
		v.state.bytes[i] = 0
	}
	v.state.bytes = nil
	v.state.destroyed = true
}

func (Value) String() string   { return Redacted }
func (Value) GoString() string { return Redacted }

func (Value) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
