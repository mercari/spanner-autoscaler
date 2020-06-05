package pointer

import (
	"testing"
	"time"
)

func TestInt32(t *testing.T) {
	i := int32(1)
	if *Int32(i) != i {
		t.Fatal("invalid value")
	}
}

func TestDuration(t *testing.T) {
	d := time.Minute
	if *Duration(d) != d {
		t.Fatal("invalid value")
	}
}

func TestString(t *testing.T) {
	s := "s"
	if *String(s) != s {
		t.Fatal("invalid value")
	}
}
