package internal

import (
	"database/sql"
	"database/sql/driver"
	"testing"

	googlesql "github.com/goccy/go-googlesql"
)

// TestGetArgsFromParamsNilVsEmpty pins the distinction between a nil
// params slice (parameters were not collected, so every value passes
// through) and a non-nil empty one (the statement has no parameters, so
// nothing is bound).
func TestGetArgsFromParamsNilVsEmpty(t *testing.T) {
	values := []driver.NamedValue{
		{Name: "A", Ordinal: 1, Value: int64(1)},
		{Ordinal: 2, Value: int64(2)},
	}

	out, err := getArgsFromParams(values, nil)
	if err != nil {
		t.Fatalf("nil params: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("nil params: got %d args, want 2", len(out))
	}
	if got, want := out[0], (sql.NamedArg{Name: "a", Value: int64(1)}); got != want {
		t.Fatalf("nil params: out[0] = %#v, want %#v", got, want)
	}
	if got, want := out[1], any(int64(2)); got != want {
		t.Fatalf("nil params: out[1] = %#v, want %#v", got, want)
	}

	out, err = getArgsFromParams(values, []*googlesql.ResolvedParameter{})
	if err != nil {
		t.Fatalf("empty params: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty params: got %d args, want 0", len(out))
	}

	out, err = getArgsFromParams(nil, nil)
	if err != nil {
		t.Fatalf("nil values: %v", err)
	}
	if out != nil {
		t.Fatalf("nil values: got %#v, want nil", out)
	}
}

// TestEncodeOrPassArgsNilVsEmpty is the prepared statement counterpart
// of TestGetArgsFromParamsNilVsEmpty.
func TestEncodeOrPassArgsNilVsEmpty(t *testing.T) {
	out, err := encodeOrPassArgs([]any{int64(1)}, nil)
	if err != nil {
		t.Fatalf("nil params: %v", err)
	}
	if len(out) != 1 || out[0] != any(int64(1)) {
		t.Fatalf("nil params: got %#v, want [1]", out)
	}

	out, err = encodeOrPassArgs([]any{int64(1)}, []*googlesql.ResolvedParameter{})
	if err != nil {
		t.Fatalf("empty params: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty params: got %#v, want no args", out)
	}
}
