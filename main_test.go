package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestSplitGlobalFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.String("context", "", "")
	fs.Bool("readonly", false, "")
	g, r := splitGlobalFlags(fs, []string{"add", "claude", "--provider", "anthropic", "--config", "c.yaml", "--readonly", "--model=x", "--context=a,b"})
	if !reflect.DeepEqual(g, []string{"--config", "c.yaml", "--readonly", "--context=a,b"}) {
		t.Fatalf("globals = %v", g)
	}
	if !reflect.DeepEqual(r, []string{"add", "claude", "--provider", "anthropic", "--model=x"}) {
		t.Fatalf("rest = %v", r)
	}
	_, r = splitGlobalFlags(fs, []string{"why", "--", "--config", "is", "odd"})
	if !reflect.DeepEqual(r, []string{"why", "--config", "is", "odd"}) {
		t.Fatalf("-- should stop flag parsing: %v", r)
	}
}
