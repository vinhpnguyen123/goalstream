package auth

import "testing"

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckPassword("correct horse battery", h); !ok || err != nil {
		t.Fatalf("right password: %v, %v", ok, err)
	}
	if ok, err := CheckPassword("correct horse batterY", h); ok || err != nil {
		t.Fatalf("wrong password: %v, %v", ok, err)
	}
	if h2, _ := HashPassword("correct horse battery"); h2 == h {
		t.Fatal("two hashes of one password are equal: no random salt")
	}
	// The dev user's '!' can never log in, and is not an error.
	if ok, err := CheckPassword("", NoPassword); ok || err != nil {
		t.Fatalf("NoPassword: %v, %v", ok, err)
	}
	if _, err := CheckPassword("x", "not-a-hash"); err == nil {
		t.Fatal("a corrupt hash must be an error, not a wrong password")
	}
}
