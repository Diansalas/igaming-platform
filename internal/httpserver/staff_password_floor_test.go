package httpserver

import (
	"os"
	"strings"
	"testing"
)

// Security review SR-A10a-2: the invitation-accept floor counts characters (runes), not bytes.
func TestStaffPasswordLongEnough_CountsCharacters(t *testing.T) {
	if staffPasswordLongEnough("密码密码") { // 4 characters, 12 bytes
		t.Fatal("4 characters (12 bytes) must be refused")
	}
	if !staffPasswordLongEnough(strings.Repeat("密", 12)) {
		t.Fatal("12 characters must be accepted")
	}
	if staffPasswordLongEnough(strings.Repeat("a", 11)) || !staffPasswordLongEnough(strings.Repeat("a", 12)) {
		t.Fatal("ASCII boundary wrong")
	}
}

// The handler must use the helper, not a byte length.
func TestInvitationAcceptHandler_UsesCharacterFloor(t *testing.T) {
	src, err := os.ReadFile("staff_invitation_accept.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "!staffPasswordLongEnough(req.Password.reveal())") || strings.Contains(s, "len(req.Password.reveal())") {
		t.Fatal("invitation accept must use staffPasswordLongEnough (character count)")
	}
}
