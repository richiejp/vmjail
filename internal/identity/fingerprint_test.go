package identity

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFingerprintCertHashesSPKI(t *testing.T) {
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := sha256.Sum256(id.Cert.RawSubjectPublicKeyInfo)
	got := FingerprintCert(id.Cert)
	if got != want {
		t.Errorf("FingerprintCert mismatch")
	}
	if id.Fingerprint() != want {
		t.Errorf("Identity.Fingerprint mismatch")
	}
}

func TestFingerprintStringIsCanonicalForm(t *testing.T) {
	var fp Fingerprint
	for i := range fp {
		fp[i] = byte(i)
	}
	got := fp.String()
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("String missing prefix: %q", got)
	}
	// Strip prefix and verify it's 32 hex pairs separated by colons.
	body := strings.TrimPrefix(got, "sha256:")
	if strings.Count(body, ":") != 31 {
		t.Errorf("expected 31 colons in body, got %d: %q", strings.Count(body, ":"), body)
	}
	for part := range strings.SplitSeq(body, ":") {
		if len(part) != 2 {
			t.Errorf("non-2-char hex pair %q in %q", part, got)
		}
	}
	if body != strings.ToUpper(body) {
		t.Errorf("hex body should be uppercase, got %q", body)
	}
}

func TestParseFingerprintCanonicalRoundTrip(t *testing.T) {
	var fp Fingerprint
	for i := range fp {
		fp[i] = byte(i + 1)
	}
	got, err := ParseFingerprint(fp.String())
	if err != nil {
		t.Fatalf("ParseFingerprint: %v", err)
	}
	if got != fp {
		t.Errorf("round trip mismatch")
	}
}

func TestParseFingerprintAcceptsBareHex(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	if _, err := ParseFingerprint(hex); err != nil {
		t.Errorf("bare hex rejected: %v", err)
	}
	if _, err := ParseFingerprint(strings.ToUpper(hex)); err != nil {
		t.Errorf("upper hex rejected: %v", err)
	}
}

func TestParseFingerprintAcceptsSpacePadding(t *testing.T) {
	hex := "sha256:" + strings.Repeat("AB:", 31) + "AB"
	if _, err := ParseFingerprint("  " + hex + "\n"); err != nil {
		t.Errorf("padded form rejected: %v", err)
	}
}

func TestParseFingerprintAcceptsUpperPrefix(t *testing.T) {
	hex := "SHA256:" + strings.Repeat("ab", 32)
	if _, err := ParseFingerprint(hex); err != nil {
		t.Errorf("uppercase prefix rejected: %v", err)
	}
}

func TestParseFingerprintRejectsWrongLength(t *testing.T) {
	cases := []string{"", "sha256:", "ab", strings.Repeat("a", 63), strings.Repeat("a", 65)}
	for _, c := range cases {
		_, err := ParseFingerprint(c)
		if !errors.Is(err, ErrInvalidFingerprint) {
			t.Errorf("ParseFingerprint(%q) = %v, want ErrInvalidFingerprint", c, err)
		}
	}
}

func TestParseFingerprintRejectsBadHex(t *testing.T) {
	bad := strings.Repeat("zz", 32)
	_, err := ParseFingerprint(bad)
	if !errors.Is(err, ErrInvalidFingerprint) {
		t.Errorf("ParseFingerprint(%q) = %v, want ErrInvalidFingerprint", bad, err)
	}
}

func TestFingerprintJSONRoundTrip(t *testing.T) {
	type wrapper struct {
		FP Fingerprint `json:"fp"`
	}
	in := wrapper{}
	for i := range in.FP {
		in.FP[i] = byte(i) ^ 0xa5
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out wrapper
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.FP != in.FP {
		t.Errorf("round trip mismatch")
	}
}

func TestFingerprintJSONRejectsBadValue(t *testing.T) {
	var fp Fingerprint
	if err := json.Unmarshal([]byte(`"not-a-fingerprint"`), &fp); err == nil {
		t.Fatal("expected error for malformed fingerprint")
	}
}

func FuzzParseFingerprint(f *testing.F) {
	f.Add("sha256:" + strings.Repeat("AB:", 31) + "AB")
	f.Add(strings.Repeat("ab", 32))
	f.Add("garbage")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		// Only contract: must not panic.
		if fp, err := ParseFingerprint(s); err == nil {
			// Round-trip survivors must reparse to the same value.
			again, err := ParseFingerprint(fp.String())
			if err != nil || again != fp {
				t.Fatalf("round trip failed: in=%q parsed=%v err=%v again=%v",
					s, fp, err, again)
			}
		}
	})
}
