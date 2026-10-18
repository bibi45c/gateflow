package password

import (
	"strings"
	"testing"
)

func TestHashChecksOnlyItsPassword(t *testing.T) {
	stored, err := Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "pbkdf2-sha256$") || strings.Contains(stored, "s3cret") {
		t.Fatalf("stored form %q", stored)
	}
	for password, want := range map[string]bool{"s3cret": true, "s3cret ": false, "": false, "S3cret": false} {
		got, err := Check(stored, password)
		if err != nil || got != want {
			t.Errorf("Check(%q) = %v, %v; want %v", password, got, err, want)
		}
	}
}

func TestHashUsesANewSalt(t *testing.T) {
	a, _ := Hash("same")
	b, _ := Hash("same")
	if a == b {
		t.Fatal("two hashes of one password are equal")
	}
}

func TestMalformedHashIsAnError(t *testing.T) {
	for _, stored := range []string{"", "plain", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$1$$", "md5$1$a$b"} {
		if ok, err := Check(stored, "x"); ok || err == nil {
			t.Errorf("Check(%q) = %v, %v; want an error", stored, ok, err)
		}
	}
}
