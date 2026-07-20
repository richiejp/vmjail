package secretvalue

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValueOnlyRevealsExplicitlyAndRedactsFormatting(t *testing.T) {
	original := []byte("subscription-token")
	value, err := New(original)
	if err != nil {
		t.Fatal(err)
	}
	original[0] = 'X'
	if err := value.WithBytes(func(got []byte) error {
		if string(got) != "subscription-token" {
			t.Fatalf("revealed = %q", got)
		}
		got[0] = 'X'
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := value.WithBytes(func(got []byte) error {
		if string(got) != "subscription-token" {
			t.Fatalf("callback mutated backing storage: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Secret Value `json:"secret"`
	}{value})
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{value.String(), fmt.Sprintf("%#v", value), string(data)} {
		if strings.Contains(rendered, "subscription-token") {
			t.Fatalf("secret leaked through %q", rendered)
		}
	}
}

func TestDestroyAffectsCopiesAndIsIdempotent(t *testing.T) {
	value, err := New([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	valueCopy := value
	value.Destroy()
	value.Destroy()
	if err := valueCopy.WithBytes(func([]byte) error { return nil }); err == nil {
		t.Fatal("destroyed copy remained readable")
	}
}

func TestValueRejectsInvalidUse(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := New(make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("oversized secret accepted")
	}
	var zero Value
	zero.Destroy()
	if err := zero.WithBytes(func([]byte) error { return nil }); err == nil {
		t.Fatal("zero value revealed")
	}
	value, _ := New([]byte("x"))
	if err := value.WithBytes(nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	text, err := value.MarshalText()
	if err != nil || string(text) != Redacted {
		t.Fatalf("MarshalText() = %q, %v", text, err)
	}
}
