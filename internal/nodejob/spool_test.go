package nodejob

import (
	"errors"
	"testing"
)

func TestSpoolLargeWriteKeepsTail(t *testing.T) {
	s := newSpool(4)
	if n, err := s.Write([]byte("012345")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	c, err := s.read(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "2345" || !c.CursorExpired || c.NextCursor != 6 {
		t.Fatalf("chunk = %+v", c)
	}
}

func TestSpoolIncrementalOverflowAndReads(t *testing.T) {
	s := newSpool(5)
	_, _ = s.Write([]byte("abc"))
	_, _ = s.Write([]byte("defg"))
	c, err := s.read(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "de" || c.CursorExpired || c.NextCursor != 5 {
		t.Fatalf("chunk = %+v", c)
	}
	c, err = s.read(5, 100)
	if err != nil || string(c.Data) != "fg" || c.NextCursor != 7 {
		t.Fatalf("tail = %+v, %v", c, err)
	}
	if _, err := s.read(8, 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("ahead error = %v", err)
	}
}
