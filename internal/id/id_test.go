package id

import (
	"testing"
	"time"
)

func TestNewIsValidAndPrefixed(t *testing.T) {
	v := New(Fork)
	if !Valid(v, Fork) {
		t.Fatalf("New(%q) produced invalid id %q", Fork, v)
	}
	if got := Prefix(v); got != Fork {
		t.Fatalf("Prefix(%q) = %q, want %q", v, got, Fork)
	}
	if Valid(v, Task) {
		t.Fatalf("fork id %q validated as a task id", v)
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 10_000)
	for range 10_000 {
		v := New(Task)
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate id generated: %q", v)
		}
		seen[v] = struct{}{}
	}
}

func TestNewSortsByCreationTime(t *testing.T) {
	first := New(Repo)
	time.Sleep(2 * time.Millisecond)
	second := New(Repo)
	if first >= second {
		t.Fatalf("ids are not time-ordered: %q >= %q", first, second)
	}
}

func TestTimeRoundTrips(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	got := Time(New(Event))
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("Time() = %v, want within [%v, %v]", got, before, after)
	}
	if !Time("not-an-id").IsZero() {
		t.Fatal("Time() of a malformed id should be the zero time")
	}
}

func TestValidRejectsMalformed(t *testing.T) {
	for _, v := range []string{"", "fork", "fork_", "fork_short", "task_0004k2m9x8q3v7bntp2wz1cgae"} {
		if Valid(v, Fork) {
			t.Errorf("Valid(%q, %q) = true, want false", v, Fork)
		}
	}
}
