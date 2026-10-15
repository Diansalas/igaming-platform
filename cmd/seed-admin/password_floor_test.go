package main

import (
	"strings"
	"testing"
)

// Security review SR-A10a-2: the floor counts characters, not bytes.
func TestPasswordLongEnough_CountsCharacters(t *testing.T) {
	if passwordLongEnough("密码密码") { // 4 characters, 12 bytes
		t.Fatal("4 characters (12 bytes) must be refused")
	}
	if !passwordLongEnough(strings.Repeat("密", 12)) {
		t.Fatal("12 characters must be accepted")
	}
	if passwordLongEnough(strings.Repeat("a", 11)) || !passwordLongEnough(strings.Repeat("a", 12)) {
		t.Fatal("ASCII boundary wrong")
	}
}
