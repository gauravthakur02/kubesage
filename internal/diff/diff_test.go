package diff

import (
	"reflect"
	"testing"
)

func TestLines(t *testing.T) {
	a := []string{"a", "b", "c", "d", "e", "f", "g"}
	b := []string{"a", "b", "X", "d", "e", "f", "g", "h"}
	got := Lines(a, b, 1)
	want := []string{" b", "-c", "+X", " d", "@@", " g", "+h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if add, del := Stats(got); add != 2 || del != 1 {
		t.Fatalf("stats = %d %d", add, del)
	}
	if d := Lines(a, a, 3); len(d) != 0 {
		for _, l := range d {
			if l[0] == '+' || l[0] == '-' {
				t.Fatalf("identical inputs differ: %q", d)
			}
		}
	}
}
