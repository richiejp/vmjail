package assert_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/assert"
)

// recoverPanic returns the panic value if fn panics, or nil otherwise.
// It exists so test cases stay readable as table entries instead of
// each one open-coding the defer+recover dance.
func recoverPanic(fn func()) (val any) {
	defer func() { val = recover() }()
	fn()
	return nil
}

func TestTrueNoPanicOnTrue(t *testing.T) {
	//daft:lint:disable=assertpure reason="literal true is the path under test"
	got := recoverPanic(func() { assert.True(true, "should not panic") })
	if got != nil {
		t.Fatalf("True(true) panicked: %v", got)
	}
}

func TestTruePanicsOnFalse(t *testing.T) {
	got := recoverPanic(func() { assert.True(false, "boom %d", 42) })
	if got == nil {
		t.Fatalf("True(false) did not panic")
	}
	msg, ok := got.(string)
	if !ok {
		t.Fatalf("panic value type = %T, want string", got)
	}
	if !strings.HasPrefix(msg, "assert: ") {
		t.Errorf("panic message %q lacks assert: prefix", msg)
	}
	if !strings.Contains(msg, "boom 42") {
		t.Errorf("panic message %q missing formatted args", msg)
	}
}

func TestNoErrNoPanicOnNil(t *testing.T) {
	//daft:lint:disable=assertpure reason="literal nil is the path under test"
	got := recoverPanic(func() { assert.NoErr(nil, "should not panic") })
	if got != nil {
		t.Fatalf("NoErr(nil) panicked: %v", got)
	}
}

func TestNoErrPanicsOnError(t *testing.T) {
	err := errors.New("underlying problem")
	got := recoverPanic(func() { assert.NoErr(err, "context %s", "foo") })
	if got == nil {
		t.Fatalf("NoErr(non-nil) did not panic")
	}
	msg, ok := got.(string)
	if !ok {
		t.Fatalf("panic value type = %T, want string", got)
	}
	if !strings.HasPrefix(msg, "assert: ") {
		t.Errorf("panic message %q lacks assert: prefix", msg)
	}
	if !strings.Contains(msg, "context foo") {
		t.Errorf("panic message %q missing formatted args", msg)
	}
	if !strings.Contains(msg, err.Error()) {
		t.Errorf("panic message %q does not contain underlying err %q", msg, err.Error())
	}
}
