package smtp

import (
	"bufio"
	"strings"
	"testing"
)

func TestConsumeData_DotUnstuffAndCount(t *testing.T) {
	input := "hello\r\n..dot-stuffed\r\n.\r\n"
	r := bufio.NewReader(strings.NewReader(input))

	payload, size, err := consumeData(r, 1024)
	if err != nil {
		t.Fatalf("consumeData returned error: %v", err)
	}

	got := string(payload)
	want := "hello\r\n.dot-stuffed\r\n"
	if got != want {
		t.Fatalf("unexpected payload\nwant: %q\ngot:  %q", want, got)
	}
	if int64(len(want)) != size {
		t.Fatalf("unexpected size: want=%d got=%d", len(want), size)
	}
}

func TestConsumeData_OverLimitReturnsNilPayload(t *testing.T) {
	input := "12345\r\n67890\r\n.\r\n"
	r := bufio.NewReader(strings.NewReader(input))

	payload, size, err := consumeData(r, 6)
	if err != nil {
		t.Fatalf("consumeData returned error: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload when over limit")
	}
	if size <= 6 {
		t.Fatalf("expected size to exceed max, got %d", size)
	}
}

func TestSplitCommand(t *testing.T) {
	verb, args := splitCommand("mail FROM:<user@example.com>")
	if verb != "MAIL" {
		t.Fatalf("unexpected verb: %q", verb)
	}
	if args != "FROM:<user@example.com>" {
		t.Fatalf("unexpected args: %q", args)
	}
}
